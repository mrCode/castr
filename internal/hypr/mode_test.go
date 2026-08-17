package hypr

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// panelFake wraps the package's compositor model with panel-shaped helpers.
// It deliberately does NOT model hyprctl a second time: one statement of the
// compositor's rules, in one place, is what keeps the fakes honest.
type panelFake struct {
	*fake
	failNext bool
}

func newPanelFake() *panelFake {
	f := newFake(
		Monitor{Name: "eDP-2", Width: 2560, Height: 1600, Refresh: 240, X: 0, Y: 0,
			Scale: 1.6, Focused: true, MirrorOf: "none"},
		Monitor{Name: "DP-1", Width: 3840, Height: 2160, Refresh: 60, X: 2560, Y: 0,
			Scale: 1, MirrorOf: "none"},
	)
	return &panelFake{fake: f}
}

func (p *panelFake) run(name string, args ...string) (string, error) {
	if p.failNext && strings.HasPrefix(strings.Join(args, " "), "eval") {
		p.failNext = false
		return "", errors.New("invalid mode")
	}
	return p.fake.run(name, args...)
}

func (p *panelFake) panel() Monitor {
	for _, m := range p.fake.monitors {
		if m.Name == "eDP-2" {
			return m
		}
	}
	return Monitor{}
}

// appliedArgs returns the Lua of every configuration applied, newest last.
func (p *panelFake) appliedArgs() []string {
	var out []string
	for _, c := range p.fake.calls {
		if len(c) >= 3 && c[1] == "eval" {
			out = append(out, c[2])
		}
	}
	return out
}

func TestSwitchingThePanelAndPuttingItBackIsLossless(t *testing.T) {
	// The panel has only 2560x1600@240 and @60, so a careless restore leaves
	// the user on a display four times slower with nothing explaining why.
	f := newPanelFake()
	dir := t.TempDir()
	before := f.panel()

	if err := SwitchPanel(f.run, dir); err != nil {
		t.Fatal(err)
	}
	if got := f.panel(); got.Width != 1920 || got.Height != 1080 {
		t.Fatalf("panel = %dx%d, want the streaming geometry", got.Width, got.Height)
	}

	if err := RestorePanel(f.run, dir); err != nil {
		t.Fatal(err)
	}

	after := f.panel()
	if after.Width != before.Width || after.Height != before.Height {
		t.Errorf("panel = %dx%d, want %dx%d back",
			after.Width, after.Height, before.Width, before.Height)
	}
	if after.Refresh != before.Refresh {
		t.Errorf("refresh = %v, want %v back -- this is the 240Hz the user lost",
			after.Refresh, before.Refresh)
	}
}

func TestTheRestoredModeKeepsThePanelWhereItWas(t *testing.T) {
	// Restoring without a position moves the panel to 0x0 and shuffles every
	// other monitor on the desk.
	f := newPanelFake()
	f.fake.monitors[0].X, f.fake.monitors[0].Y = 1920, 200
	dir := t.TempDir()

	if err := SwitchPanel(f.run, dir); err != nil {
		t.Fatal(err)
	}
	if err := RestorePanel(f.run, dir); err != nil {
		t.Fatal(err)
	}

	applied := f.appliedArgs()
	last := applied[len(applied)-1]
	if !strings.Contains(last, `position = "1920x200"`) {
		t.Errorf("restore = %q, want the original position", last)
	}
}

func TestTheScaleSurvivesTheRoundTrip(t *testing.T) {
	// A 1.6 scale silently becoming 1 makes every window tiny.
	f := newPanelFake()
	dir := t.TempDir()

	if err := SwitchPanel(f.run, dir); err != nil {
		t.Fatal(err)
	}
	if err := RestorePanel(f.run, dir); err != nil {
		t.Fatal(err)
	}

	applied := f.appliedArgs()
	if !strings.Contains(applied[len(applied)-1], "scale = 1.6") {
		t.Errorf("restore = %q, want the original scale", applied[len(applied)-1])
	}
}

func TestRestoringWithoutHavingSwitchedChangesNothing(t *testing.T) {
	// Otherwise it fights whatever the user set during the cast.
	f := newPanelFake()

	if err := RestorePanel(f.run, t.TempDir()); err != nil {
		t.Fatal(err)
	}

	if got := f.appliedArgs(); len(got) != 0 {
		t.Errorf("applied %v, want nothing touched", got)
	}
}

func TestADaemonThatDiedMidCastRestoresThePanelOnItsNextStart(t *testing.T) {
	// Otherwise the panel stays at 1080p60 forever and the user has no idea
	// what it used to be.
	f := newPanelFake()
	dir := t.TempDir()
	if err := SwitchPanel(f.run, dir); err != nil {
		t.Fatal(err)
	}
	// ...daemon is killed here; nothing restores it.

	if !RestorePanelIfPending(f.run, dir) {
		t.Fatal("a pending restore was not noticed")
	}

	if got := f.panel(); got.Width != 2560 || got.Refresh != 240 {
		t.Errorf("panel = %dx%d@%v, want the original back", got.Width, got.Height, got.Refresh)
	}
}

func TestAFailedRestoreKeepsTheSavedModeForAnotherTry(t *testing.T) {
	// Forgetting it leaves the panel wrong forever.
	f := newPanelFake()
	dir := t.TempDir()
	if err := SwitchPanel(f.run, dir); err != nil {
		t.Fatal(err)
	}
	f.failNext = true

	if err := RestorePanel(f.run, dir); err == nil {
		t.Fatal("a failed restore reported success")
	}

	if err := RestorePanel(f.run, dir); err != nil {
		t.Fatalf("the saved mode was thrown away after one failure: %v", err)
	}
	if got := f.panel(); got.Width != 2560 {
		t.Errorf("panel = %dx%d, want the retry to have worked", got.Width, got.Height)
	}
}

func TestAFailedSwitchLeavesNoSavedModeToRestoreLater(t *testing.T) {
	// A saved mode with no switch behind it would "restore" a panel nobody
	// changed, at the next daemon start.
	f := newPanelFake()
	dir := t.TempDir()
	f.failNext = true

	if err := SwitchPanel(f.run, dir); err == nil {
		t.Fatal("a failed switch reported success")
	}

	if RestorePanelIfPending(f.run, dir) {
		t.Error("a failed switch left a restore pending")
	}
}

func TestACorruptSavedModeIsIgnoredRatherThanApplied(t *testing.T) {
	// Applying a mode parsed out of nonsense is worse than leaving the panel be.
	f := newPanelFake()
	dir := t.TempDir()
	if err := os.WriteFile(modePath(dir), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := RestorePanel(f.run, dir); err != nil {
		t.Fatal(err)
	}

	if got := f.appliedArgs(); len(got) != 0 {
		t.Errorf("applied %v from a corrupt file", got)
	}
}

func TestOurOwnVirtualOutputIsNeverMistakenForThePanel(t *testing.T) {
	// It is focused for most of a cast. Switching IT accomplishes nothing and
	// saves a mode for an output that is about to be destroyed.
	f := newPanelFake()
	f.fake.monitors = append(f.fake.monitors, Monitor{Name: OutputExtend,
		Width: 1920, Height: 1080, Refresh: 60, Scale: 1, Focused: true,
		MirrorOf: "none"})
	f.fake.monitors[0].Focused = false

	if err := SwitchPanel(f.run, t.TempDir()); err != nil {
		t.Fatal(err)
	}

	applied := f.appliedArgs()
	if len(applied) != 1 || strings.HasPrefix(applied[0], OutputExtend) {
		t.Errorf("switched %v, want a real monitor", applied)
	}
}
