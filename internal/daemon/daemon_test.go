package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mrCode/castr/internal/backend/airplay"
	"github.com/mrCode/castr/internal/discovery"
	"github.com/mrCode/castr/internal/session"
)

// fakeBackend models a backend's OBSERVABLE behaviour: it reports state
// through the daemon's own callback, exactly as the real one does.
type fakeBackend struct {
	mu       sync.Mutex
	daemon   *Daemon
	startErr error
	stopErr  error
	states   []session.State // emitted, in order, during Start
	starts   []string        // "id:mode"
	stops    []string
	pins     []string
}

func (b *fakeBackend) Start(_ context.Context, device discovery.Device, mode string) error {
	b.mu.Lock()
	b.starts = append(b.starts, device.ID+":"+mode)
	states, err := b.states, b.startErr
	d := b.daemon
	b.mu.Unlock()

	for _, s := range states {
		d.OnState(device, s, "")
	}
	return err
}

func (b *fakeBackend) Stop(_ context.Context, device discovery.Device) error {
	b.mu.Lock()
	b.stops = append(b.stops, device.ID)
	err := b.stopErr
	d := b.daemon
	b.mu.Unlock()
	if err == nil {
		d.OnState(device, session.Idle, "")
	}
	return err
}

func (b *fakeBackend) SubmitPin(_ context.Context, device discovery.Device, pin string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pins = append(b.pins, device.ID+":"+pin)
	return nil
}

func (b *fakeBackend) startCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.starts)
}

func (b *fakeBackend) stopCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.stops)
}

type rig struct {
	daemon   *Daemon
	backend  *fakeBackend
	registry *Registry
	notified []session.State
	mu       sync.Mutex
}

func newRig(t *testing.T, devices ...discovery.Device) *rig {
	t.Helper()
	r := &rig{backend: &fakeBackend{states: []session.State{session.Connecting, session.Streaming}}}

	browse := func() ([]discovery.Device, error) { return devices, nil }
	r.registry = NewRegistry(browse, time.Now)
	if err := r.registry.Refresh(); err != nil {
		t.Fatal(err)
	}

	r.daemon = New(r.registry, map[string]Backend{discovery.ProtocolAirPlay: r.backend})
	r.daemon.ListGrace = 20 * time.Millisecond
	r.daemon.StartGrace = 50 * time.Millisecond
	r.daemon.Notify = func(s session.State, _ discovery.Device, _ string) {
		r.mu.Lock()
		r.notified = append(r.notified, s)
		r.mu.Unlock()
	}
	r.backend.daemon = r.daemon
	return r
}

func (r *rig) state(id string) session.State {
	for _, s := range r.daemon.Sessions() {
		if s.Device.ID == id {
			return s.State
		}
	}
	return ""
}

func TestStartRegistersASessionCarryingItsMode(t *testing.T) {
	r := newRig(t, dev("a", "TV"))

	if err := r.daemon.Start(context.Background(), "a", session.ModeExtend); err != nil {
		t.Fatal(err)
	}

	sessions := r.daemon.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(sessions))
	}
	if sessions[0].Mode != session.ModeExtend {
		t.Errorf("mode = %q, want extend", sessions[0].Mode)
	}
	if sessions[0].State != session.Streaming {
		t.Errorf("state = %q, want streaming", sessions[0].State)
	}
}

func TestAnUnknownModeIsRejectedBeforeAnythingHappens(t *testing.T) {
	r := newRig(t, dev("a", "TV"))

	err := r.daemon.Start(context.Background(), "a", "clone")

	if err == nil {
		t.Fatal("want an error for an unknown mode")
	}
	if r.backend.startCount() != 0 {
		t.Error("the backend was called with an unknown mode")
	}
}

func TestARefusedStartPutsTheDisplacedSessionBack(t *testing.T) {
	// The backend declined without touching the device, so its existing cast
	// is still live. Dropping the record stranded it: the bar showed "not
	// casting" and no stop could reach it.
	r := newRig(t, dev("a", "TV"))
	ctx := context.Background()
	if err := r.daemon.Start(ctx, "a", session.ModeMirror); err != nil {
		t.Fatal(err)
	}

	r.backend.mu.Lock()
	r.backend.startErr = airplay.ErrRefused
	r.backend.states = nil
	r.backend.mu.Unlock()

	if err := r.daemon.Start(ctx, "a", session.ModeExtend); !errors.Is(err, airplay.ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}

	if got := r.state("a"); got != session.Streaming {
		t.Errorf("state = %q, want the original mirror session still streaming", got)
	}
	sessions := r.daemon.Sessions()
	if len(sessions) == 1 && sessions[0].Mode != session.ModeMirror {
		t.Errorf("mode = %q, want the ORIGINAL mode back, not the refused one", sessions[0].Mode)
	}
}

func TestAFailedRestartDoesNotClaimTheOldCastIsStillLive(t *testing.T) {
	// The opposite error: a failed restart tears the old cast down on its way
	// in, so restoring that record claims a cast that is gone -- a green
	// indicator on the bar for nothing.
	r := newRig(t, dev("a", "TV"))
	ctx := context.Background()
	if err := r.daemon.Start(ctx, "a", session.ModeMirror); err != nil {
		t.Fatal(err)
	}

	r.backend.mu.Lock()
	r.backend.startErr = errors.New("doubletake exited before mirroring started")
	r.backend.states = nil
	r.backend.mu.Unlock()

	if err := r.daemon.Start(ctx, "a", session.ModeMirror); err == nil {
		t.Fatal("want the start error")
	}

	if got := r.state("a"); got != "" {
		t.Errorf("state = %q, want no session at all after a failed restart", got)
	}
}

func TestAStartThatFailsLeavesNoSessionBlockingARetry(t *testing.T) {
	r := newRig(t, dev("a", "TV"))
	r.backend.mu.Lock()
	r.backend.startErr = errors.New("no route to host")
	r.backend.states = nil
	r.backend.mu.Unlock()

	_ = r.daemon.Start(context.Background(), "a", session.ModeMirror)

	if len(r.daemon.Sessions()) != 0 {
		t.Error("a never-transitioned session was left behind to block a retry")
	}
}

func TestStartWaitsForDiscoveryOnAColdDaemon(t *testing.T) {
	// `start <id>` against a freshly spawned daemon reported "device not
	// found" for a receiver that `list` would show a second later.
	release := make(chan struct{})
	var once sync.Once
	browse := func() ([]discovery.Device, error) {
		<-release
		return []discovery.Device{dev("a", "TV")}, nil
	}
	reg := NewRegistry(browse, time.Now)
	backend := &fakeBackend{states: []session.State{session.Connecting, session.Streaming}}
	d := New(reg, map[string]Backend{discovery.ProtocolAirPlay: backend})
	backend.daemon = d
	d.StartGrace = 5 * time.Second
	go func() { _ = reg.Refresh() }()
	go func() {
		time.Sleep(80 * time.Millisecond)
		once.Do(func() { close(release) })
	}()

	err := d.Start(context.Background(), "a", session.ModeMirror)

	if err != nil {
		t.Fatalf("a cold daemon refused a receiver that was about to appear: %v", err)
	}
}

func TestStartGivesUpEventuallyOnAGenuinelyAbsentDevice(t *testing.T) {
	r := newRig(t)

	err := r.daemon.Start(context.Background(), "ghost", session.ModeMirror)

	if !errors.Is(err, ErrDeviceNotFound) {
		t.Errorf("err = %v, want ErrDeviceNotFound", err)
	}
}

func TestStartWaitsLongerThanList(t *testing.T) {
	// `start` names a receiver, so the user has committed and waiting beats
	// failing. `list` is interactive and stays snappy.
	d := New(NewRegistry(func() ([]discovery.Device, error) { return nil, nil }, time.Now), nil)

	if d.StartGrace <= d.ListGrace {
		t.Errorf("start grace %v is not longer than list grace %v", d.StartGrace, d.ListGrace)
	}
	if d.StartGrace < 10*time.Second {
		t.Errorf("start grace = %v; a real Apple TV took ~8s to answer a cold browser", d.StartGrace)
	}
}

func TestStopRefusesASessionTheBackendHasNotAcknowledged(t *testing.T) {
	// Start registers the session before calling the backend. Stopping one
	// still in idle reaches a backend that does not recognise the device and
	// reports success for a cast that never began.
	r := newRig(t, dev("a", "TV"))
	r.backend.mu.Lock()
	r.backend.states = nil // the backend never emits
	r.backend.mu.Unlock()
	if err := r.daemon.Start(context.Background(), "a", session.ModeMirror); err != nil {
		t.Fatal(err)
	}

	err := r.daemon.Stop(context.Background(), "a")

	if !errors.Is(err, ErrNotCasting) {
		t.Errorf("err = %v, want ErrNotCasting", err)
	}
	if r.backend.stopCount() != 0 {
		t.Error("the backend was asked to stop a cast it never started")
	}
}

func TestAnIdleSessionIsNotReportedAsACast(t *testing.T) {
	// The bar widget has no idle branch and falls through to its streaming
	// return, so an idle session showed a green indicator offering a Stop
	// that Stop then refused.
	r := newRig(t, dev("a", "TV"))
	r.backend.mu.Lock()
	r.backend.states = nil
	r.backend.mu.Unlock()
	if err := r.daemon.Start(context.Background(), "a", session.ModeMirror); err != nil {
		t.Fatal(err)
	}

	if len(r.daemon.Sessions()) != 0 {
		t.Error("an unacknowledged session was reported as a live cast")
	}
}

func TestAFailedSessionIsClearedSoTheDeviceCanBeRetried(t *testing.T) {
	r := newRig(t, dev("a", "TV"))
	r.backend.mu.Lock()
	r.backend.states = []session.State{session.Connecting, session.Failed}
	r.backend.mu.Unlock()

	if err := r.daemon.Start(context.Background(), "a", session.ModeMirror); err != nil {
		t.Fatal(err)
	}

	if len(r.daemon.Sessions()) != 0 {
		t.Error("a failed session was left behind")
	}
}

func TestAPinIsOnlyAcceptedWhileTheReceiverIsAskingForOne(t *testing.T) {
	r := newRig(t, dev("a", "TV"))
	if err := r.daemon.Start(context.Background(), "a", session.ModeMirror); err != nil {
		t.Fatal(err)
	} // now streaming, not awaiting a pin

	err := r.daemon.SubmitPin(context.Background(), "a", "1234")

	if err == nil {
		t.Error("a PIN was accepted for a cast that is not asking for one")
	}
}

func TestAPinReachesTheBackendWhileAwaitingOne(t *testing.T) {
	r := newRig(t, dev("a", "TV"))
	r.backend.mu.Lock()
	r.backend.states = []session.State{session.Connecting, session.AwaitingPin}
	r.backend.mu.Unlock()
	if err := r.daemon.Start(context.Background(), "a", session.ModeMirror); err != nil {
		t.Fatal(err)
	}

	if err := r.daemon.SubmitPin(context.Background(), "a", "1234"); err != nil {
		t.Fatal(err)
	}

	r.backend.mu.Lock()
	defer r.backend.mu.Unlock()
	if len(r.backend.pins) != 1 || r.backend.pins[0] != "a:1234" {
		t.Errorf("backend received %v, want the PIN forwarded", r.backend.pins)
	}
}

func TestTheWatchdogDoesNotExitUnderALiveCast(t *testing.T) {
	// It fired mid-cast once: counting only "streaming" as active let it exit
	// while a cast was mid-handshake, or while the user read a PIN off the TV.
	r := newRig(t, dev("a", "TV"))
	r.backend.mu.Lock()
	r.backend.states = []session.State{session.Connecting, session.AwaitingPin}
	r.backend.mu.Unlock()
	if err := r.daemon.Start(context.Background(), "a", session.ModeMirror); err != nil {
		t.Fatal(err)
	}
	r.daemon.IdleTimeout = 20 * time.Millisecond

	go r.daemon.WatchIdle(5 * time.Millisecond)
	time.Sleep(200 * time.Millisecond)

	select {
	case <-r.daemon.Stopping():
		t.Error("the watchdog exited while a cast was waiting for its PIN")
	default:
	}
}

func TestTheWatchdogExitsWhenNothingIsCasting(t *testing.T) {
	r := newRig(t)
	r.daemon.IdleTimeout = 20 * time.Millisecond

	go r.daemon.WatchIdle(5 * time.Millisecond)

	select {
	case <-r.daemon.Stopping():
	case <-time.After(2 * time.Second):
		t.Error("an idle daemon never exited")
	}
}

func TestTheProductionIdleTimeoutKeepsDiscoveryWarm(t *testing.T) {
	// At 30s almost every command started a cold daemon, which is why `list`
	// came back empty for a network full of receivers.
	if IdleTimeout < 10*time.Minute {
		t.Errorf("IdleTimeout = %v; too short to keep the discovery cache useful", IdleTimeout)
	}
}

func TestShutdownIsSafeFromSeveralGoroutinesAtOnce(t *testing.T) {
	// A signal handler and the idle watchdog race for it.
	r := newRig(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r.daemon.Shutdown() }()
	}
	wg.Wait()

	select {
	case <-r.daemon.Stopping():
	default:
		t.Error("Shutdown did not stop the daemon")
	}
}
