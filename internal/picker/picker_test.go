package picker

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// fakeDesktop models which programs exist and what the menu returns.
type fakeDesktop struct {
	installed map[string]bool
	reply     string
	err       error

	argv  []string
	stdin string
	calls int
}

func (d *fakeDesktop) look(name string) (string, error) {
	if d.installed[name] {
		return "/usr/bin/" + name, nil
	}
	return "", exec.ErrNotFound
}

func (d *fakeDesktop) run(argv []string, stdin string) (string, error) {
	d.calls++
	d.argv = argv
	d.stdin = stdin
	return d.reply, d.err
}

func newPicker(reply string, installed ...string) (Picker, *fakeDesktop) {
	d := &fakeDesktop{installed: map[string]bool{}, reply: reply}
	for _, name := range installed {
		d.installed[name] = true
	}
	return Picker{Look: d.look, Exec: d.run}, d
}

func TestOmarchysMenuIsPreferredWhenPresent(t *testing.T) {
	p, _ := newPicker("", OmarchySelect, Walker)

	if got := p.Kind(); got != KindOmarchy {
		t.Errorf("kind = %q, want omarchy", got)
	}
}

func TestWalkerIsUsedOnDesktopsWithoutOmarchysMenu(t *testing.T) {
	// Earlier Omarchy, and every non-Omarchy setup.
	p, _ := newPicker("", Walker)

	if got := p.Kind(); got != KindWalker {
		t.Errorf("kind = %q, want walker", got)
	}
}

func TestNeitherMenuIsReportedRatherThanCrashing(t *testing.T) {
	// The Quarto update removed walker. Hardcoding it turned a system update
	// into a broken cast keybind that died with "executable file not found".
	p, _ := newPicker("")

	_, err := p.Pick("Cast to", []string{"a", "b"})

	if !errors.Is(err, ErrNoMenu) {
		t.Errorf("err = %v, want ErrNoMenu", err)
	}
}

func TestOmarchyReceivesItsOptionsAsArguments(t *testing.T) {
	// The two menus differ in more than name. Feeding Omarchy's menu on stdin
	// shows an empty list.
	p, d := newPicker("Living Room [airplay:aa]", OmarchySelect)

	if _, err := p.Pick("Cast to", []string{"Living Room [airplay:aa]", "Kitchen [airplay:bb]"}); err != nil {
		t.Fatal(err)
	}

	want := []string{OmarchySelect, "Cast to", "Living Room [airplay:aa]", "Kitchen [airplay:bb]"}
	if strings.Join(d.argv, "|") != strings.Join(want, "|") {
		t.Errorf("argv = %v\nwant = %v", d.argv, want)
	}
	if d.stdin != "" {
		t.Errorf("stdin = %q, want the options passed as arguments only", d.stdin)
	}
}

func TestWalkerReceivesItsOptionsOnStdin(t *testing.T) {
	p, d := newPicker("Kitchen", Walker)

	if _, err := p.Pick("Cast to", []string{"Living Room", "Kitchen"}); err != nil {
		t.Fatal(err)
	}

	if d.stdin != "Living Room\nKitchen" {
		t.Errorf("stdin = %q, want newline-separated options", d.stdin)
	}
	joined := strings.Join(d.argv, " ")
	if !strings.Contains(joined, "--dmenu") {
		t.Errorf("argv = %v, want walker in dmenu mode", d.argv)
	}
	if strings.Contains(joined, "Kitchen") {
		t.Errorf("argv = %v, want the options on stdin, not as arguments", d.argv)
	}
}

func TestTheChosenLineComesBackTrimmed(t *testing.T) {
	// Both menus add a trailing newline, and the id parser anchors on the end
	// of the line.
	p, _ := newPicker("  Living Room [airplay:aa:bb]  \n", OmarchySelect)

	got, err := p.Pick("Cast to", []string{"Living Room [airplay:aa:bb]"})

	if err != nil {
		t.Fatal(err)
	}
	if got != "Living Room [airplay:aa:bb]" {
		t.Errorf("got %q, want it trimmed", got)
	}
}

func TestCancellingIsNotAnError(t *testing.T) {
	// Escape is a normal way to leave a menu. Every one of these programs
	// reports it as a non-zero exit, and treating that as a failure put an
	// error banner on screen every single time.
	p, d := newPicker("", OmarchySelect)
	d.err = errors.New("exit status 1")

	got, err := p.Pick("Cast to", []string{"a"})

	if err != nil {
		t.Errorf("err = %v, want cancelling to be silent", err)
	}
	if got != "" {
		t.Errorf("got %q, want nothing chosen", got)
	}
}

func TestAskUsesTheInputProgramNotTheSelector(t *testing.T) {
	// omarchy-menu-select with no options shows an empty list the user cannot
	// type into.
	p, d := newPicker("10.10.10.231", OmarchySelect)

	got, err := p.Ask("Receiver address")

	if err != nil {
		t.Fatal(err)
	}
	if got != "10.10.10.231" {
		t.Errorf("got %q", got)
	}
	if d.argv[0] != OmarchyInput {
		t.Errorf("ran %q, want %q", d.argv[0], OmarchyInput)
	}
}

func TestAskFallsBackToWalkersEmptyDmenu(t *testing.T) {
	p, d := newPicker("10.0.0.5", Walker)

	if _, err := p.Ask("Receiver address"); err != nil {
		t.Fatal(err)
	}

	if d.stdin != "" {
		t.Errorf("stdin = %q, want an empty option list so walker takes free text", d.stdin)
	}
}

func TestTheMissingMenuMessageNamesBothProgramsAndAWayOut(t *testing.T) {
	// It is the only thing a user with neither installed will see.
	msg := MissingMessage()

	for _, want := range []string{OmarchySelect, Walker, "castr list", "castr start"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message does not mention %q: %s", want, msg)
		}
	}
}
