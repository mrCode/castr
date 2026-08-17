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
