package airplay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrCode/castr/internal/discovery"
	"github.com/mrCode/castr/internal/hypr"
	"github.com/mrCode/castr/internal/session"
)

// fakeProc feeds scripted output and records whether it was terminated.
//
// Once its script runs out it BLOCKS, the way a real doubletake sits there
// mirroring. Only a test that wants a dying child sets exits.
type fakeProc struct {
	mu         sync.Mutex
	chunks     []string
	exits      bool
	closed     bool
	done       chan struct{}
	terminated bool
	written    []byte

	// afterWrite is printed once something is written to stdin -- the way a
	// real helper starts capture only after it is given the code.
	afterWrite []string
	wake       chan struct{}
	// exitAfterWrite makes the child exit once afterWrite has been printed.
	exitAfterWrite bool
	// terminateDelay makes Terminate slow, the way a real helper takes a
	// moment to shut its capture pipelines down.
	terminateDelay time.Duration
	terminations   int
}

func (p *fakeProc) Read(b []byte) (int, error) {
	for {
		p.mu.Lock()
		if len(p.chunks) > 0 {
			c := p.chunks[0]
			p.chunks = p.chunks[1:]
			p.mu.Unlock()
			return copy(b, c), nil
		}
		exits, done, wake := p.exits, p.done, p.wake
		p.mu.Unlock()

		if exits {
			return 0, io.EOF
		}
		select {
		case <-done:
			return 0, io.EOF
		case <-wake: // a nil wake never fires
		}
	}
}

func (p *fakeProc) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.written = append(p.written, b...)
	if len(p.afterWrite) > 0 {
		p.chunks = append(p.chunks, p.afterWrite...)
		p.afterWrite = nil
		if p.exitAfterWrite {
			p.exits = true
		}
		select {
		case p.wake <- struct{}{}:
		default:
		}
	}
	return len(b), nil
}

func (p *fakeProc) Terminate() error {
	p.mu.Lock()
	delay := p.terminateDelay
	p.terminations++
	p.mu.Unlock()
	time.Sleep(delay)

	p.mu.Lock()
	defer p.mu.Unlock()
	p.terminated = true
	p.release()
	return nil
}

// die ends the child WITHOUT anyone having asked, the way a real doubletake
// dies when the receiver drops the connection. Caller must hold p.mu.
func (p *fakeProc) release() {
	if !p.closed {
		p.closed = true
		close(p.done)
	}
}

func (p *fakeProc) die() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.release()
}

func (p *fakeProc) Wait() error { return nil }

func (p *fakeProc) wasTerminated() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.terminated
}

// fakeHypr models Hyprland 0.56.2's actual rules, which are not the obvious
// ones: mutating commands answer "ok" or an error message and ALWAYS exit 0,
// monitors are configured through `eval hl.monitor{...}` rather than `keyword`,
// and a MIRRORING output cannot be removed until its mirror is cleared.
//
// The last rule is why this fake is worth its length: castr shipped a mirror
// output that mirrored nothing, and no call-recording stub would have noticed.
type fakeHypr struct {
	mu         sync.Mutex
	monitors   []fakeMonitor
	failNew    bool
	failRemove bool
	creates    int // how many times an output was asked for
}

type fakeMonitor struct {
	name     string
	width    int
	height   int
	refresh  float64
	focused  bool
	mirrorOf string
}

func (f *fakeHypr) run(name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	joined := strings.Join(args, " ")

	switch {
	case strings.HasPrefix(joined, "-j monitors"):
		var parts []string
		for i, m := range f.monitors {
			parts = append(parts, fmt.Sprintf(
				`{"id":%d,"name":%q,"width":%d,"height":%d,"focused":%v,"mirrorOf":%q}`,
				i, m.name, m.width, m.height, m.focused, m.mirrorOf))
		}
		return "[" + strings.Join(parts, ",") + "]", nil

	case strings.HasPrefix(joined, "output create headless"):
		f.creates++
		if f.failNew {
			return "could not create output", nil // exit 0, like the real thing
		}
		want := args[len(args)-1]
		if f.find(want) >= 0 {
			return "Name already taken", nil
		}
		f.monitors = append(f.monitors, fakeMonitor{name: want,
			width: 1920, height: 1080, mirrorOf: "none"})
		return "ok", nil

	case strings.HasPrefix(joined, "output remove"):
		if f.failRemove {
			return "output not found", nil
		}
		target := args[len(args)-1]
		i := f.find(target)
		if i < 0 {
			return "output not found", nil
		}
		if m := f.monitors[i]; m.mirrorOf != "" && m.mirrorOf != "none" {
			// Still mirroring: 0.56.2 refuses, and says so while exiting 0.
			return "output not found", nil
		}
		f.monitors = append(f.monitors[:i], f.monitors[i+1:]...)
		return "ok", nil

	case strings.HasPrefix(joined, "eval "):
		return f.eval(args[len(args)-1]), nil

	case strings.HasPrefix(joined, "keyword "):
		return "keyword can't work with non-legacy parsers. Use eval.", nil
	}
	return "ok", nil
}

func (f *fakeHypr) eval(code string) string {
	output, ok := luaField(code, "output")
	if !ok {
		return "ok"
	}
	i := f.find(output)
	if i < 0 {
		return "ok"
	}
	if mode, ok := luaField(code, "mode"); ok {
		m := &f.monitors[i]
		fmt.Sscanf(mode, "%dx%d@%f", &m.width, &m.height, &m.refresh)
	}
	if mirror, ok := luaField(code, "mirror"); ok {
		if mirror == hypr.MirrorNone {
			f.monitors[i].mirrorOf = "none"
		} else {
			f.monitors[i].mirrorOf = mirror
		}
	}
	return "ok"
}

func luaField(code, name string) (string, bool) {
	marker := name + ` = "`
	i := strings.Index(code, marker)
	if i < 0 {
		return "", false
	}
	rest := code[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}

func (f *fakeHypr) find(name string) int {
	for i, m := range f.monitors {
		if m.name == name {
			return i
		}
	}
	return -1
}

// mirrors reports what the named output mirrors, "" if it is independent.
func (f *fakeHypr) mirrors(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.find(name)
	if i < 0 || f.monitors[i].mirrorOf == "none" {
		return ""
	}
	return f.monitors[i].mirrorOf
}

// mode reports the named output's mode as "WxH@R", "" if it does not exist.
func (f *fakeHypr) mode(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.find(name)
	if i < 0 {
		return ""
	}
	m := f.monitors[i]
	return fmt.Sprintf("%dx%d@%g", m.width, m.height, m.refresh)
}

// created reports how many outputs the compositor was asked to create.
func (f *fakeHypr) created() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates
}

func (f *fakeHypr) has(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.find(name) >= 0
}

type harness struct {
	backend *Backend
	hypr    *fakeHypr
	procs   []*fakeProc
	// switched stays: no test may ever see it become true, because nothing in
	// castr switches a panel now.
	switched  bool
	states    []string
	credModes []string
	argvs     [][]string

	// childExits makes spawned children hit EOF once their script runs out.
	childExits bool
	// afterPin is what spawned children print once given a code.
	afterPin []string
	// afterPinExits makes them exit after printing it.
	afterPinExits bool
	// terminateDelay slows every child's Terminate.
	terminateDelay time.Duration
	mu             sync.Mutex
}

func newHarness(t *testing.T, chunks ...string) *harness {
	t.Helper()
	h := &harness{hypr: &fakeHypr{monitors: []fakeMonitor{
		{name: "eDP-2", width: 2560, height: 1600, focused: true, mirrorOf: "none"}}}}

	h.backend = &Backend{
		Config:       config(),
		Hypr:         h.hypr.run,
		ReadyTimeout: 200 * time.Millisecond,
		Creds: func(mode string) (string, error) {
			h.mu.Lock()
			h.credModes = append(h.credModes, mode)
			h.mu.Unlock()
			return "/state/" + mode + ".json", nil
		},
		Spawn: func(_ context.Context, argv []string) (Process, error) {
			h.mu.Lock()
			exits, afterPin := h.childExits, h.afterPin
			afterPinExits, delay := h.afterPinExits, h.terminateDelay
			h.mu.Unlock()
			p := &fakeProc{
				chunks:     append([]string{}, chunks...),
				exits:      exits,
				done:       make(chan struct{}),
				afterWrite: append([]string{}, afterPin...),
				wake:       make(chan struct{}, 1),

				exitAfterWrite: afterPinExits,
				terminateDelay: delay,
			}
			h.mu.Lock()
			h.procs = append(h.procs, p)
			h.argvs = append(h.argvs, argv)
			h.mu.Unlock()
			return p, nil
		},
		Emit: func(d discovery.Device, s session.State, reason string) {
			h.mu.Lock()
			h.states = append(h.states, string(s))
			h.mu.Unlock()
		},
	}
	return h
}

func (h *harness) sawState(want session.State) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.states {
		if s == string(want) {
			return true
		}
	}
	return false
}

func device(id, name string) discovery.Device {
	return discovery.Device{ID: id, Name: name, Address: "10.0.0.5", Port: 7000,
		Protocol: discovery.ProtocolAirPlay}
}

const readyOutput = "mirror session ready\nscreen capture started\n"

func TestMirrorCreatesNothingAndLeavesThePanelAlone(t *testing.T) {
	// Two rules in one, and both were learned the hard way.
	//
	// The panel must keep its own mode: forcing it to 1080p cost a 240Hz
	// display 60Hz and the user typed on it for a whole cast.
	//
	// And mirror creates NO virtual output. It used to create one that
	// mirrored the panel, which the screen-share portal never offered --
	// mirrored outputs are not active monitors, so nothing could ever capture
	// it. It was a phantom monitor on every cast.
	h := newHarness(t, readyOutput)

	if err := h.backend.Start(context.Background(), device("a", "TV"), session.ModeMirror); err != nil {
		t.Fatal(err)
	}

	if h.hypr.has(hypr.OutputMirror) || h.hypr.has(hypr.OutputExtend) {
		t.Error("mirror created a virtual output that no portal can offer")
	}
	if h.switched {
		t.Error("the panel was switched; mirror must never touch it")
	}
	if !h.sawState(session.Streaming) {
		t.Error("never reported streaming")
	}
}

func TestExtendCreatesItsOwnIndependentOutput(t *testing.T) {
	h := newHarness(t, readyOutput)

	if err := h.backend.Start(context.Background(), device("a", "TV"), session.ModeExtend); err != nil {
		t.Fatal(err)
	}

	if !h.hypr.has(hypr.OutputExtend) {
		t.Error("no extend output was created")
	}
	if h.hypr.has(hypr.OutputMirror) {
		t.Error("extend must not create the mirror output")
	}
	// INDEPENDENT, not mirrored -- and this is what makes it usable at all.
	// A mirrored output never appears in the compositor's active monitor list,
	// so the screen-share portal cannot offer it; an independent one is a real
	// monitor and the picker lists it. Verified against the real picker.
	if got := h.hypr.mirrors(hypr.OutputExtend); got != "" {
		t.Errorf("extend output mirrors %q; the portal would never offer it", got)
	}
}

func TestASecondExtendIsRefusedWithoutTouchingTheFirst(t *testing.T) {
	h := newHarness(t, readyOutput)
	ctx := context.Background()
	if err := h.backend.Start(ctx, device("a", "First"), session.ModeExtend); err != nil {
		t.Fatal(err)
	}

	err := h.backend.Start(ctx, device("b", "Second"), session.ModeExtend)

	if !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if !h.hypr.has(hypr.OutputExtend) {
		t.Error("the refusal removed the first session's live output")
	}
}

func TestARefusalDoesNotTearDownTheRequestingDevicesCast(t *testing.T) {
	// The guard used to run AFTER teardown, so refusing a request first
	// destroyed that device's working cast and then declined.
	h := newHarness(t, readyOutput)
	ctx := context.Background()
	if err := h.backend.Start(ctx, device("a", "First"), session.ModeExtend); err != nil {
		t.Fatal(err)
	}
	if err := h.backend.Start(ctx, device("b", "Second"), session.ModeMirror); err != nil {
		t.Fatal(err)
	}

	// Device b is mirroring happily; asking it to extend must be refused
	// without killing what it is already doing.
	_ = h.backend.Start(ctx, device("b", "Second"), session.ModeExtend)

	h.mu.Lock()
	proc := h.procs[1]
	h.mu.Unlock()
	if proc.wasTerminated() {
		t.Error("a refused request tore down the device's existing cast")
	}
}

func TestMirrorAndExtendCoexist(t *testing.T) {
	h := newHarness(t, readyOutput)
	ctx := context.Background()

	if err := h.backend.Start(ctx, device("a", "One"), session.ModeMirror); err != nil {
		t.Fatal(err)
	}
	// An active mirror must not make extend refuse: the guard keys on mode,
	// not on merely owning a virtual output.
	if err := h.backend.Start(ctx, device("b", "Two"), session.ModeExtend); err != nil {
		t.Fatalf("extend refused while a mirror was active: %v", err)
	}

	if !h.hypr.has(hypr.OutputExtend) {
		t.Error("extend lost its output to the mirror session")
	}
	if h.hypr.has(hypr.OutputMirror) {
		t.Error("mirror created an output again")
	}
}

func TestStopRemovesOnlyItsOwnOutput(t *testing.T) {
	h := newHarness(t, readyOutput)
	ctx := context.Background()
	if err := h.backend.Start(ctx, device("a", "One"), session.ModeMirror); err != nil {
		t.Fatal(err)
	}
	if err := h.backend.Start(ctx, device("b", "Two"), session.ModeExtend); err != nil {
		t.Fatal(err)
	}

	if err := h.backend.Stop(ctx, device("a", "One")); err != nil {
		t.Fatal(err)
	}

	if h.hypr.has(hypr.OutputMirror) {
		t.Error("stop left its own output behind")
	}
	if !h.hypr.has(hypr.OutputExtend) {
		t.Error("stop removed the other session's live output")
	}
}

func TestStopTerminatesTheChild(t *testing.T) {
	h := newHarness(t, readyOutput)
	ctx := context.Background()
	if err := h.backend.Start(ctx, device("a", "TV"), session.ModeMirror); err != nil {
		t.Fatal(err)
	}

	if err := h.backend.Stop(ctx, device("a", "TV")); err != nil {
		t.Fatal(err)
	}

	h.mu.Lock()
	proc := h.procs[0]
	h.mu.Unlock()
	if !proc.wasTerminated() {
		t.Error("the child was not terminated")
	}
}

func TestSessionReadyAloneTimesOutAndCleansUp(t *testing.T) {
	// Four seconds of "mirror session ready" is not a stream. Timing out must
	// leave nothing behind.
	h := newHarness(t, "mirror session ready (data port: 49277)\n")

	err := h.backend.Start(context.Background(), device("a", "TV"), session.ModeExtend)

	if err == nil {
		t.Fatal("want a failure when capture never starts")
	}
	if h.hypr.has(hypr.OutputExtend) {
		t.Error("a timed-out session left its output behind")
	}
	if !h.sawState(session.Failed) {
		t.Error("never reported failure")
	}
}

func TestAPortalFailureIsReportedRatherThanGuessedAt(t *testing.T) {
	h := newHarness(t,
		"mirror session ready\n",
		"screen capture failed: screencast portal: timeout waiting for portal response\n")

	err := h.backend.Start(context.Background(), device("a", "TV"), session.ModeExtend)

	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "screen-share prompt") {
		t.Errorf("err = %q, want it to name the unanswered prompt", err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "firewall") {
		t.Errorf("err = %q, blamed the firewall when the child said otherwise", err)
	}
}

func TestAPinPromptIsReportedRatherThanTimingOut(t *testing.T) {
	h := newHarness(t, "pairing required. "+PinPrompt+": ")

	if err := h.backend.Start(context.Background(), device("a", "TV"), session.ModeMirror); err != nil {
		t.Fatal(err)
	}

	if !h.sawState(session.AwaitingPin) {
		t.Error("never reported awaiting_pin")
	}
}

func TestAChildThatDiesBeforeStreamingFails(t *testing.T) {
	h := newHarness(t) // no output at all, immediate EOF
	h.childExits = true

	err := h.backend.Start(context.Background(), device("a", "TV"), session.ModeMirror)

	if err == nil {
		t.Fatal("want an error when the child exits before streaming")
	}
	if h.hypr.has(hypr.OutputMirror) {
		t.Error("output left behind after an early exit")
	}
}

func TestStoppingAnUnknownDeviceIsNotSuccess(t *testing.T) {
	h := newHarness(t)

	err := h.backend.Stop(context.Background(), device("nope", "Nope"))

	if err == nil {
		t.Error("stop reported success for a session that never existed")
	}
}

func TestStopReportsFailureWhenTheOutputSurvives(t *testing.T) {
	// Reporting success while a virtual output remains is the failure shape
	// this project keeps producing: the bar says idle, the phantom monitor
	// stays, and windows keep migrating onto it.
	h := newHarness(t, readyOutput)
	ctx := context.Background()
	// Extend, because it is the mode that owns an output to fail to remove.
	if err := h.backend.Start(ctx, device("a", "TV"), session.ModeExtend); err != nil {
		t.Fatal(err)
	}
	h.hypr.failRemove = true

	err := h.backend.Stop(ctx, device("a", "TV"))

	if err == nil {
		t.Fatal("stop reported success while the output was still there")
	}
	if h.sawState(session.Idle) {
		t.Error("reported idle despite a failed teardown")
	}
}

func TestACrashBeforeStreamingDoesNotOverwriteTheStartupOutcome(t *testing.T) {
	// awaitReady owns the outcome until streaming is announced. A late emit
	// from the reader goroutine used to add a second, different failure --
	// two banners for one event, the useful one arriving first.
	h := newHarness(t, "connecting to receiver\n")
	h.childExits = true

	if err := h.backend.Start(context.Background(), device("a", "TV"), session.ModeMirror); err == nil {
		t.Fatal("want an error")
	}
	time.Sleep(50 * time.Millisecond) // give any stray emit a chance to land

	h.mu.Lock()
	defer h.mu.Unlock()
	failures := 0
	for _, s := range h.states {
		if s == string(session.Failed) {
			failures++
		}
	}
	if failures != 1 {
		t.Errorf("got %d failure reports, want exactly 1 (states: %v)", failures, h.states)
	}
}

func TestACrashAfterStreamingIsAnnouncedAndCleanedUp(t *testing.T) {
	h := newHarness(t, readyOutput)
	h.childExits = true
	ctx := context.Background()
	if err := h.backend.Start(ctx, device("a", "TV"), session.ModeMirror); err != nil {
		t.Fatal(err)
	}

	// The fake child hit EOF right after the ready marker, which is a crash
	// mid-stream: the user must be told, and nothing may be left behind.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !h.sawState(session.Failed) {
		time.Sleep(5 * time.Millisecond)
	}

	if !h.sawState(session.Failed) {
		t.Fatal("a crash mid-stream was never reported")
	}
	if h.hypr.has(hypr.OutputExtend) {
		t.Error("the crashed session left its output behind")
	}
}

func TestEachModeGetsItsOwnCredentials(t *testing.T) {
	// Both modes capture a different virtual output. Sharing one restore token
	// replays a token pointing at the other output -- or at the real panel.
	h := newHarness(t, readyOutput)
	ctx := context.Background()
	if err := h.backend.Start(ctx, device("a", "One"), session.ModeMirror); err != nil {
		t.Fatal(err)
	}
	if err := h.backend.Start(ctx, device("b", "Two"), session.ModeExtend); err != nil {
		t.Fatal(err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.credModes) != 2 || h.credModes[0] == h.credModes[1] {
		t.Fatalf("credential modes = %v, want one per mode", h.credModes)
	}
	first := strings.Join(h.argvs[0], " ")
	second := strings.Join(h.argvs[1], " ")
	if !strings.Contains(first, "-creds /state/"+session.ModeMirror) ||
		!strings.Contains(second, "-creds /state/"+session.ModeExtend) {
		t.Errorf("argv did not carry per-mode creds:\n%s\n%s", first, second)
	}
}

func TestASubmittedPinIsForwardedToTheChild(t *testing.T) {
	h := newHarness(t, "pairing required. "+PinPrompt+": ", readyOutput)
	ctx := context.Background()
	if err := h.backend.Start(ctx, device("a", "TV"), session.ModeMirror); err != nil {
		t.Fatal(err)
	}

	if err := h.backend.SubmitPin(ctx, device("a", "TV"), "1234"); err != nil {
		t.Fatal(err)
	}

	h.mu.Lock()
	got := string(h.procs[0].written)
	h.mu.Unlock()
	if got != "1234\n" {
		t.Errorf("child received %q, want the PIN and a newline", got)
	}
}

func TestACastThatDiesLongAfterStartingIsAnnouncedAndCleanedUp(t *testing.T) {
	// The other arm of the handoff: here the startup outcome is settled well
	// before the child dies, so the reader goroutine owns the announcement.
	// This is the common real failure -- the receiver drops the connection
	// minutes in, and the bar used to keep showing a green, live cast.
	h := newHarness(t, readyOutput)
	ctx := context.Background()
	// Extend: the mode with an output, so the cleanup is observable.
	if err := h.backend.Start(ctx, device("a", "TV"), session.ModeExtend); err != nil {
		t.Fatal(err)
	}
	if !h.hypr.has(hypr.OutputExtend) {
		t.Fatal("no output to begin with")
	}

	h.mu.Lock()
	proc := h.procs[0]
	h.mu.Unlock()
	proc.die()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !h.sawState(session.Failed) {
		time.Sleep(5 * time.Millisecond)
	}

	if !h.sawState(session.Failed) {
		t.Error("a cast that died mid-stream was never reported")
	}
	if h.hypr.has(hypr.OutputExtend) {
		t.Error("the dead session left its output behind")
	}
}

func TestADeliberateStopIsNotReportedAsACrash(t *testing.T) {
	// Stopping terminates the child, and the reader goroutine then sees
	// exactly what a crash looks like. Announcing it produced a red
	// "stopped unexpectedly" banner every single time the user stopped a
	// cast on purpose -- the notification flood that shipped once already.
	h := newHarness(t, readyOutput)
	ctx := context.Background()
	if err := h.backend.Start(ctx, device("a", "TV"), session.ModeMirror); err != nil {
		t.Fatal(err)
	}
	if err := h.backend.Stop(ctx, device("a", "TV")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // let any stray announcement land

	if h.sawState(session.Failed) {
		h.mu.Lock()
		defer h.mu.Unlock()
		t.Errorf("a deliberate stop was reported as a failure (states: %v)", h.states)
	}
}

func TestAChildThatDiesWaitingForAPinFailsTheSession(t *testing.T) {
	// awaitReady has already returned at the pin prompt, so nothing else owns
	// the outcome. Leaving it meant the bar showed a cast forever, `stop` found
	// nothing to stop, and submitting the code answered "no active session" for
	// a session the daemon was still listing.
	//
	// Observed against a real Apple TV with pairing turned off: no code ever
	// appeared and doubletake gave up on its own.
	h := newHarness(t, "pairing required. "+PinPrompt+": ")
	h.childExits = true

	if err := h.backend.Start(context.Background(), device("a", "TV"), session.ModeMirror); err != nil {
		t.Fatal(err)
	}
	if !h.sawState(session.AwaitingPin) {
		t.Fatal("never reported awaiting_pin")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !h.sawState(session.Failed) {
		time.Sleep(5 * time.Millisecond)
	}

	if !h.sawState(session.Failed) {
		t.Error("a session whose child died awaiting a PIN was never failed")
	}
	if h.hypr.has(hypr.OutputExtend) {
		t.Error("the dead session left its output behind")
	}
}

func TestTheFailureAfterAPinPromptSuggestsWhereToLook(t *testing.T) {
	// The user is standing at a television that showed nothing. "failed" alone
	// sends them to the network; the receiver's own settings are the cause.
	h := newHarness(t, "pairing required. "+PinPrompt+": ")
	h.childExits = true
	var reasons []string
	h.backend.Emit = func(_ discovery.Device, s session.State, reason string) {
		h.mu.Lock()
		h.states = append(h.states, string(s))
		if s == session.Failed {
			reasons = append(reasons, reason)
		}
		h.mu.Unlock()
	}

	if err := h.backend.Start(context.Background(), device("a", "TV"), session.ModeMirror); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !h.sawState(session.Failed) {
		time.Sleep(5 * time.Millisecond)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(reasons) == 0 {
		t.Fatal("no failure reason")
	}
	if !strings.Contains(reasons[0], "pairing code") {
		t.Errorf("reason = %q, want it to name the pairing code", reasons[0])
	}
}

func TestAFailedStopLeavesTheSessionStoppableAgain(t *testing.T) {
	// Removing the record but flagging it as stopping silenced everything
	// afterwards: the daemon listed a cast the backend had forgotten, nothing
	// could stop it, and no crash would ever be announced. A real dead end,
	// reached on real hardware.
	h := newHarness(t, readyOutput)
	ctx := context.Background()
	if err := h.backend.Start(ctx, device("a", "TV"), session.ModeExtend); err != nil {
		t.Fatal(err)
	}
	h.hypr.failRemove = true

	if err := h.backend.Stop(ctx, device("a", "TV")); err == nil {
		t.Fatal("a failed teardown reported success")
	}

	// The output can be removed now; the retry must work rather than answer
	// "no active session".
	h.hypr.failRemove = false
	if err := h.backend.Stop(ctx, device("a", "TV")); err != nil {
		t.Errorf("the retry failed: %v", err)
	}
	if h.hypr.has(hypr.OutputExtend) {
		t.Error("the output is still there after a successful retry")
	}
}

func TestTheTimeoutMessageLeadsWithTheScreenSharePrompt(t *testing.T) {
	// An unanswered prompt has caused this timeout more often than everything
	// else combined. Leading with ports and firewalls sent the reader after
	// the network -- wrong three times, and it cost hours.
	h := newHarness(t, "mirror session ready\n") // ready, but capture never starts

	err := h.backend.Start(context.Background(), device("a", "TV"), session.ModeMirror)

	if err == nil {
		t.Fatal("want a timeout error")
	}
	msg := err.Error()
	share := strings.Index(strings.ToLower(msg), "screen-share")
	ports := strings.Index(msg, h.backend.Config.PortRange)
	if share < 0 {
		t.Fatalf("message never mentions the screen-share prompt: %s", msg)
	}
	if ports >= 0 && ports < share {
		t.Errorf("message blames the network before the prompt:\n%s", msg)
	}
}

func TestStopWalksTheStatesTheMachineAllows(t *testing.T) {
	// streaming -> stopping -> idle. Jumping straight to idle is rejected by
	// the session state machine, and the daemon then keeps listing a cast that
	// has already been torn down.
	h := newHarness(t, readyOutput)
	ctx := context.Background()
	if err := h.backend.Start(ctx, device("a", "TV"), session.ModeMirror); err != nil {
		t.Fatal(err)
	}
	if err := h.backend.Stop(ctx, device("a", "TV")); err != nil {
		t.Fatal(err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	var stopping, idle = -1, -1
	for i, s := range h.states {
		if s == string(session.Stopping) && stopping < 0 {
			stopping = i
		}
		if s == string(session.Idle) && idle < 0 {
			idle = i
		}
	}
	if stopping < 0 {
		t.Fatalf("never reported stopping (states: %v)", h.states)
	}
	if idle < 0 || idle < stopping {
		t.Errorf("states = %v, want stopping before idle", h.states)
	}
}

func TestATimeoutTellsTheUserWhyRatherThanGoingQuiet(t *testing.T) {
	// It went quiet. The teardown ran through Stop, Stop announced idle, the
	// daemon dropped the session on it, and the Failed that followed arrived
	// for a device with no session -- discarded. A cast that timed out
	// produced no banner, no log line and no reason, which is the worst
	// possible outcome for the one path the user most needs explained.
	h := newHarness(t, "mirror session ready\n") // ready, capture never starts

	err := h.backend.Start(context.Background(), device("a", "TV"), session.ModeMirror)

	if err == nil {
		t.Fatal("want a timeout error")
	}
	if !h.sawState(session.Failed) {
		h.mu.Lock()
		defer h.mu.Unlock()
		t.Errorf("states = %v, want a failure the user can see", h.states)
	}
}

func TestAFailureIsNotAnnouncedAsACleanStop(t *testing.T) {
	// Reporting idle for a cast that failed would clear the bar and leave the
	// user believing they stopped it themselves.
	h := newHarness(t, "mirror session ready\n")

	_ = h.backend.Start(context.Background(), device("a", "TV"), session.ModeMirror)

	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.states {
		if s == string(session.Idle) {
			t.Errorf("states = %v, want no idle on a failure", h.states)
			return
		}
	}
}

// The port has to survive the trip from discovery to the spawned command, not
// just appear in BuildArgv's output: issue #2 was a port discovered, carried
// on the device, and dropped at exactly this hand-off.
func TestStartingACastDialsTheReceiversOwnPort(t *testing.T) {
	h := newHarness(t, readyOutput)
	redmi := device("redmi", "REDMI display")
	redmi.Port = 47000

	if err := h.backend.Start(context.Background(), redmi, session.ModeMirror); err != nil {
		t.Fatal(err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.argvs) == 0 {
		t.Fatal("doubletake was never started")
	}
	if got := strings.Join(h.argvs[0], " "); !strings.Contains(got, "-port 47000") {
		t.Errorf("doubletake was started without the receiver's port:\n%s", got)
	}
}

// The hook's answer has to reach the spawned command: the backend is what
// knows which receiver a cast is for.
func TestStartingACastCapturesItsOwnSound(t *testing.T) {
	h := newHarness(t, readyOutput)
	var asked string
	h.backend.AudioSource = func(deviceID string) string {
		asked = deviceID
		return "castr_cast_1.monitor"
	}

	if err := h.backend.Start(context.Background(), device("tv", "TV"), session.ModeMirror); err != nil {
		t.Fatal(err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if asked != "tv" {
		t.Errorf("the source was asked for %q, want the cast's device", asked)
	}
	if len(h.argvs) == 0 {
		t.Fatal("doubletake was never started")
	}
	if got := strings.Join(h.argvs[0], " "); !strings.Contains(got, "-audio-source castr_cast_1.monitor") {
		t.Errorf("doubletake was started without the cast's sound:\n%s", got)
	}
}

// A geometry caps mirror too -- through the helper, which scales the panel
// itself. Mirror still creates no output: the portal never offers one.
func TestGeometryCapsMirrorWithoutCreatingAnOutput(t *testing.T) {
	h := newHarness(t, readyOutput)
	h.backend.Config.MaxSize = "2560x1440"

	if err := h.backend.Start(context.Background(), device("tv", "TV"), session.ModeMirror); err != nil {
		t.Fatal(err)
	}

	if h.hypr.has(hypr.OutputMirror) || h.hypr.has(hypr.OutputExtend) {
		t.Error("mirror created a virtual output")
	}
	if n := h.hypr.created(); n != 0 {
		t.Errorf("the compositor was asked to create %d output(s) for mirror", n)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.argvs) == 0 {
		t.Fatal("the helper was never started")
	}
	if got := strings.Join(h.argvs[0], " "); !strings.Contains(got, "-max-size 2560x1440") {
		t.Errorf("the helper was started without the cap:\n%s", got)
	}
}

func TestExtendUsesTheConfiguredGeometry(t *testing.T) {
	h := newHarness(t, readyOutput)
	h.backend.Geometry = "2560x1440@60"

	if err := h.backend.Start(context.Background(), device("tv", "TV"), session.ModeExtend); err != nil {
		t.Fatal(err)
	}

	if got := h.hypr.mode(hypr.OutputExtend); got != "2560x1440@60" {
		t.Errorf("extend output is %q, want 2560x1440@60", got)
	}
}

func TestExtendWithNoGeometryIsTheSameSizeAsEver(t *testing.T) {
	h := newHarness(t, readyOutput)

	if err := h.backend.Start(context.Background(), device("tv", "TV"), session.ModeExtend); err != nil {
		t.Fatal(err)
	}

	if got := h.hypr.mode(hypr.OutputExtend); got != "1920x1080@60" {
		t.Errorf("extend output is %q, want 1920x1080@60", got)
	}
}

// pulls records the modes a backend asked to pull pairings into, in order,
// alongside the spawns, so a test can tell which came first.
type pulls struct {
	mu     sync.Mutex
	events []string
	err    error
}

func (p *pulls) hook(h *harness) {
	spawn := h.backend.Spawn
	h.backend.PullPairing = func(mode string) error {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.events = append(p.events, "pull "+mode)
		return p.err
	}
	h.backend.Spawn = func(ctx context.Context, argv []string) (Process, error) {
		p.mu.Lock()
		p.events = append(p.events, "spawn")
		p.mu.Unlock()
		return spawn(ctx, argv)
	}
}

func (p *pulls) got() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(p.events, ", ")
}

func TestACastPullsItsModesPairingsBeforeTheHelperStarts(t *testing.T) {
	// The helper reads its credentials file once, at start: a pairing mirror
	// saved a minute ago is only any use to extend if it is there by then.
	h := newHarness(t, readyOutput)
	p := &pulls{}
	p.hook(h)
	ctx := context.Background()

	if err := h.backend.Start(ctx, device("a", "One"), session.ModeMirror); err != nil {
		t.Fatal(err)
	}
	if err := h.backend.Start(ctx, device("a", "One"), session.ModeExtend); err != nil {
		t.Fatal(err)
	}

	if want := "pull mirror, spawn, pull extend, spawn"; p.got() != want {
		t.Errorf("events = %q, want %q", p.got(), want)
	}
}

func TestAModeAnotherCastIsUsingIsNotPulledInto(t *testing.T) {
	// A running helper rewrites its file from memory and may be mid-write, so
	// castr leaves a file in use alone. Replacing one device's own cast is not
	// "in use": that cast is stopped first.
	h := newHarness(t, readyOutput)
	p := &pulls{}
	p.hook(h)
	ctx := context.Background()

	for _, id := range []string{"a", "b", "b"} {
		if err := h.backend.Start(ctx, device(id, "TV "+id), session.ModeMirror); err != nil {
			t.Fatal(err)
		}
	}
	if want := "pull mirror, spawn, spawn, spawn"; p.got() != want {
		t.Errorf("with a's mirror running: events = %q, want %q", p.got(), want)
	}

	// With a stopped, b is the only mirror -- and replacing it stops it
	// first, so its file is free again.
	if err := h.backend.Stop(ctx, device("a", "TV a")); err != nil {
		t.Fatal(err)
	}
	if err := h.backend.Start(ctx, device("b", "TV b"), session.ModeMirror); err != nil {
		t.Fatal(err)
	}
	if want := "pull mirror, spawn, spawn, spawn, pull mirror, spawn"; p.got() != want {
		t.Errorf("replacing the only mirror: events = %q, want %q", p.got(), want)
	}
}

func TestTwoCastsStartingAtOnceInOneModePullOnlyOnce(t *testing.T) {
	// The second start must not pull while the first one's helper is already
	// running but not yet registered: that helper may be writing the file.
	h := newHarness(t, readyOutput)
	p := &pulls{}
	p.hook(h)
	ctx := context.Background()
	spawn := h.backend.Spawn
	var nested bool
	var inner error
	h.backend.Spawn = func(ctx context.Context, argv []string) (Process, error) {
		proc, err := spawn(ctx, argv)
		// Helper a is running; its session is not registered yet. Called
		// again from inside, for b, nested is already set.
		if !nested {
			nested = true
			inner = h.backend.Start(ctx, device("b", "Two"), session.ModeMirror)
		}
		return proc, err
	}

	if err := h.backend.Start(ctx, device("a", "One"), session.ModeMirror); err != nil {
		t.Fatal(err)
	}
	if inner != nil {
		t.Fatal(inner)
	}
	if want := "pull mirror, spawn, spawn"; p.got() != want {
		t.Errorf("events = %q, want %q", p.got(), want)
	}
}

func TestAFailedPullDoesNotStopTheCast(t *testing.T) {
	// The worst a missed pull costs is typing a code again; refusing to cast
	// would cost the cast.
	h := newHarness(t, readyOutput)
	p := &pulls{err: errors.New("disk full")}
	p.hook(h)

	if err := h.backend.Start(context.Background(), device("a", "One"), session.ModeExtend); err != nil {
		t.Fatalf("cast failed over a pull error: %v", err)
	}
	if !h.sawState(session.Streaming) {
		t.Error("the cast never reached streaming")
	}
}

// eventually polls cond for up to a second.
func eventually(cond func() bool) bool {
	for i := 0; i < 100; i++ {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

func (h *harness) count(want session.State) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, s := range h.states {
		if s == string(want) {
			n++
		}
	}
	return n
}

func TestACastThatTookItsCodeIsReportedStreamingOnceCaptureStarts(t *testing.T) {
	// Seen against a stand-in receiver: the code was taken and the receiver
	// got video for a minute, while castr reported awaiting_pin the whole
	// time. SubmitPin waited again, found the prompt it had already seen, and
	// stopped there; nothing else ever reported streaming.
	h := newHarness(t, "pairing required. "+PinPrompt+": ")
	h.afterPin = []string{readyOutput}
	ctx := context.Background()
	if err := h.backend.Start(ctx, device("a", "TV"), session.ModeMirror); err != nil {
		t.Fatal(err)
	}
	if !h.sawState(session.AwaitingPin) {
		t.Fatal("the cast never asked for its code")
	}

	if err := h.backend.SubmitPin(ctx, device("a", "TV"), "1234"); err != nil {
		t.Fatal(err)
	}

	if !eventually(func() bool { return h.sawState(session.Streaming) }) {
		t.Fatalf("states = %v, want streaming once capture started", h.states)
	}
	// A second code sent by an impatient user must not report it twice.
	_ = h.backend.SubmitPin(ctx, device("a", "TV"), "1234")
	time.Sleep(50 * time.Millisecond)
	if n := h.count(session.Streaming); n != 1 {
		t.Errorf("streaming reported %d times, want once", n)
	}
}

func TestACastThatTookItsCodeButNeverStartsFailsInTheEnd(t *testing.T) {
	// The same wait that reports streaming also gives up: otherwise a helper
	// stuck after the code would sit in awaiting_pin forever.
	h := newHarness(t, "pairing required. "+PinPrompt+": ")
	ctx := context.Background()
	if err := h.backend.Start(ctx, device("a", "TV"), session.ModeMirror); err != nil {
		t.Fatal(err)
	}
	if err := h.backend.SubmitPin(ctx, device("a", "TV"), "1234"); err != nil {
		t.Fatal(err)
	}

	if !eventually(func() bool { return h.sawState(session.Failed) }) {
		t.Fatalf("states = %v, want failed after the ready timeout", h.states)
	}
	if !h.procs[0].wasTerminated() {
		t.Error("the stuck helper was left running")
	}
}

func TestAHelperThatExitsAfterAChattyPairingFailsTheCast(t *testing.T) {
	// The scanner keeps only the last 16KB of output. A helper that logged
	// more than that after its code and then died left the prompt trimmed out
	// of view -- and nothing reported the exit: the cast sat in awaiting_pin,
	// and extend's output was left behind.
	h := newHarness(t, "pairing required. "+PinPrompt+": ")
	for i := 0; i < 2000; i++ { // 32KB, a line at a time
		h.afterPin = append(h.afterPin, "rtsp: SETUP ...\n")
	}
	h.afterPinExits = true
	ctx := context.Background()
	if err := h.backend.Start(ctx, device("a", "TV"), session.ModeExtend); err != nil {
		t.Fatal(err)
	}
	if err := h.backend.SubmitPin(ctx, device("a", "TV"), "1234"); err != nil {
		t.Fatal(err)
	}

	if !eventually(func() bool { return h.sawState(session.Failed) }) {
		t.Fatalf("states = %v, want failed once the helper exited", h.states)
	}
	if h.hypr.has(hypr.OutputExtend) {
		t.Error("the extend output was left behind")
	}
	if err := h.backend.Stop(ctx, device("a", "TV")); !errors.Is(err, errNoSession) {
		t.Errorf("Stop after the failure = %v, want no session left", err)
	}
}

func TestAStopDuringTheWaitAfterACodeIsNotAlsoAFailure(t *testing.T) {
	// The user sees nothing for a while and presses stop, and the ready
	// timeout fires while the helper is shutting down. That tore the cast
	// down twice and reported a failure after the stop -- after the NEXT
	// cast's connecting, when the stop was a restart.
	h := newHarness(t, "pairing required. "+PinPrompt+": ")
	h.terminateDelay = 150 * time.Millisecond
	ctx := context.Background()
	if err := h.backend.Start(ctx, device("a", "TV"), session.ModeExtend); err != nil {
		t.Fatal(err)
	}
	if err := h.backend.SubmitPin(ctx, device("a", "TV"), "1234"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(120 * time.Millisecond)
	if err := h.backend.Stop(ctx, device("a", "TV")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond) // past the ready timeout

	if h.sawState(session.Failed) {
		t.Errorf("states = %v: a deliberate stop was also reported as a failure", h.states)
	}
	h.procs[0].mu.Lock()
	n := h.procs[0].terminations
	h.procs[0].mu.Unlock()
	if n != 1 {
		t.Errorf("the helper was terminated %d times, want once", n)
	}
}
