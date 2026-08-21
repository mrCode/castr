package chromecast

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrCode/castr/internal/capture"
	"github.com/mrCode/castr/internal/discovery"
	"github.com/mrCode/castr/internal/session"
)

var tv = discovery.Device{
	ID: "chromecast:abc", Name: "Cinema", Address: "192.168.100.48",
	Port: 8009, Protocol: discovery.ProtocolChromecast,
}

type fakePortal struct{ closed bool }

func (f *fakePortal) Node() uint32         { return 76 }
func (f *fakePortal) Descriptor() *os.File { return os.Stdin }
func (f *fakePortal) Close() error         { f.closed = true; return nil }

type fakePipeline struct {
	mu        sync.Mutex
	done      chan struct{}
	terminate int
}

func newPipeline() *fakePipeline { return &fakePipeline{done: make(chan struct{})} }
func (f *fakePipeline) Pid() int { return 4242 }
func (f *fakePipeline) Wait() error {
	<-f.done
	return nil
}
func (f *fakePipeline) Terminate() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminate++
	select {
	case <-f.done:
	default:
		close(f.done)
	}
	return nil
}
func (f *fakePipeline) terminations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.terminate
}

type fakeServer struct{ closed bool }

func (f *fakeServer) URLFor(name string) string { return "http://192.168.100.8:8010/" + name }
func (f *fakeServer) Stats() (int64, int, int)  { return 0, 0, 0 }
func (f *fakeServer) Close() error              { f.closed = true; return nil }

type fakeCaster struct {
	mu         sync.Mutex
	loaded     string
	stopped    bool
	closed     bool
	loadErr    error
	beforeLoad func()
}

func (f *fakeCaster) Launch(context.Context, string) (App, error) {
	return App{SessionID: "s", TransportID: "t", AppID: DefaultMediaReceiver}, nil
}
func (f *fakeCaster) Load(_ context.Context, _ App, url, _, _ string) error {
	if f.beforeLoad != nil {
		f.beforeLoad()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loadErr != nil {
		return f.loadErr
	}
	f.loaded = url
	return nil
}

func (f *fakeCaster) loadedURL() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loaded
}
func (f *fakeCaster) StopApp(context.Context, App) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = true
	return nil
}

func (f *fakeCaster) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// fakeGraph is safe for concurrent use because the running cast re-reads it on
// its own goroutine while a test changes what it reports.
type fakeGraph struct {
	mu      sync.Mutex
	sources []capture.Node
}

func (f *fakeGraph) SourcesFeeding(int) ([]capture.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sources, nil
}

func (f *fakeGraph) becomes(sources ...capture.Node) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sources = sources
}

// harness wires a backend to fakes and records what happened in what order.
type harness struct {
	backend  *Backend
	portal   *fakePortal
	pipeline *fakePipeline
	server   *fakeServer
	caster   *fakeCaster

	graph *fakeGraph

	mu     sync.Mutex
	order  []string
	states []session.State
	dir    string
}

func newHarness(t *testing.T, sources []capture.Node) *harness {
	t.Helper()
	h := &harness{
		portal:   &fakePortal{},
		pipeline: newPipeline(),
		server:   &fakeServer{},
		caster:   &fakeCaster{},
		dir:      t.TempDir(),
	}

	// A playlist with enough segments, so the wait is not what a test measures.
	playlist := "#EXTM3U\n" + strings.Repeat("#EXTINF:1,\nseg.ts\n", 4)
	if err := os.WriteFile(filepath.Join(h.dir, capture.PlaylistName), []byte(playlist), 0o644); err != nil {
		t.Fatal(err)
	}

	record := func(what string) {
		h.mu.Lock()
		h.order = append(h.order, what)
		h.mu.Unlock()
	}

	h.graph = &fakeGraph{sources: sources}
	h.backend = &Backend{
		Config: Config{FPS: 30, Encoder: "x264enc", Port: 8010, StartTimeout: 2 * time.Second},
		OpenPortal: func(context.Context) (Portal, error) {
			record("portal")
			return h.portal, nil
		},
		SerialOf: func(uint32) (uint32, error) { return 4617, nil },
		Graph:    h.graph,
		StartCapture: func(capture.Options) (Pipeline, error) {
			record("capture")
			return h.pipeline, nil
		},
		Serve: func(string, int, string) (Server, error) {
			record("serve")
			return h.server, nil
		},
		Dial: func(context.Context, string) (Caster, error) {
			record("dial")
			return h.caster, nil
		},
		LocalAddress: func(string) (string, error) { return "192.168.100.8", nil },
		TempDir:      func() (string, error) { return h.dir, nil },
		Emit: func(_ discovery.Device, state session.State, _ string) {
			h.mu.Lock()
			h.states = append(h.states, state)
			h.mu.Unlock()
		},
	}
	return h
}

func (h *harness) seen() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.order...)
}

func (h *harness) reported() []session.State {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]session.State(nil), h.states...)
}

var screen = capture.Node{ID: 76, Name: "xdg-desktop-portal-hyprland"}
var webcam = capture.Node{ID: 46, Name: "v4l2_input", Role: "Camera"}

func TestStartCastsTheGrantedScreen(t *testing.T) {
	h := newHarness(t, []capture.Node{screen})

	if err := h.backend.Start(context.Background(), tv, "mirror"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if h.caster.loadedURL() == "" {
		t.Fatal("the receiver was never given a URL")
	}
	if !strings.HasSuffix(h.caster.loadedURL(), capture.PlaylistName) {
		t.Errorf("loaded %q, want the playlist", h.caster.loadedURL())
	}
	want := []session.State{session.Connecting, session.Streaming}
	if got := h.reported(); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("reported %v, want %v", got, want)
	}
}

// The rule the whole backend exists to keep. A capture that turns out to be a
// camera must never become a URL a television can fetch.
func TestACaptureThatIsNotTheScreenIsNeverServed(t *testing.T) {
	h := newHarness(t, []capture.Node{webcam})

	err := h.backend.Start(context.Background(), tv, "mirror")
	if !errors.Is(err, capture.ErrWrongSource) {
		t.Fatalf("Start returned %v, want ErrWrongSource", err)
	}
	if h.caster.loadedURL() != "" {
		t.Fatal("a receiver was given a URL for a capture that was not the screen")
	}
	for _, step := range h.seen() {
		if step == "serve" || step == "dial" {
			t.Fatalf("%q ran even though the capture was refused; steps: %v", step, h.seen())
		}
	}
	if h.pipeline.terminations() == 0 {
		t.Error("the refused capture was left running")
	}
	if !h.portal.closed {
		t.Error("the portal session was left open")
	}
}

// Ordering, stated as its own rule: the check must happen before the stream is
// reachable. Verifying after Serve would leave a window in which a camera is
// fetchable from the network.
func TestTheCaptureIsVerifiedBeforeItIsServed(t *testing.T) {
	h := newHarness(t, []capture.Node{screen})
	if err := h.backend.Start(context.Background(), tv, "mirror"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	steps := h.seen()
	capturedAt, servedAt := -1, -1
	for i, s := range steps {
		switch s {
		case "capture":
			capturedAt = i
		case "serve":
			servedAt = i
		}
	}
	if capturedAt < 0 || servedAt < 0 || capturedAt > servedAt {
		t.Fatalf("unexpected order %v", steps)
	}
}

func TestStopEndsTheCastAndReleasesEverything(t *testing.T) {
	h := newHarness(t, []capture.Node{screen})
	if err := h.backend.Start(context.Background(), tv, "mirror"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := h.backend.Stop(context.Background(), tv); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if !h.caster.wasStopped() {
		t.Error("the receiver was left showing the app")
	}
	if h.pipeline.terminations() == 0 {
		t.Error("the capture was left running")
	}
	if !h.server.closed || !h.portal.closed || !h.caster.isClosed() {
		t.Errorf("something was left open: server=%v portal=%v caster=%v",
			h.server.closed, h.portal.closed, h.caster.isClosed())
	}

	// Stopping is announced before the work. Without it a stop that takes a
	// moment leaves the indicator green, which reads as still casting.
	got := h.reported()
	if len(got) < 4 || got[2] != session.Stopping || got[3] != session.Idle {
		t.Errorf("reported %v, want ... Stopping, Idle", got)
	}
}

// A capture that dies on its own must be reported. Leaving the session green
// while the television shows nothing is the failure mode this project keeps
// finding.
func TestACaptureThatDiesIsReportedAsFailed(t *testing.T) {
	h := newHarness(t, []capture.Node{screen})
	if err := h.backend.Start(context.Background(), tv, "mirror"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	h.pipeline.Terminate() // as if it crashed

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got := h.reported()
		if len(got) > 0 && got[len(got)-1] == session.Failed {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("never reported Failed; reported %v", h.reported())
}

// A stop must not also be announced as a crash.
func TestStoppingDoesNotAlsoReportAFailure(t *testing.T) {
	h := newHarness(t, []capture.Node{screen})
	if err := h.backend.Start(context.Background(), tv, "mirror"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := h.backend.Stop(context.Background(), tv); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	time.Sleep(150 * time.Millisecond)
	for _, s := range h.reported() {
		if s == session.Failed {
			t.Fatalf("a deliberate stop was reported as a failure: %v", h.reported())
		}
	}
}

func TestSubmitPinIsRefused(t *testing.T) {
	var b Backend
	if err := b.SubmitPin(context.Background(), tv, "1234"); !errors.Is(err, ErrNoPin) {
		t.Fatalf("got %v, want ErrNoPin", err)
	}
}

func TestASecondCastToTheSameReceiverIsRefused(t *testing.T) {
	h := newHarness(t, []capture.Node{screen})
	if err := h.backend.Start(context.Background(), tv, "mirror"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := h.backend.Start(context.Background(), tv, "mirror"); err == nil {
		t.Fatal("a second cast to the same receiver was allowed")
	}
}

// Starting a cast can take minutes, nearly all of it spent waiting for the
// user to answer the share prompt. A stop arriving in that window must leave
// nothing running.
//
// Before this was fixed, the stop tore down a session record that had not been
// filled in yet, and the start then went on to bring up a capture, an HTTP
// server serving the screen to the network, and a session on the television --
// none of which anything was tracking, and a second stop was a no-op. The user
// had pressed stop and their screen was still being broadcast.
func TestStoppingDuringTheSharePromptLeavesNothingRunning(t *testing.T) {
	h := newHarness(t, []capture.Node{screen})

	atPrompt := make(chan struct{})
	release := make(chan struct{})
	h.backend.OpenPortal = func(context.Context) (Portal, error) {
		close(atPrompt)
		<-release // the user is staring at "Select what to share"
		return h.portal, nil
	}

	started := make(chan error, 1)
	go func() { started <- h.backend.Start(context.Background(), tv, "mirror") }()

	<-atPrompt
	if err := h.backend.Stop(context.Background(), tv); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	close(release)

	if err := <-started; err != nil {
		t.Fatalf("Start reported %v; a cancelled start is not a failure", err)
	}

	if h.caster.loadedURL() != "" {
		t.Error("a receiver was given a URL for a cast the user had already stopped")
	}
	// Whatever got as far as being created must have been released. Some
	// steps never run at all when the stop lands early, and demanding that a
	// resource which was never created be closed would test the timing rather
	// than the rule.
	h.assertNothingLeftRunning(t)

	h.backend.mu.Lock()
	tracked := len(h.backend.sessions)
	h.backend.mu.Unlock()
	if tracked != 0 {
		t.Errorf("%d sessions still tracked", tracked)
	}
}

// The same window, one step later: stopped after the capture is running but
// before the television is dialled.
func TestStoppingBeforeTheReceiverIsDialledLeavesNothingRunning(t *testing.T) {
	h := newHarness(t, []capture.Node{screen})

	atDial := make(chan struct{})
	release := make(chan struct{})
	h.backend.Dial = func(context.Context, string) (Caster, error) {
		close(atDial)
		<-release
		return h.caster, nil
	}

	started := make(chan error, 1)
	go func() { started <- h.backend.Start(context.Background(), tv, "mirror") }()

	<-atDial
	if err := h.backend.Stop(context.Background(), tv); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	close(release)
	<-started

	if h.caster.loadedURL() != "" {
		t.Error("a receiver was given a URL after the user pressed stop")
	}
	h.assertNothingLeftRunning(t)

	// This one got far enough that both really were created, so "released"
	// here means the stop actually reached them.
	if h.pipeline.terminations() == 0 {
		t.Error("the capture was left running")
	}
	if !h.server.closed {
		t.Error("the HTTP server was left serving the screen to the network")
	}
}

// The last step has no adoption after it, so only the final check catches a
// stop that lands here -- while the television is being told what to play.
func TestStoppingDuringTheFinalStepLeavesNothingRunning(t *testing.T) {
	h := newHarness(t, []capture.Node{screen})

	atLoad := make(chan struct{})
	release := make(chan struct{})
	h.caster.beforeLoad = func() {
		close(atLoad)
		<-release
	}

	started := make(chan error, 1)
	go func() { started <- h.backend.Start(context.Background(), tv, "mirror") }()

	<-atLoad
	stopped := make(chan error, 1)
	go func() { stopped <- h.backend.Stop(context.Background(), tv) }()
	time.Sleep(20 * time.Millisecond)
	close(release)

	<-started
	if err := <-stopped; err != nil {
		t.Fatalf("Stop: %v", err)
	}

	h.assertNothingLeftRunning(t)

	h.backend.mu.Lock()
	tracked := len(h.backend.sessions)
	h.backend.mu.Unlock()
	if tracked != 0 {
		t.Errorf("%d sessions still tracked after a stop", tracked)
	}
	// The capture must not survive: the user pressed stop, and the screen is
	// still being encoded until something terminates it.
	if h.pipeline.terminations() == 0 {
		t.Error("the capture was left running after the user pressed stop")
	}
}

func (f *fakeCaster) wasStopped() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}

func (f *fakeCaster) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// assertNothingLeftRunning checks that every resource the start actually
// created was released.
func (h *harness) assertNothingLeftRunning(t *testing.T) {
	t.Helper()
	created := map[string]bool{}
	for _, step := range h.seen() {
		created[step] = true
	}

	if created["portal"] && !h.portal.closed {
		t.Error("the screen-capture grant was left open")
	}
	if created["capture"] && h.pipeline.terminations() == 0 {
		t.Error("the capture was left running")
	}
	if created["serve"] && !h.server.closed {
		t.Error("the HTTP server was left serving the screen to the network")
	}
	if created["dial"] && !h.caster.isClosed() {
		t.Error("the connection to the receiver was left open")
	}
}

// The granted node can go away underneath a running pipeline -- pipewire
// restarting on an upgrade, the grant revoked, a monitor unplugged -- and the
// source has autoconnect on because no configuration of it fails safe. So a
// cast that has been running for an hour must still be proving what it sends.
func TestACastThatChangesToACameraIsTornDownWhileRunning(t *testing.T) {
	h := newHarness(t, []capture.Node{screen})

	if err := h.backend.Start(context.Background(), tv, "mirror"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The graph changes underneath the running cast.
	h.graph.becomes(webcam)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got := h.reported()
		if len(got) > 0 && got[len(got)-1] == session.Failed {
			if h.pipeline.terminations() == 0 {
				t.Error("the substituted capture was left running")
			}
			if !h.server.closed {
				t.Error("the substituted capture was left served to the network")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("a cast that switched to the webcam kept running")
}
