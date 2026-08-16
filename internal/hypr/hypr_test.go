package hypr

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// fake models the compositor's state, not just a canned string. A stub that
// only records calls would have hidden every bug this package exists to
// prevent: whether an output really exists is the whole question.
type fake struct {
	monitors []Monitor
	calls    [][]string
	failOn   string // substring of a command that should fail
}

func newFake(monitors ...Monitor) *fake {
	return &fake{monitors: monitors}
}

func panel() Monitor {
	// Real shape, from a laptop with no native 1080p mode.
	return Monitor{Name: "eDP-2", Width: 2560, Height: 1600, Refresh: 240,
		Scale: 2, Focused: true, MirrorOf: "none"}
}

func (f *fake) run(name string, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	joined := strings.Join(args, " ")

	if f.failOn != "" && strings.Contains(joined, f.failOn) {
		return "", errors.New("hyprctl said no")
	}

	switch {
	case strings.HasPrefix(joined, "-j monitors"):
		var parts []string
		for _, m := range f.monitors {
			parts = append(parts, fmt.Sprintf(
				`{"name":%q,"width":%d,"height":%d,"refreshRate":%v,"scale":%v,"focused":%v,"mirrorOf":%q}`,
				m.Name, m.Width, m.Height, m.Refresh, m.Scale, m.Focused, m.MirrorOf))
		}
		return "[" + strings.Join(parts, ",") + "]", nil

	case strings.HasPrefix(joined, "output create headless"):
		f.monitors = append(f.monitors, Monitor{Name: args[len(args)-1],
			Width: 1920, Height: 1080, Refresh: 60})
		return "ok", nil

	case strings.HasPrefix(joined, "output remove"):
		target := args[len(args)-1]
		kept := f.monitors[:0]
		for _, m := range f.monitors {
			if m.Name != target {
				kept = append(kept, m)
			}
		}
		f.monitors = kept
		return "ok", nil
	}
	return "ok", nil
}

func (f *fake) names() []string {
	var out []string
	for _, m := range f.monitors {
		out = append(out, m.Name)
	}
	return out
}

func (f *fake) has(name string) bool {
	for _, m := range f.monitors {
		if m.Name == name {
			return true
		}
	}
	return false
}

func TestCreateReturnsTheOutputName(t *testing.T) {
	f := newFake(panel())

	got, err := CreateOutput(f.run, OutputExtend, "")
	if err != nil {
		t.Fatal(err)
	}

	if got != OutputExtend {
		t.Errorf("name = %q, want %q", got, OutputExtend)
	}
	if !f.has(OutputExtend) {
		t.Error("the output was not created")
	}
}

func TestMirrorOutputMirrorsTheGivenMonitor(t *testing.T) {
	f := newFake(panel())

	if _, err := CreateOutput(f.run, OutputMirror, "eDP-2"); err != nil {
		t.Fatal(err)
	}

	var config string
	for _, c := range f.calls {
		if len(c) > 2 && c[1] == "keyword" && c[2] == "monitor" {
			config = c[3]
		}
	}
	if !strings.Contains(config, ",mirror,eDP-2") {
		t.Errorf("monitor config = %q, want it to mirror eDP-2", config)
	}
	if !strings.Contains(config, "1920x1080@60") {
		t.Errorf("monitor config = %q, want 1080p", config)
	}
}

func TestAnExistingStaleRuleDoesNotLookLikeFailure(t *testing.T) {
	// `monitors all` lists stale monitor RULES from earlier runs, so the name
	// can already be present. Diffing before/after came back empty here and
	// reported failure for an output that had just been created -- which made
	// the caller fall back to switching the panel, the very thing this avoids.
	f := newFake(panel(), Monitor{Name: OutputMirror, Width: 1920, Height: 1080})

	got, err := CreateOutput(f.run, OutputMirror, "eDP-2")
	if err != nil {
		t.Fatalf("create reported failure for an output that exists: %v", err)
	}
	if got != OutputMirror {
		t.Errorf("name = %q, want %q", got, OutputMirror)
	}
}

func TestARenamedOutputIsStillFound(t *testing.T) {
	// Naming a headless output is undocumented. If a Hyprland version drops
	// it, the output appears as HEADLESS-N and must still be usable.
	f := newFake(panel())
	f.monitors = append(f.monitors, Monitor{Name: "HEADLESS-2"})

	got, err := CreateOutput(f.run, OutputExtend, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != OutputExtend && got != "HEADLESS-2" {
		t.Errorf("name = %q, want the requested name or the headless one", got)
	}
}

func TestAFailedConfigurationLeavesNothingBehind(t *testing.T) {
	// Returning an error while leaving the output stranded a 1920x1080 monitor
	// on the desktop AND told the user nothing could be created.
	f := newFake(panel())
	f.failOn = "keyword monitor"

	_, err := CreateOutput(f.run, OutputExtend, "")

	if err == nil {
		t.Fatal("want an error when configuration fails")
	}
	if f.has(OutputExtend) {
		t.Errorf("output left behind: %v", f.names())
	}
}

func TestMirrorAndExtendUseDifferentNames(t *testing.T) {
	// Sharing one name meant either session's teardown destroyed the other's
	// live output.
	if OutputExtend == OutputMirror {
		t.Fatal("mirror and extend must not share an output name")
	}
}

func TestCleanupRemovesOurOwnStrays(t *testing.T) {
	f := newFake(panel(), Monitor{Name: OutputExtend}, Monitor{Name: OutputMirror})

	n, err := CleanupStrays(f.run, nil)
	if err != nil {
		t.Fatal(err)
	}

	if n != 2 {
		t.Errorf("removed %d, want 2", n)
	}
	if got := f.names(); len(got) != 1 || got[0] != "eDP-2" {
		t.Errorf("monitors after cleanup = %v, want just the panel", got)
	}
}

func TestCleanupNeverTouchesAnOutputALiveSessionOwns(t *testing.T) {
	// A second daemon swept the output of a cast running in another process.
	// The owning daemon logged nothing, because the damage happened elsewhere.
	f := newFake(panel(), Monitor{Name: OutputExtend}, Monitor{Name: OutputMirror})

	n, err := CleanupStrays(f.run, []string{OutputExtend})
	if err != nil {
		t.Fatal(err)
	}

	if n != 1 {
		t.Errorf("removed %d, want 1", n)
	}
	if !f.has(OutputExtend) {
		t.Error("cleanup removed an output a live session was casting")
	}
}

func TestCleanupLeavesOutputsWeDidNotCreate(t *testing.T) {
	// wayvnc and Sunshine make HEADLESS outputs. Sweeping every HEADLESS*
	// killed a user's live remote-desktop output on any invocation.
	f := newFake(panel(), Monitor{Name: "HEADLESS-1"}, Monitor{Name: "DP-3"})

	n, err := CleanupStrays(f.run, nil)
	if err != nil {
		t.Fatal(err)
	}

	if n != 0 {
		t.Errorf("removed %d outputs castr never created", n)
	}
	if len(f.names()) != 3 {
		t.Errorf("monitors = %v, want all three untouched", f.names())
	}
}

func TestFocusedMonitorIsTheMirrorSource(t *testing.T) {
	other := Monitor{Name: "HDMI-A-1", Focused: false}
	f := newFake(other, panel())

	got, err := Focused(f.run)
	if err != nil {
		t.Fatal(err)
	}

	if got != "eDP-2" {
		t.Errorf("focused = %q, want eDP-2", got)
	}
}

func TestFocusedFallsBackToTheFirstMonitor(t *testing.T) {
	f := newFake(Monitor{Name: "DP-1", Focused: false})

	got, err := Focused(f.run)
	if err != nil {
		t.Fatal(err)
	}
	if got != "DP-1" {
		t.Errorf("focused = %q, want DP-1", got)
	}
}

func TestMonitorsUsesTheListingThatIncludesMirrors(t *testing.T) {
	// A mirrored output does not appear in plain `hyprctl monitors`, so code
	// looking there concluded its own output had not been created.
	f := newFake(panel())

	if _, err := Monitors(f.run); err != nil {
		t.Fatal(err)
	}

	last := f.calls[len(f.calls)-1]
	if got := strings.Join(last, " "); !strings.HasSuffix(got, "monitors all") {
		t.Errorf("command = %q, want it to end with 'monitors all'", got)
	}
}

func TestABrokenHyprctlIsAnErrorNotAPanic(t *testing.T) {
	broken := func(string, ...string) (string, error) { return "not json", nil }

	if _, err := Monitors(broken); err == nil {
		t.Error("want an error for unparseable output")
	}
}
