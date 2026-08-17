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
}

func (p *fakeProc) Read(b []byte) (int, error) {
	p.mu.Lock()
	if len(p.chunks) > 0 {
		c := p.chunks[0]
		p.chunks = p.chunks[1:]
		p.mu.Unlock()
		return copy(b, c), nil
	}
	exits, done := p.exits, p.done
	p.mu.Unlock()

	if exits {
		return 0, io.EOF
	}
	<-done
	return 0, io.EOF
}

func (p *fakeProc) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.written = append(p.written, b...)
	return len(b), nil
}

func (p *fakeProc) Terminate() error {
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
}

type fakeMonitor struct {
	name     string
	width    int
	height   int
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

func (f *fakeHypr) has(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.find(name) >= 0
}

type harness struct {
	backend   *Backend
	hypr      *fakeHypr
	procs     []*fakeProc
	switched  bool
	restored  bool
	states    []string
	credModes []string
	argvs     [][]string

	// childExits makes spawned children hit EOF once their script runs out.
	childExits bool
	mu         sync.Mutex
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
			exits := h.childExits
			h.mu.Unlock()
			p := &fakeProc{
				chunks: append([]string{}, chunks...),
				exits:  exits,
				done:   make(chan struct{}),
			}
			h.mu.Lock()
			h.procs = append(h.procs, p)
			h.argvs = append(h.argvs, argv)
			h.mu.Unlock()
			return p, nil
		},
		SwitchDisplay:  func() error { h.switched = true; return nil },
		RestoreDisplay: func() error { h.restored = true; return nil },
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

func TestMirrorUsesAMirroredOutputAndLeavesThePanelAlone(t *testing.T) {
	// The whole point: forcing the panel to 1080p cost a 240Hz display 60Hz.
	h := newHarness(t, readyOutput)

	if err := h.backend.Start(context.Background(), device("a", "TV"), session.ModeMirror); err != nil {
		t.Fatal(err)
	}

	if !h.hypr.has(hypr.OutputMirror) {
		t.Error("no mirrored output was created")
	}
	// Not merely "an output named mirror" -- it must actually track the panel,
	// or the receiver shows an empty desktop instead of the user's screen.
	if got := h.hypr.mirrors(hypr.OutputMirror); got != "eDP-2" {
		t.Errorf("mirror output mirrors %q, want the focused panel eDP-2", got)
	}
	if h.switched {
		t.Error("the panel was switched even though a virtual output worked")
	}
	if !h.sawState(session.Streaming) {
		t.Error("never reported streaming")
	}
}

func TestMirrorFallsBackToSwitchingOnlyWhenNoOutputCanBeMade(t *testing.T) {
	h := newHarness(t, readyOutput)
	h.hypr.failNew = true

	if err := h.backend.Start(context.Background(), device("a", "TV"), session.ModeMirror); err != nil {
		t.Fatal(err)
	}

	if !h.switched {
		t.Error("want the panel switch as a fallback rather than refusing to cast")
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
	// Extend is a SECOND desktop: mirroring the panel would make it a
	// duplicate, which is what mirror mode is for.
	if got := h.hypr.mirrors(hypr.OutputExtend); got != "" {
		t.Errorf("extend output mirrors %q, want an independent output", got)
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

	if !h.hypr.has(hypr.OutputMirror) || !h.hypr.has(hypr.OutputExtend) {
		t.Error("both sessions should own their own output")
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

	err := h.backend.Start(context.Background(), device("a", "TV"), session.ModeMirror)

	if err == nil {
		t.Fatal("want a failure when capture never starts")
	}
	if h.hypr.has(hypr.OutputMirror) {
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
	if err := h.backend.Start(ctx, device("a", "TV"), session.ModeMirror); err != nil {
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
	if h.hypr.has(hypr.OutputMirror) {
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
	if err := h.backend.Start(ctx, device("a", "TV"), session.ModeMirror); err != nil {
		t.Fatal(err)
	}
	if !h.hypr.has(hypr.OutputMirror) {
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
	if h.hypr.has(hypr.OutputMirror) {
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

func TestAFallbackPanelSwitchIsRestoredOnStop(t *testing.T) {
	// If we took the fallback and changed the user's panel, we own putting it
	// back. Leaving it at 1080p60 after the cast ends is worse than the cast.
	h := newHarness(t, readyOutput)
	h.hypr.failNew = true
	ctx := context.Background()
	if err := h.backend.Start(ctx, device("a", "TV"), session.ModeMirror); err != nil {
		t.Fatal(err)
	}

	if err := h.backend.Stop(ctx, device("a", "TV")); err != nil {
		t.Fatal(err)
	}

	if !h.restored {
		t.Error("the panel was switched for the cast and never switched back")
	}
}

func TestNoPanelSwitchMeansNoRestore(t *testing.T) {
	// Restoring a panel we never touched would fight whatever the user set
	// during the cast.
	h := newHarness(t, readyOutput)
	ctx := context.Background()
	if err := h.backend.Start(ctx, device("a", "TV"), session.ModeMirror); err != nil {
		t.Fatal(err)
	}

	if err := h.backend.Stop(ctx, device("a", "TV")); err != nil {
		t.Fatal(err)
	}

	if h.restored {
		t.Error("restored a panel that was never switched")
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
	if h.hypr.has(hypr.OutputMirror) {
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
