package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/mrCode/castr/internal/backend/airplay"
	"github.com/mrCode/castr/internal/discovery"
	"github.com/mrCode/castr/internal/session"
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
type Notifier func(state session.State, device discovery.Device, reason string)

// Daemon owns discovery, the session records, and the backends.
type Daemon struct {
	Registry *Registry
	Backends map[string]Backend
	Notify   Notifier
	Clock    func() time.Time

	// Graces are fields rather than constants so tests do not sleep for
	// twelve seconds to prove a rule about twelve seconds.
	ListGrace   time.Duration
	StartGrace  time.Duration
	IdleTimeout time.Duration

	mu         sync.Mutex
	sessions   map[string]*session.Session
	lastActive time.Time
	stopping   chan struct{}
	stopOnce   sync.Once
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
	if state == session.Failed || state == session.Idle {
		delete(d.sessions, device.ID)
	}
	if s.IsActive() {
		d.lastActive = d.now()
	}
	d.mu.Unlock()

	if d.Notify != nil {
		d.Notify(state, device, reason)
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
	d.lastActive = d.now()
	d.mu.Unlock()

	err := backend.Start(ctx, device, mode)
	if err == nil {
		return nil
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sessions[device.ID] == s {
		delete(d.sessions, device.ID)

		// The backend declined WITHOUT touching the device, so whatever it was
		// already doing, it still is: the record displaced above is still true
		// and has to go back. Dropping it stranded a live session -- the bar
		// showed "not casting" and no stop could reach it. Restoring on ANY
		// failure is wrong for the opposite reason: a failed restart tears the
		// old cast down on its way in, so putting that record back claims a
		// cast that is gone.
		if errors.Is(err, airplay.ErrRefused) && previous != nil {
			d.sessions[device.ID] = previous
		}
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
	return backend.Stop(ctx, s.Device)
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
