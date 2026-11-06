package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mrCode/castr/internal/discovery"
	"github.com/mrCode/castr/internal/session"
	"github.com/mrCode/castr/internal/sound"
)

// IdleTimeout is how long the daemon stays resident with nothing casting.
//
// Discovery is the reason it is fifteen minutes rather than thirty seconds. A
// freshly started browser takes seconds to hear back from receivers, while
// avahi -- running since boot with a warm cache -- answers instantly. At the
// old 30s timeout almost every command started a cold daemon, which is why
// `list` came back empty and `start` said "device not found" for receivers
// plainly present. Staying resident costs a few MB and keeps discovery useful.
const IdleTimeout = 15 * time.Minute

// DiscoveryGrace bounds how long an interactive command waits for mDNS.
const DiscoveryGrace = 3 * time.Second

// StartDiscoveryGrace is longer because `start` names a specific receiver: the
// user has already committed, so waiting beats failing. `list` is interactive
// and stays snappy. Measured on a real network, an Apple TV took ~8s to answer
// a freshly started browser, so a 3s ceiling reported "device not found" for a
// receiver that was plainly there.
const StartDiscoveryGrace = 12 * time.Second

const discoveryPoll = 100 * time.Millisecond

// Backend is what the daemon needs from a protocol implementation.
type Backend interface {
	Start(ctx context.Context, device discovery.Device, mode string) error
	Stop(ctx context.Context, device discovery.Device) error
	SubmitPin(ctx context.Context, device discovery.Device, pin string) error
}

// Notifier shows the user a desktop banner.
type Notifier func(device discovery.Device, state session.State, reason string)

// Daemon owns discovery, the session records, and the backends.
type Daemon struct {
	Registry *Registry
	Backends map[string]Backend
	Notify   Notifier
	Clock    func() time.Time

	// Sound routes each cast's sound; nil means no routing. SoundOn is the
	// config's sound setting: with it off, Sound is never called.
	Sound   Sound
	SoundOn bool

	// Graces are fields rather than constants so tests do not sleep for
	// twelve seconds to prove a rule about twelve seconds.
	ListGrace   time.Duration
	StartGrace  time.Duration
	IdleTimeout time.Duration

	mu         sync.Mutex
	sessions   map[string]*session.Session
	lastActive time.Time
	// soundSources holds, by device id, the source Sound.Attach gave each
	// cast whose sound is routed. An entry is removed exactly once, by
	// whoever ends the cast, and that removal is what earns the Detach.
	soundSources map[string]string
	// soundNotes says why a cast with sound on has no choice.
	soundNotes map[string]string
	// starting holds, by device id, the session a Start in progress
	// registered. Until that Start returns, an end reported for it leaves the
	// sound alone: only Start knows whether the cast before it lives on.
	starting map[string]*session.Session
	stopping chan struct{}
	stopOnce sync.Once
}

// New returns a daemon with the production timings.
func New(reg *Registry, backends map[string]Backend) *Daemon {
	d := &Daemon{
		Registry:    reg,
		Backends:    backends,
		Clock:       time.Now,
		ListGrace:   DiscoveryGrace,
		StartGrace:  StartDiscoveryGrace,
		IdleTimeout: IdleTimeout,
		sessions:    map[string]*session.Session{},
		stopping:    make(chan struct{}),

		soundSources: map[string]string{},
		soundNotes:   map[string]string{},
		starting:     map[string]*session.Session{},
	}
	d.lastActive = d.Clock()
	return d
}

func (d *Daemon) now() time.Time {
	if d.Clock != nil {
		return d.Clock()
	}
	return time.Now()
}

// OnState records a backend's report and tells the user when it matters.
//
// This is the daemon's half of the backend contract: the backend knows what
// happened, the daemon knows whether anyone is watching.
func (d *Daemon) OnState(device discovery.Device, state session.State, reason string) {
	d.mu.Lock()
	s, ok := d.sessions[device.ID]
	if !ok {
		// A state for a device with no record: the session was already
		// resolved (a failure pops it) and this is a late straggler.
		d.mu.Unlock()
		return
	}
	if err := s.Transition(state, reason); err != nil {
		d.mu.Unlock()
		return // an impossible transition is a bug upstream, not news for the user
	}
	ended := state == session.Failed || state == session.Idle
	if ended {
		delete(d.sessions, device.ID)
	}
	// A refusing backend reports Failed before Start hears ErrRefused, and
	// the sound may belong to the cast the refused start displaced, which is
	// still running. Start decides once the backend returns.
	dropSound := ended && d.starting[device.ID] != s
	if s.IsActive() {
		d.lastActive = d.now()
	}
	d.mu.Unlock()

	if dropSound {
		d.dropSound(device.ID)
	}

	if d.Notify != nil {
		d.Notify(device, state, reason)
	}
}

// Sessions returns the sessions that have reached a state worth reporting.
//
// Idle is filtered out because Start registers a session BEFORE calling the
// backend, so an idle session is one whose backend has not emitted anything
// yet -- AirPlay's pre-start teardown can sit there for two seconds. Reporting
// it made the bar widget, which has no idle branch and falls through to its
// streaming return, show a green indicator offering a Stop that Stop refused.
func (d *Daemon) Sessions() []*session.Session {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*session.Session, 0, len(d.sessions))
	for _, s := range d.sessions {
		if s.State != session.Idle {
			out = append(out, s)
		}
	}
	return out
}

// List returns the known receivers, giving a cold daemon its moment first.
func (d *Daemon) List() []discovery.Device {
	d.Registry.Await(d.ListGrace, discoveryPoll)
	return d.Registry.Devices()
}

// ErrDeviceNotFound means no receiver with that id is known.
var ErrDeviceNotFound = errors.New("device not found")

// Start begins a cast.
func (d *Daemon) Start(ctx context.Context, deviceID, mode string) error {
	if !session.ValidMode(mode) {
		return fmt.Errorf("unknown mode: %s; expected one of %v", mode, session.Modes)
	}

	device, ok := d.Registry.Find(deviceID)
	if !ok {
		// A cold daemon has not heard from mDNS yet. Without this, starting by
		// id against a freshly spawned daemon reported "device not found" for
		// a receiver that `list` would show a second later.
		d.Registry.Await(d.StartGrace, discoveryPoll)
		device, ok = d.Registry.Find(deviceID)
	}
	if !ok {
		return fmt.Errorf("%w: %s", ErrDeviceNotFound, deviceID)
	}

	backend, ok := d.Backends[device.Protocol]
	if !ok {
		return fmt.Errorf("no backend for protocol: %s", device.Protocol)
	}

	// Register the session -- WITH ITS MODE -- before calling the backend,
	// rather than stashing the mode in shared daemon state. Start can suspend
	// before the backend's first emit (AirPlay awaits a teardown that can take
	// two seconds) and the daemon serves clients concurrently, so a second
	// in-flight start could otherwise overwrite a "pending mode" before the
	// first device's session was ever created from it.
	d.mu.Lock()
	previous := d.sessions[device.ID]
	s := session.New(device, mode, d.now)
	d.sessions[device.ID] = s
	d.starting[device.ID] = s
	d.lastActive = d.now()
	// A restart -- mirror to extend, say -- finds the receiver's sound
	// already routed, and keeps it: the router would refuse a second Attach.
	_, restart := d.soundSources[device.ID]
	previousNote, hadNote := d.soundNotes[device.ID]
	d.mu.Unlock()

	// Before the backend, which asks AudioSource for the source to capture.
	attached := false
	if d.routesSound() && !restart {
		attached = d.attachSound(device.ID)
	}

	err := backend.Start(ctx, device, mode)

	// From here an end reported for s drops its sound itself; one reported
	// while the backend ran shows as s missing from the sessions.
	d.mu.Lock()
	if d.starting[device.ID] == s {
		delete(d.starting, device.ID)
	}
	current, ok := d.sessions[device.ID]
	d.mu.Unlock()

	if err == nil {
		if !ok {
			// It ended before the backend returned.
			d.dropSound(device.ID)
			return nil
		}
		if restart && current == s {
			// Every cast starts on TV, a restarted one too. A failure leaves
			// the old choice, which status still reports truthfully.
			_ = d.Sound.Set(device.ID, sound.TV)
		}
		return nil
	}

	d.mu.Lock()
	drop := true
	if current, ok := d.sessions[device.ID]; ok && current != s {
		// A newer start owns the device now, and its sound with it.
		drop = false
	} else {
		// s is still listed, or the backend's own Failed report removed it.
		delete(d.sessions, device.ID)

		// The backend declined WITHOUT touching the device, so whatever it was
		// already doing, it still is: the record displaced above is still true
		// and has to go back. Dropping it stranded a live session -- the bar
		// showed "not casting" and no stop could reach it. Restoring on ANY
		// failure is wrong for the opposite reason: a failed restart tears the
		// old cast down on its way in, so putting that record back claims a
		// cast that is gone.
		if errors.Is(err, session.ErrRefused) && previous != nil {
			d.sessions[device.ID] = previous
			// The old cast's sound is still live too. Only routing this start
			// made for it goes, and the old cast's note comes back.
			drop = attached
		}
	}
	d.mu.Unlock()

	if drop {
		d.dropSound(device.ID)
	}
	if errors.Is(err, session.ErrRefused) && previous != nil && hadNote {
		d.mu.Lock()
		if d.sessions[device.ID] == previous {
			d.soundNotes[device.ID] = previousNote
		}
		d.mu.Unlock()
	}
	return err
}

// ErrNotCasting means there is nothing to stop.
var ErrNotCasting = errors.New("not casting to that device")

// Stop ends a cast.
func (d *Daemon) Stop(ctx context.Context, deviceID string) error {
	d.mu.Lock()
	s, ok := d.sessions[deviceID]
	// A session still in Idle was registered by Start but its backend has not
	// emitted anything yet. Calling stop on it reaches a backend that does not
	// recognise the device, which reports success for a cast that never began.
	if ok && s.State == session.Idle {
		ok = false
	}
	d.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotCasting, deviceID)
	}

	backend, ok := d.Backends[s.Device.Protocol]
	if !ok {
		return fmt.Errorf("no backend for protocol: %s", s.Device.Protocol)
	}

	err := backend.Stop(ctx, s.Device)
	if err != nil && errors.Is(err, session.ErrNoSession) {
		// The BACKEND is the authority on whether a cast exists. If it has no
		// record, ours is stale and keeping it strands the device forever:
		// status lists a cast that cannot be stopped, the bar shows it
		// connecting, and every later stop repeats the same refusal. Observed
		// on real hardware after a stop whose teardown failed.
		d.mu.Lock()
		stale := false
		if current, ok := d.sessions[deviceID]; ok && current == s {
			delete(d.sessions, deviceID)
			stale = true
		}
		d.mu.Unlock()
		if stale {
			d.dropSound(deviceID)
		}
		return nil
	}
	return err
}

// SubmitPin forwards a pairing code.
func (d *Daemon) SubmitPin(ctx context.Context, deviceID, pin string) error {
	d.mu.Lock()
	s, ok := d.sessions[deviceID]
	d.mu.Unlock()
	if !ok || s.State != session.AwaitingPin {
		return fmt.Errorf("%w: %s is not waiting for a PIN", ErrNotCasting, deviceID)
	}

	backend, ok := d.Backends[s.Device.Protocol]
	if !ok {
		return fmt.Errorf("no backend for protocol: %s", s.Device.Protocol)
	}
	return backend.SubmitPin(ctx, s.Device, pin)
}

// Add records a manually configured receiver.
func (d *Daemon) Add(device discovery.Device) { d.Registry.Add(device) }

// Forget drops a manually configured receiver.
func (d *Daemon) Forget(id string) bool { return d.Registry.Forget(id) }

// Shutdown asks the daemon to stop. Safe to call more than once, from any
// goroutine: a signal handler and the idle watchdog both race for it.
func (d *Daemon) Shutdown() {
	d.stopOnce.Do(func() { close(d.stopping) })
}

// Stopping is closed when the daemon should exit.
func (d *Daemon) Stopping() <-chan struct{} { return d.stopping }

// WatchIdle exits the daemon once nothing has been casting for IdleTimeout.
//
// "Active" is the session state machine's own notion, which covers connecting
// and awaiting-pin as well as streaming. Counting only streaming let the
// watchdog fire while a cast was mid-handshake, or while the user was reading
// a PIN off the television.
func (d *Daemon) WatchIdle(tick time.Duration) {
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	for {
		select {
		case <-d.stopping:
			return
		case <-ticker.C:
			d.mu.Lock()
			for _, s := range d.sessions {
				if s.IsActive() {
					d.lastActive = d.now()
					break
				}
			}
			idleFor := d.now().Sub(d.lastActive)
			d.mu.Unlock()

			if idleFor > d.IdleTimeout {
				d.Shutdown()
				return
			}
		}
	}
}

// WatchDiscovery re-browses until the daemon stops.
func (d *Daemon) WatchDiscovery(interval time.Duration) {
	_ = d.Registry.Refresh() // the first browse, immediately

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-d.stopping:
			return
		case <-ticker.C:
			_ = d.Registry.Refresh()
		}
	}
}

// Sound is what the daemon needs from the sound router.
type Sound interface {
	Attach(id string) (string, error)
	Set(id string, c sound.Choice) error
	Choice(id string) (sound.Choice, bool)
	Detach(id string) error
	Follow() error
	Restore() error
}

// The Sound calls below run pactl, so none is made holding d.mu: status
// would wait on them.

func (d *Daemon) routesSound() bool { return d.SoundOn && d.Sound != nil }

// attachSound routes a new cast's sound and reports whether it did. When it
// cannot, the cast goes ahead with its sound on the laptop and the TV, as
// before castr routed sound, and the note says why.
func (d *Daemon) attachSound(id string) bool {
	source, err := d.Sound.Attach(id)
	d.mu.Lock()
	defer d.mu.Unlock()
	if err != nil {
		d.soundNotes[id] = "sound stays on the laptop and the TV: " + firstLine(err)
		return false
	}
	d.soundSources[id] = source
	delete(d.soundNotes, id)
	return true
}

// dropSound ends a cast's sound routing. Every way a cast ends calls it, and
// often more than one does, so only the first call that finds the cast
// routed detaches it.
func (d *Daemon) dropSound(id string) {
	d.mu.Lock()
	_, routed := d.soundSources[id]
	delete(d.soundSources, id)
	delete(d.soundNotes, id)
	d.mu.Unlock()
	if routed {
		// Nothing to do about a failure here: the last detach restores the
		// laptop, and what it could not undo the next Recover does.
		_ = d.Sound.Detach(id)
	}
}

// AudioSource is the source a cast's receiver captures, or "" to leave the
// backend to its default: sound is off, or it could not be routed.
func (d *Daemon) AudioSource(id string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.soundSources[id]
}

// SetSound changes where one cast's sound plays.
func (d *Daemon) SetSound(id string, c sound.Choice) error {
	d.mu.Lock()
	_, casting := d.sessions[id]
	_, routed := d.soundSources[id]
	note := d.soundNotes[id]
	d.mu.Unlock()

	switch {
	case !casting:
		return fmt.Errorf("%w: %s", ErrNotCasting, id)
	case !d.routesSound():
		return errors.New("sound is off in castr's config")
	case !routed && note != "":
		return fmt.Errorf("no sound choice for %s: %s", id, note)
	case !routed:
		return fmt.Errorf("no sound choice for %s", id)
	}
	return d.Sound.Set(id, c)
}

// soundOf is a cast's sound choice, or else the note saying why it has none.
func (d *Daemon) soundOf(id string) (choice, note string) {
	d.mu.Lock()
	_, routed := d.soundSources[id]
	note = d.soundNotes[id]
	d.mu.Unlock()
	if !routed {
		return "", note
	}
	if c, ok := d.Sound.Choice(id); ok {
		return string(c), ""
	}
	return "", note
}

// FollowSound keeps the routing right while the user changes outputs. It
// does nothing while no cast's sound is routed.
//
// Only a lost routing changes what the casts report: the router has then
// restored the laptop and forgotten every cast, which Choice shows, so each
// cast it forgot loses its choice and gains a note. Any other failure leaves
// the routing in place, and the next call retries.
func (d *Daemon) FollowSound() {
	if !d.routesSound() {
		return
	}
	d.mu.Lock()
	ids := make([]string, 0, len(d.soundSources))
	for id := range d.soundSources {
		ids = append(ids, id)
	}
	d.mu.Unlock()
	if len(ids) == 0 {
		return
	}

	err := d.Sound.Follow()
	if err == nil {
		return
	}
	var lost []string
	for _, id := range ids {
		if _, ok := d.Sound.Choice(id); !ok {
			lost = append(lost, id)
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, id := range lost {
		// Gone already means its cast ended meanwhile and detached it.
		if _, ok := d.soundSources[id]; ok {
			delete(d.soundSources, id)
			d.soundNotes[id] = "sound is back on the laptop: " + firstLine(err)
		}
	}
}

// WatchSound calls FollowSound every tick until the daemon stops.
func (d *Daemon) WatchSound(tick time.Duration) {
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-d.stopping:
			return
		case <-ticker.C:
			d.FollowSound()
		}
	}
}

// firstLine keeps a note to one line: a joined error carries one per cause.
func firstLine(err error) string {
	msg, _, _ := strings.Cut(err.Error(), "\n")
	return msg
}
