package airplay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/mrCode/castr/internal/discovery"
	"github.com/mrCode/castr/internal/hypr"
	"github.com/mrCode/castr/internal/session"
)

// Process is the part of a doubletake child the backend needs.
type Process interface {
	io.Reader // merged stdout+stderr
	io.Writer // stdin, for the pairing PIN

	// Terminate signals the process GROUP. Signalling only doubletake leaves
	// its GStreamer capture pipelines running, re-parented to init, still
	// holding a portal node and the GPU -- five accumulated during one bad
	// session before this was fixed.
	Terminate() error
	Wait() error
}

// Spawner starts a child. Injected: no test spawns a real process.
type Spawner func(ctx context.Context, argv []string) (Process, error)

// CredsFor returns the credentials path for a mode, or "" for doubletake's own.
type CredsFor func(mode string) (string, error)

// Emit reports a state change to the daemon.
type Emit func(device discovery.Device, state session.State, reason string)

// ErrRefused is session.ErrRefused. The daemon matches on it to decide whether
// to put back the session record it displaced before calling us.
var ErrRefused = session.ErrRefused

// Backend supervises one doubletake child per session.
type Backend struct {
	Config       Config
	Hypr         hypr.Runner
	Spawn        Spawner
	Creds        CredsFor
	Emit         Emit
	ReadyTimeout time.Duration

	mu       sync.Mutex
	sessions map[string]*castSession
}

type castSession struct {
	device discovery.Device
	mode   string
	proc   Process
	scan   *Scanner

	// What this session set up, so teardown undoes exactly what it caused and
	// never a sibling session's. Mirror sets up nothing; only extend has an
	// output to own.
	virtual string

	// The startup outcome and the child's exit race each other. Both sides
	// record their half under the backend mutex, and whichever finds the other
	// already done owns the crash announcement -- so a child that dies one
	// millisecond after the ready marker is neither announced twice nor lost.
	settled    bool
	streaming  bool
	exitedFlag bool
	crashed    bool
	stopping   bool

	ready  chan struct{}
	pin    chan struct{}
	exited chan struct{}
	once   sync.Once
}

func (b *Backend) init() {
	if b.sessions == nil {
		b.sessions = map[string]*castSession{}
	}
	if b.ReadyTimeout == 0 {
		// Measured on real hardware: capture began 23s after session-ready,
		// and extend adds a portal round-trip on top. 30s was too tight.
		b.ReadyTimeout = 60 * time.Second
	}
}

// Start begins a cast. It blocks until the receiver is streaming, needs a PIN,
// or fails.
func (b *Backend) Start(ctx context.Context, device discovery.Device, mode string) error {
	b.mu.Lock()
	b.init()

	// The guard runs BEFORE any teardown. It used to run after, so refusing a
	// request first tore down that device's working cast and then declined --
	// the user lost a live mirror and the error mentioned a different device.
	if mode == session.ModeExtend {
		if other := b.activeExtend(); other != nil && other.device.ID != device.ID {
			b.mu.Unlock()
			msg := fmt.Sprintf("already extending to %s; stop it first", other.device.Name)
			b.emit(device, session.Failed, msg)
			return fmt.Errorf("%w: %s", ErrRefused, msg)
		}
	}
	b.mu.Unlock()

	if err := b.Stop(ctx, device); err != nil && !errors.Is(err, errNoSession) {
		return err
	}

	b.emit(device, session.Connecting, "")

	cs := &castSession{
		device: device, mode: mode, scan: &Scanner{},
		ready: make(chan struct{}), pin: make(chan struct{}), exited: make(chan struct{}),
	}

	if err := b.prepareOutput(cs); err != nil {
		b.emit(device, session.Failed, err.Error())
		return err
	}

	credsPath, err := b.Creds(mode)
	if err != nil {
		b.undo(cs)
		b.emit(device, session.Failed, err.Error())
		return err
	}

	proc, err := b.Spawn(ctx, BuildArgv(b.Config, device.Address, credsPath))
	if err != nil {
		b.undo(cs)
		msg := fmt.Sprintf("%s failed to start: %v", Binary, err)
		b.emit(device, session.Failed, msg)
		return errors.New(msg)
	}
	cs.proc = proc

	b.mu.Lock()
	b.init()
	b.sessions[device.ID] = cs
	b.mu.Unlock()

	go b.pump(cs)

	return b.awaitReady(ctx, cs)
}

// prepareOutput gives the encoder a 1920x1080 source without touching the panel.
func (b *Backend) prepareOutput(cs *castSession) error {
	b.mu.Lock()
	inUse := b.outputsInUse()
	b.mu.Unlock()

	if cs.mode == session.ModeExtend {
		_, _ = hypr.CleanupStrays(b.Hypr, inUse)
		name, err := hypr.CreateOutput(b.Hypr, hypr.OutputExtend, "")
		if err != nil {
			return fmt.Errorf("could not create a virtual display for extend mode: %w", err)
		}
		cs.virtual = name
		return nil
	}

	// MIRROR CREATES NOTHING, and that is the finding of an afternoon at a
	// real Apple TV rather than a simplification.
	//
	// castr used to create a virtual output that mirrored the panel, to hand
	// the encoder a 1080p source while the panel kept its own mode. The output
	// was created correctly and mirrored correctly -- and the screen-share
	// portal never offered it. A mirrored output is not an ACTIVE monitor on
	// Hyprland (it is absent from `hyprctl monitors`, which is why this package
	// reads `monitors all`), and the portal enumerates active monitors only.
	// The picker listed one source, the panel, every time.
	//
	// So the output was a phantom monitor created on every cast that nothing
	// could ever capture. What actually happened -- doubletake capturing the
	// panel and scaling it down itself -- worked, looked right on the
	// television, and left the panel at its own 240Hz. That is now what castr
	// asks for, honestly, instead of building something unreachable and
	// getting the same result by accident.
	//
	// EXTEND is different and keeps its output: an independent headless output
	// IS an active monitor, and the picker does offer it. Verified in the same
	// session, in the same dialog.
	_, _ = hypr.CleanupStrays(b.Hypr, inUse)
	return nil
}

// awaitReady waits for capture to start, a PIN prompt, or failure.
func (b *Backend) awaitReady(ctx context.Context, cs *castSession) error {
	timer := time.NewTimer(b.ReadyTimeout)
	defer timer.Stop()

	select {
	case <-cs.ready:
		b.settle(cs, true)
		b.emit(cs.device, session.Streaming, "")
		return nil

	case <-cs.pin:
		b.emit(cs.device, session.AwaitingPin, "")
		return nil

	case <-cs.exited:
		// The child may have printed the ready marker and only then exited --
		// and both channels are closed, so select would pick between them at
		// random. Capture did start; the pump owns what happens next.
		if cs.scan.Ready() {
			b.emit(cs.device, session.Streaming, "")
			b.settle(cs, true)
			return nil
		}
		return b.fail(ctx, cs, b.failureMessage(cs, true))

	case <-timer.C:
		return b.fail(ctx, cs, b.failureMessage(cs, false))

	case <-ctx.Done():
		return b.fail(ctx, cs, ctx.Err().Error())
	}
}

// timeoutMessage explains a stall without asserting a cause we did not check.
//
// doubletake usually says exactly what went wrong and castr used to discard it
// and guess. The guess blamed the firewall three times in one session -- once
// when the receiver was on another subnet with zero firewall drops, and twice
// when the child had already printed the real reason.
func (b *Backend) failureMessage(cs *castSession, exited bool) string {
	// Checked before anything else, and on BOTH paths: whether the child
	// timed out or exited, if it told us why then that is the answer.
	if failure := cs.scan.PortalFailure(); failure != "" {
		return fmt.Sprintf(
			"%s: screen capture never started because the screen-share prompt "+
				"was not answered. Pick the castr output in the dialog. %s said: %s",
			cs.device.Name, Binary, failure)
	}
	if exited {
		return fmt.Sprintf("%s exited before mirroring started: %s",
			Binary, cs.scan.Tail(300))
	}
	// Ordered by how often each is actually the cause, which is NOT the order
	// that sounds most technical. An unanswered screen-share prompt has now
	// caused this timeout more times than everything else combined -- and the
	// old wording, which led with ports and firewalls, sent the reader after
	// the network every time. It was wrong on three separate occasions.
	return fmt.Sprintf(
		"%s never started mirroring within %.0fs, and %s said nothing about why.\n"+
			"  1. A screen-share prompt may be waiting for an answer. Look for a "+
			"\"Select what to share\" window and pick the castr output.\n"+
			"  2. If no prompt ever appears, the screen-share picker itself may be "+
			"broken -- check custom_picker_binary in ~/.config/hypr/xdph.conf.\n"+
			"  3. Only then suspect the network: the receiver connects BACK to this "+
			"machine on %s.",
		cs.device.Name, b.ReadyTimeout.Seconds(), Binary, b.Config.PortRange)
}

// settle records the startup outcome exactly once.
func (b *Backend) settle(cs *castSession, streaming bool) {
	b.mu.Lock()
	cs.settled = true
	cs.streaming = streaming
	exited := cs.exitedFlag
	b.mu.Unlock()

	// The child was already gone before we got here: nobody else will report
	// it, so this side owns the announcement.
	if streaming && exited {
		b.announceCrash(cs)
	}
}

func (b *Backend) fail(ctx context.Context, cs *castSession, msg string) error {
	b.settle(cs, false)

	// Torn down WITHOUT going through Stop, and the distinction is the whole
	// message. Stop announces idle, the daemon drops the session on it, and the
	// Failed that followed then arrived for a device with no session -- dropped
	// on the floor. A cast that timed out said nothing at all: no banner, no
	// log line, no reason. Observed the first time a real cast timed out after
	// the stopping/idle fix.
	b.mu.Lock()
	if b.sessions[cs.device.ID] == cs {
		delete(b.sessions, cs.device.ID)
	}
	cs.stopping = true
	b.mu.Unlock()

	b.teardown(cs)
	b.emit(cs.device, session.Failed, msg)
	return errors.New(msg)
}

// teardown ends the child and undoes what the session set up, silently. The
// callers own what the user is told.
func (b *Backend) teardown(cs *castSession) error {
	if cs.proc != nil {
		_ = cs.proc.Terminate()
		_ = cs.proc.Wait()
	}
	return b.undo(cs)
}

// pump reads the child's output until it exits.
func (b *Backend) pump(cs *castSession) {
	buf := make([]byte, 4096)
	for {
		n, err := cs.proc.Read(buf)
		if n > 0 {
			cs.scan.Absorb(string(buf[:n]))
			if cs.scan.Ready() {
				cs.once.Do(func() { close(cs.ready) })
			}
			if cs.scan.NeedsPin() {
				select {
				case <-cs.pin:
				default:
					close(cs.pin)
				}
			}
		}
		if err != nil {
			break
		}
	}
	b.mu.Lock()
	cs.exitedFlag = true
	settled, streaming := cs.settled, cs.streaming
	b.mu.Unlock()
	close(cs.exited)

	// Only a crash AFTER streaming is announced here. Until the startup
	// outcome is settled, awaitReady owns it, so a startup failure cannot be
	// overwritten by a late emit from this goroutine -- two banners for one
	// event, with the useful one arriving first.
	if settled && streaming {
		b.announceCrash(cs)
		return
	}

	// A child that dies while waiting for a PIN is the exception: awaitReady
	// already RETURNED at the pin prompt, so nobody else owns the outcome.
	// Without this the session sits in awaiting_pin forever -- the bar shows a
	// cast that is connecting, `stop` finds nothing to stop, and submitting the
	// code answers "no active session" for a session the daemon is listing.
	//
	// Observed against a real Apple TV whose owner had turned pairing off, so
	// the code never appeared and doubletake gave up on its own.
	if !settled && cs.scan.NeedsPin() {
		b.failAfterPin(cs)
	}
}

// failAfterPin reports a child that died while the user was looking for a code.
func (b *Backend) failAfterPin(cs *castSession) {
	b.mu.Lock()
	if cs.stopping || cs.crashed {
		b.mu.Unlock()
		return
	}
	cs.crashed = true
	cs.settled = true
	delete(b.sessions, cs.device.ID)
	b.mu.Unlock()

	_ = b.undo(cs)

	msg := fmt.Sprintf(
		"%s stopped waiting for its pairing code. If the receiver never showed "+
			"one, check AirPlay access control on it. (%s)",
		cs.device.Name, cs.scan.Tail(160))
	b.emit(cs.device, session.Failed, msg)
}

// announceCrash reports a cast that died mid-stream and removes what it left.
func (b *Backend) announceCrash(cs *castSession) {
	b.mu.Lock()
	if cs.stopping || cs.crashed {
		b.mu.Unlock() // a deliberate stop, or already reported
		return
	}
	cs.crashed = true
	delete(b.sessions, cs.device.ID)
	b.mu.Unlock()

	_ = b.undo(cs)

	verb := "mirroring to"
	if cs.mode == session.ModeExtend {
		verb = "extending to"
	}
	b.emit(cs.device, session.Failed,
		fmt.Sprintf("%s %s stopped unexpectedly (%s)", verb, cs.device.Name, cs.scan.Tail(120)))
}

// errNoSession is session.ErrNoSession: the daemon needs the same value to
// tell "I never had this" from "stopping it failed".
var errNoSession = session.ErrNoSession

// Stop ends a cast and undoes exactly what it set up.
func (b *Backend) Stop(ctx context.Context, device discovery.Device) error {
	b.mu.Lock()
	b.init()
	cs, ok := b.sessions[device.ID]
	if ok {
		delete(b.sessions, device.ID)
	}
	b.mu.Unlock()

	if !ok {
		return errNoSession
	}

	cs.stopping = true

	// Announced BEFORE the teardown, and this is not cosmetic. The state
	// machine allows streaming -> stopping -> idle; emitting idle straight from
	// streaming is an invalid transition, which the daemon rejects, leaving the
	// session listed as streaming after a stop that actually succeeded. It took
	// a second stop to clear it, and only because the backend by then reported
	// no session at all.
	b.emit(device, session.Stopping, "")

	if err := b.teardown(cs); err != nil {
		// Reporting success while a virtual output remains is the failure
		// shape this project keeps producing; say so instead.
		//
		// The session goes BACK, and stopping is cleared. Leaving it removed
		// but flagged as stopping silenced every later report about it: the
		// daemon listed a cast the backend had forgotten, nothing could stop
		// it, and no crash would ever be announced. That dead end was reached
		// on real hardware.
		b.mu.Lock()
		cs.stopping = false
		b.sessions[device.ID] = cs
		b.mu.Unlock()
		return err
	}
	b.emit(device, session.Idle, "")
	return nil
}

// undo removes only what this session created.
func (b *Backend) undo(cs *castSession) error {
	var firstErr error
	if cs.virtual != "" {
		if err := hypr.RemoveOutput(b.Hypr, cs.virtual); err != nil {
			// KEPT, not forgotten. Clearing it on failure meant the retry had
			// nothing to remove and reported success while a 1080p monitor sat
			// on the desk -- exactly the "stop must not lie" rule, defeated
			// one line below where it is enforced.
			firstErr = err
		} else {
			cs.virtual = ""
		}
	}
	return firstErr
}

// SubmitPin forwards a pairing code to the waiting child.
func (b *Backend) SubmitPin(ctx context.Context, device discovery.Device, pin string) error {
	b.mu.Lock()
	cs, ok := b.sessions[device.ID]
	b.mu.Unlock()
	if !ok {
		return errNoSession
	}
	if _, err := cs.proc.Write([]byte(pin + "\n")); err != nil {
		return fmt.Errorf("sending PIN: %w", err)
	}
	return b.awaitReady(ctx, cs)
}

func (b *Backend) activeExtend() *castSession {
	for _, cs := range b.sessions {
		// Keyed on MODE, not on owning an output: once mirror owned one too,
		// an active mirror made every extend refuse with "already extending".
		if cs.mode == session.ModeExtend && cs.virtual != "" {
			return cs
		}
	}
	return nil
}

func (b *Backend) outputsInUse() []string {
	var names []string
	for _, cs := range b.sessions {
		if cs.virtual != "" {
			names = append(names, cs.virtual)
		}
	}
	return names
}

func (b *Backend) emit(device discovery.Device, state session.State, reason string) {
	if b.Emit != nil {
		b.Emit(device, state, reason)
	}
}
