// Package picker shows the graphical menu, whichever one this desktop provides.
//
// Omarchy replaced walker with a Quickshell-based menu, so `walker --dmenu`
// stopped existing and `castr menu` would have died with "executable file not
// found" -- the cast keybind simply broken after a system update. The launcher
// is not something to hardcode.
//
// Two backends, probed in order:
//
//	omarchy-menu-select / omarchy-menu-input   Omarchy's own menu
//	walker --dmenu                             earlier Omarchy, and other setups
//
// They differ in more than name: walker reads its options from stdin, Omarchy's
// takes them as arguments. Both return the chosen line on stdout.
package picker

import (
	"fmt"
	"strings"
)

// The programs castr knows how to drive.
const (
	OmarchySelect = "omarchy-menu-select"
	OmarchyInput  = "omarchy-menu-input"
	Walker        = "walker"
)

// Kind identifies which menu program is present.
type Kind string

const (
	// KindNone means the desktop provides neither.
	KindNone    Kind = ""
	KindOmarchy Kind = "omarchy"
	KindWalker  Kind = "walker"
)

// LookPath reports whether a program exists, as exec.LookPath does.
type LookPath func(name string) (string, error)

// Run executes a menu program and returns its stdout.
type Run func(argv []string, stdin string) (string, error)

// Picker drives whichever menu is installed. Both effects are injected: a test
// that opened a real omarchy-menu-input dialog hung the whole suite until
// somebody noticed the window and clicked it.
type Picker struct {
	Look LookPath
	Exec Run
}

// Kind reports which menu program this desktop provides.
func (p Picker) Kind() Kind {
	if _, err := p.Look(OmarchySelect); err == nil {
		return KindOmarchy
	}
	if _, err := p.Look(Walker); err == nil {
		return KindWalker
	}
	return KindNone
}

// ErrNoMenu means neither menu program is installed.
var ErrNoMenu = fmt.Errorf("no menu program found")

// Pick shows entries and returns the chosen one, or "" if the user cancelled.
//
// Cancelling is not an error: pressing Escape is a normal way to leave a menu,
// and reporting it as a failure put an error banner on screen every time.
func (p Picker) Pick(prompt string, entries []string) (string, error) {
	switch p.Kind() {
	case KindOmarchy:
		// Options are ARGUMENTS here, not stdin.
		argv := append([]string{OmarchySelect, prompt}, entries...)
		return p.output(argv, "")
	case KindWalker:
		return p.output([]string{Walker, "--dmenu", "-p", prompt}, strings.Join(entries, "\n"))
	default:
		return "", ErrNoMenu
	}
}

// Ask prompts for free text -- an address to connect to directly.
func (p Picker) Ask(prompt string) (string, error) {
	switch p.Kind() {
	case KindOmarchy:
		return p.output([]string{OmarchyInput, prompt}, "")
	case KindWalker:
		// walker's dmenu with no options doubles as a text prompt.
		return p.output([]string{Walker, "--dmenu", "-p", prompt}, "")
	default:
		return "", ErrNoMenu
	}
}

func (p Picker) output(argv []string, stdin string) (string, error) {
	out, err := p.Exec(argv, stdin)
	if err != nil {
		// A non-zero exit is how every one of these reports "cancelled", which
		// is indistinguishable from a crash at this level. Treated as a
		// cancellation: the cost of being wrong is a menu that closes, and
		// the alternative was an error banner on every Escape.
		return "", nil
	}
	return strings.TrimSpace(out), nil
}

// MissingMessage tells the user what to install, and what works without it.
func MissingMessage() string {
	return fmt.Sprintf(
		"no menu program found. castr looks for %s (Omarchy) or %s. "+
			"Install one, or use the CLI: castr list / castr start <id>",
		OmarchySelect, Walker)
}
