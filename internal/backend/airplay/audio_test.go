package airplay

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrCode/castr/internal/session"
)

// fakePactl models the parts of pipewire-pulse the virtual output relies on:
// a default output, null-sink modules, and the session manager falling back
// to another output when the default one disappears.
type fakePactl struct {
	mu       sync.Mutex
	def      string
	fallback string
	modules  map[string]string // module id -> sink name
	props    map[string]string // module id -> sink_properties argument
	next     int
	calls    []string
	failLoad bool
}

func newFakePactl(def string) *fakePactl {
	return &fakePactl{def: def, fallback: def, modules: map[string]string{}, props: map[string]string{}, next: 40}
}

func (f *fakePactl) run(args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join(args, " "))
	switch args[0] {
	case "get-default-sink":
		return f.def + "\n", nil
	case "set-default-sink":
		f.def = args[1]
		return "", nil
	case "load-module":
		if f.failLoad {
			return "", errors.New("pactl: failure")
		}
		f.next++
		id := strconv.Itoa(f.next)
		f.modules[id] = strings.TrimPrefix(args[2], "sink_name=")
		f.props[id] = args[3]
		return id + "\n", nil
	case "unload-module":
		name := f.modules[args[1]]
		delete(f.modules, args[1])
		if f.def == name {
			f.def = f.fallback
		}
		return "", nil
	case "list":
		ids := make([]string, 0, len(f.modules))
		for id := range f.modules {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		var sb strings.Builder
		fmt.Fprintf(&sb, "7\tmodule-null-sink\tsink_name=someone_else\n")
		for _, id := range ids {
			fmt.Fprintf(&sb, "%s\tmodule-null-sink\tsink_name=%s %s\n", id, f.modules[id], f.props[id])
		}
		return sb.String(), nil
	}
	return "", fmt.Errorf("unexpected pactl %v", args)
}

func (f *fakePactl) state() (def string, sinks int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.def, len(f.modules)
}

// withPactl wires a fake pactl into the harness and records the default
// output at the moment each child is spawned: castr-airplay reads it then.
func withPactl(h *harness, def string) (*fakePactl, *[]string) {
	f := newFakePactl(def)
	h.backend.Pactl = f.run
	var atSpawn []string
	spawn := h.backend.Spawn
	h.backend.Spawn = func(ctx context.Context, argv []string) (Process, error) {
		d, _ := f.state()
		h.mu.Lock()
		atSpawn = append(atSpawn, d)
		h.mu.Unlock()
		return spawn(ctx, argv)
	}
	return f, &atSpawn
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !cond() {
		time.Sleep(5 * time.Millisecond)
	}
}

func TestACastSendsTheSoundToAVirtualOutputAndBack(t *testing.T) {
	h := newHarness(t, readyOutput)
	f, atSpawn := withPactl(h, "speakers")
	ctx := context.Background()

	if err := h.backend.Start(ctx, device("a", "75in Hisense Roku TV"), session.ModeMirror); err != nil {
		t.Fatal(err)
	}
	if got := (*atSpawn)[0]; got != SinkName {
		t.Fatalf("default output when castr-airplay started = %q, want the virtual %q", got, SinkName)
	}
	if def, sinks := f.state(); def != SinkName || sinks != 1 {
		t.Fatalf("while casting: default %q with %d virtual outputs, want %q and 1", def, sinks, SinkName)
	}
	for _, props := range f.props {
		if !strings.Contains(props, `device.description="75in Hisense Roku TV (cast)"`) ||
			!strings.Contains(props, "monitor.channel-volumes=true") {
			t.Errorf("virtual output properties = %q", props)
		}
	}

	if err := h.backend.Stop(ctx, device("a", "75in Hisense Roku TV")); err != nil {
		t.Fatal(err)
	}
	if def, sinks := f.state(); def != "speakers" || sinks != 0 {
		t.Fatalf("after stop: default %q with %d virtual outputs, want speakers and 0", def, sinks)
	}
}

func TestACastWithoutAudioLeavesTheSoundAlone(t *testing.T) {
	h := newHarness(t, readyOutput)
	h.backend.Config.Audio = false
	f, _ := withPactl(h, "speakers")
	ctx := context.Background()

	if err := h.backend.Start(ctx, device("a", "TV"), session.ModeMirror); err != nil {
		t.Fatal(err)
	}
	if err := h.backend.Stop(ctx, device("a", "TV")); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("a cast without audio ran pactl: %v", f.calls)
	}
}

func TestTwoCastsShareOneVirtualOutput(t *testing.T) {
	h := newHarness(t, readyOutput)
	f, _ := withPactl(h, "speakers")
	ctx := context.Background()

	if err := h.backend.Start(ctx, device("a", "One"), session.ModeMirror); err != nil {
		t.Fatal(err)
	}
	if err := h.backend.Start(ctx, device("b", "Two"), session.ModeMirror); err != nil {
		t.Fatal(err)
	}
	if _, sinks := f.state(); sinks != 1 {
		t.Fatalf("%d virtual outputs for two casts, want 1", sinks)
	}

	if err := h.backend.Stop(ctx, device("a", "One")); err != nil {
		t.Fatal(err)
	}
	if def, sinks := f.state(); def != SinkName || sinks != 1 {
		t.Fatalf("stopping one cast took the sound from the other: default %q, %d outputs", def, sinks)
	}

	if err := h.backend.Stop(ctx, device("b", "Two")); err != nil {
		t.Fatal(err)
	}
	if def, sinks := f.state(); def != "speakers" || sinks != 0 {
		t.Fatalf("after the last stop: default %q with %d outputs, want speakers and 0", def, sinks)
	}
}

func TestACrashGivesTheSoundBack(t *testing.T) {
	h := newHarness(t, readyOutput)
	h.childExits = true
	f, _ := withPactl(h, "speakers")

	if err := h.backend.Start(context.Background(), device("a", "TV"), session.ModeMirror); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		def, sinks := f.state()
		return h.sawState(session.Failed) && def == "speakers" && sinks == 0
	})

	if def, sinks := f.state(); def != "speakers" || sinks != 0 {
		t.Fatalf("after a crash: default %q with %d virtual outputs, want speakers and 0", def, sinks)
	}
}

func TestAFailedStartGivesTheSoundBack(t *testing.T) {
	h := newHarness(t, "mirror session ready\n") // never reaches the ready marker
	f, _ := withPactl(h, "speakers")

	if err := h.backend.Start(context.Background(), device("a", "TV"), session.ModeMirror); err == nil {
		t.Fatal("a start that never streamed reported success")
	}
	if def, sinks := f.state(); def != "speakers" || sinks != 0 {
		t.Fatalf("after a failed start: default %q with %d virtual outputs, want speakers and 0", def, sinks)
	}
}

func TestASpawnFailureGivesTheSoundBack(t *testing.T) {
	h := newHarness(t, readyOutput)
	f := newFakePactl("speakers")
	h.backend.Pactl = f.run
	h.backend.Spawn = func(context.Context, []string) (Process, error) { return nil, errors.New("no such file") }

	if err := h.backend.Start(context.Background(), device("a", "TV"), session.ModeMirror); err == nil {
		t.Fatal("a failed spawn reported success")
	}
	if def, sinks := f.state(); def != "speakers" || sinks != 0 {
		t.Fatalf("after a failed spawn: default %q with %d virtual outputs, want speakers and 0", def, sinks)
	}
}

// A daemon killed mid-cast leaves its virtual output, possibly as the default.
// The next cast must not remember that leftover as the output to go back to.
func TestALeftoverOutputIsReplacedNotRestored(t *testing.T) {
	h := newHarness(t, readyOutput)
	f, _ := withPactl(h, "speakers")
	f.modules["12"] = SinkName
	f.props["12"] = `sink_properties=device.description="old (cast)"`
	f.def = SinkName
	ctx := context.Background()

	if err := h.backend.Start(ctx, device("a", "TV"), session.ModeMirror); err != nil {
		t.Fatal(err)
	}
	if _, sinks := f.state(); sinks != 1 {
		t.Fatalf("%d virtual outputs while casting, want the leftover replaced by 1", sinks)
	}
	if _, ok := f.modules["7"]; ok {
		t.Fatal("removed a null sink that is not castr's")
	}
	if err := h.backend.Stop(ctx, device("a", "TV")); err != nil {
		t.Fatal(err)
	}
	if def, sinks := f.state(); def != "speakers" || sinks != 0 {
		t.Fatalf("after stop: default %q with %d outputs, want speakers and 0", def, sinks)
	}
}

func TestAPactlFailureDoesNotStopTheCast(t *testing.T) {
	h := newHarness(t, readyOutput)
	f, _ := withPactl(h, "speakers")
	f.failLoad = true
	ctx := context.Background()

	if err := h.backend.Start(ctx, device("a", "TV"), session.ModeMirror); err != nil {
		t.Fatalf("the cast failed because the virtual output could not be made: %v", err)
	}
	if def, _ := f.state(); def != "speakers" {
		t.Fatalf("default changed to %q without a virtual output", def)
	}
	if err := h.backend.Stop(ctx, device("a", "TV")); err != nil {
		t.Fatal(err)
	}
	if def, _ := f.state(); def != "speakers" {
		t.Fatalf("after stop: default %q, want speakers", def)
	}
}

func TestTheReceiversNameCannotBreakOutOfTheLabel(t *testing.T) {
	for in, want := range map[string]string{
		"75in Hisense Roku TV":                    "75in Hisense Roku TV (cast)",
		`Evil" monitor.channel-volumes=false x="`: "Evil monitor.channel-volumes false x (cast)",
		"":                        "TV (cast)",
		"Tele sala":               "Tele sala (cast)",
		strings.Repeat("a", 60):   strings.Repeat("a", 40) + " (cast)",
		"[LG] webOS TV UP7710PSB": "LG webOS TV UP7710PSB (cast)",
	} {
		if got := sinkDescription(in); got != want {
			t.Errorf("sinkDescription(%q) = %q, want %q", in, got, want)
		}
	}
}
