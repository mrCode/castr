package hypr

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

func monitorJSON(m Monitor) string {
	raw, _ := json.Marshal(struct {
		Name     string  `json:"name"`
		Width    int     `json:"width"`
		Height   int     `json:"height"`
		Refresh  float64 `json:"refreshRate"`
		X        int     `json:"x"`
		Y        int     `json:"y"`
		Scale    float64 `json:"scale"`
		Focused  bool    `json:"focused"`
		MirrorOf string  `json:"mirrorOf"`
	}{m.Name, m.Width, m.Height, m.Refresh, m.X, m.Y, m.Scale, m.Focused, m.MirrorOf})
	return string(raw)
}

// fmtSscan parses "1920x1080@60.00000" the way hyprctl writes it.
func fmtSscan(spec string, w, h *int, refresh *float64) (int, error) {
	return fmt.Sscanf(spec, "%dx%d@%f", w, h, refresh)
}

// panelFake models a compositor whose monitor really changes mode.
type panelFake struct {
	mu       sync.Mutex
	monitors []Monitor
	applied  []string
	failNext bool
}

func newPanelFake() *panelFake {
	return &panelFake{monitors: []Monitor{
		{Name: "eDP-2", Width: 2560, Height: 1600, Refresh: 240, X: 0, Y: 0,
			Scale: 1.6, Focused: true},
		{Name: "DP-1", Width: 3840, Height: 2160, Refresh: 60, X: 2560, Y: 0, Scale: 1},
	}}
}

func (f *panelFake) run(name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	joined := strings.Join(args, " ")

	switch {
	case strings.HasPrefix(joined, "-j monitors"):
		var parts []string
		for _, m := range f.monitors {
			parts = append(parts, monitorJSON(m))
		}
		return "[" + strings.Join(parts, ",") + "]", nil

	case strings.HasPrefix(joined, "keyword monitor"):
		if f.failNext {
			f.failNext = false
			return "", errors.New("invalid mode")
		}
		arg := args[len(args)-1]
		f.applied = append(f.applied, arg)
		f.apply(arg)
		return "ok", nil
	}
	return "ok", nil
}

// apply mutates the modelled monitor, so a test can ask what the panel IS
// rather than what it was told.
func (f *panelFake) apply(arg string) {
	fields := strings.Split(arg, ",")
	if len(fields) < 2 {
		return
	}
	var w, h int
	var refresh float64
	if _, err := fmtSscan(fields[1], &w, &h, &refresh); err != nil {
		return
	}
	for i := range f.monitors {
		if f.monitors[i].Name == fields[0] {
			f.monitors[i].Width, f.monitors[i].Height = w, h
			f.monitors[i].Refresh = refresh
		}
	}
}

func (f *panelFake) panel() Monitor {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.monitors {
		if m.Name == "eDP-2" {
			return m
		}
	}
	return Monitor{}
}

func (f *panelFake) appliedArgs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.applied...)
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
	f.mu.Lock()
	f.monitors[0].X, f.monitors[0].Y = 1920, 200
	f.mu.Unlock()
	dir := t.TempDir()

	if err := SwitchPanel(f.run, dir); err != nil {
		t.Fatal(err)
	}
	if err := RestorePanel(f.run, dir); err != nil {
		t.Fatal(err)
	}

	applied := f.appliedArgs()
	last := applied[len(applied)-1]
	if !strings.Contains(last, "1920x200") {
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
	if !strings.HasSuffix(applied[len(applied)-1], ",1.6") {
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
	f.mu.Lock()
	f.failNext = true
	f.mu.Unlock()

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
	f.mu.Lock()
	f.failNext = true
	f.mu.Unlock()

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
	f.mu.Lock()
	f.monitors = append(f.monitors, Monitor{Name: OutputExtend, Width: 1920,
		Height: 1080, Refresh: 60, Scale: 1, Focused: true})
	f.monitors[0].Focused = false
	f.mu.Unlock()

	if err := SwitchPanel(f.run, t.TempDir()); err != nil {
		t.Fatal(err)
	}

	applied := f.appliedArgs()
	if len(applied) != 1 || strings.HasPrefix(applied[0], OutputExtend) {
		t.Errorf("switched %v, want a real monitor", applied)
	}
}
