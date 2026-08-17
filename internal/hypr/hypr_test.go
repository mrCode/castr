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

	// ignoreMirror models a compositor that answers "ok" to a mirror request
	// and does not apply it. This is not hypothetical: it is precisely what
	// Hyprland 0.56.2 did through `hyprctl keyword`, and castr believed it all
	// the way to a live Apple TV that would have shown an empty desktop.
	ignoreMirror bool
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
		for i, m := range f.monitors {
			parts = append(parts, fmt.Sprintf(
				`{"id":%d,"name":%q,"width":%d,"height":%d,"refreshRate":%v,"x":%d,"y":%d,"scale":%v,"focused":%v,"mirrorOf":%q}`,
				i, m.Name, m.Width, m.Height, m.Refresh, m.X, m.Y, m.Scale, m.Focused, m.MirrorOf))
		}
		return "[" + strings.Join(parts, ",") + "]", nil

	case strings.HasPrefix(joined, "output create headless"):
		want := args[len(args)-1]
		// Measured on 0.56.2: creating a name that already exists is refused,
		// and the refusal still exits 0.
		if f.has(want) {
			return "Name already taken", nil
		}
		f.monitors = append(f.monitors, Monitor{Name: want,
			Width: 1920, Height: 1080, Refresh: 60, MirrorOf: "none"})
		return "ok", nil

	case strings.HasPrefix(joined, "output remove"):
		target := args[len(args)-1]
		for _, m := range f.monitors {
			if m.Name != target {
				continue
			}
			// THE RULE THAT COST US A LIVE CAST: a mirroring output cannot be
			// removed on 0.56.2. hyprctl says "output not found" about an
			// output it is itself listing, and exits 0.
			if m.MirrorOf != "" && m.MirrorOf != "none" {
				return "output not found", nil
			}
			f.drop(target)
			return "ok", nil
		}
		return "output not found", nil

	case strings.HasPrefix(joined, "eval "):
		return f.eval(args[len(args)-1]), nil

	case strings.HasPrefix(joined, "keyword "):
		// Refused outright by the Lua parser, and still exit 0.
		return "keyword can't work with non-legacy parsers. Use eval.", nil
	}
	return "ok", nil
}

// eval applies an hl.monitor call the way the compositor would.
func (f *fake) eval(code string) string {
	spec := map[string]string{}
	for _, field := range []string{"output", "mode", "position", "mirror"} {
		if v, ok := luaField(code, field); ok {
			spec[field] = v
		}
	}
	output, ok := spec["output"]
	if !ok {
		return "ok" // hyprctl accepts nonsense Lua silently; so does this
	}

	for i := range f.monitors {
		if f.monitors[i].Name != output {
			continue
		}
		if mode, ok := spec["mode"]; ok {
			var w, h int
			var refresh float64
			if _, err := fmt.Sscanf(mode, "%dx%d@%f", &w, &h, &refresh); err == nil {
				f.monitors[i].Width, f.monitors[i].Height = w, h
				f.monitors[i].Refresh = refresh
			}
		}
		if position, ok := spec["position"]; ok && position != "auto" {
			var x, y int
			if _, err := fmt.Sscanf(position, "%dx%d", &x, &y); err == nil {
				f.monitors[i].X, f.monitors[i].Y = x, y
			}
		}
		if mirror, ok := spec["mirror"]; ok && f.ignoreMirror {
			// Accepted, and quietly dropped.
		} else if ok {
			if mirror == MirrorNone {
				f.monitors[i].MirrorOf = "none"
			} else {
				// Reported as the SOURCE'S ID, not its name -- the detail that
				// makes a naive verification pass when nothing is mirrored.
				f.monitors[i].MirrorOf = f.idOf(mirror)
			}
		}
		if strings.Contains(code, "disabled = true") {
			f.drop(output)
		}
		return "ok"
	}
	return "ok"
}

// luaField pulls `name = "value"` out of an hl.monitor call.
func luaField(code, name string) (string, bool) {
	marker := name + " = \""
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

func (f *fake) idOf(name string) string {
	for i, m := range f.monitors {
		if m.Name == name {
			return fmt.Sprintf("%d", i)
		}
	}
	return "none"
}

func (f *fake) drop(name string) {
	kept := f.monitors[:0]
	for _, m := range f.monitors {
		if m.Name != name {
			kept = append(kept, m)
		}
	}
	f.monitors = kept
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

	// Asserted on the compositor's STATE, not on the command we sent. The
	// command was sent correctly for a whole Hyprland release while the mirror
	// was never applied, and only state would have shown it.
	var got Monitor
	for _, m := range f.monitors {
		if m.Name == OutputMirror {
			got = m
		}
	}
	if got.Name == "" {
		t.Fatal("no mirror output exists")
	}
	if got.MirrorOf == "" || got.MirrorOf == "none" {
		t.Errorf("mirrorOf = %q, want it mirroring the panel", got.MirrorOf)
	}
	if got.Width != 1920 || got.Height != 1080 {
		t.Errorf("size = %dx%d, want 1920x1080", got.Width, got.Height)
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
	f.failOn = "eval"

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

func TestAMirrorThatTheCompositorSilentlyIgnoresIsAFailure(t *testing.T) {
	// The exact production bug. hyprctl answered "ok", the output existed, the
	// panel was untouched -- and the mirror was never applied, so the receiver
	// would have shown an empty desktop with no error anywhere in the logs.
	//
	// Asking is not enough. This checks.
	f := newFake(panel())
	f.ignoreMirror = true

	_, err := CreateOutput(f.run, OutputMirror, "eDP-2")

	if err == nil {
		t.Fatal("a mirror that was never applied was reported as success")
	}
	if !strings.Contains(err.Error(), "mirror") {
		t.Errorf("err = %q, want it to name the problem", err)
	}
	if f.has(OutputMirror) {
		t.Errorf("the unusable output was left behind: %v", f.names())
	}
}

func TestRemovingAMirroringOutputUnmirrorsItFirst(t *testing.T) {
	// On 0.56.2 a mirroring output cannot be removed at all: `output remove`
	// answers "output not found" about an output it is itself listing, exits
	// 0, and the output stays on the desk stealing windows until the
	// compositor restarts.
	f := newFake(panel())
	name, err := CreateOutput(f.run, OutputMirror, "eDP-2")
	if err != nil {
		t.Fatal(err)
	}
	if f.mirrorOf(name) == "" {
		t.Fatal("the output is not mirroring; this test proves nothing")
	}

	if err := RemoveOutput(f.run, name); err != nil {
		t.Fatal(err)
	}

	if f.has(name) {
		t.Error("a mirroring output survived removal -- it would sit on the desk forever")
	}
}

func (f *fake) mirrorOf(name string) string {
	for _, m := range f.monitors {
		if m.Name == name && m.MirrorOf != "none" {
			return m.MirrorOf
		}
	}
	return ""
}
