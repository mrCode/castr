package cli

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/mrCode/castr/internal/daemon"
	"github.com/mrCode/castr/internal/picker"
	"github.com/mrCode/castr/internal/session"
)

// Pairing is the one step of a cast that needs the user to type something, and
// for a long time castr only took it on the command line, with both the
// receiver's id and the code spelled out: `castr pin <id> <code>`. Nothing told
// anyone the id. A cast started from the menu reached a receiver that wanted
// pairing, the television showed a code, a banner said to enter it, and nowhere
// would accept it -- so pairing never completed, and the receiver asked again on
// every single cast.
//
// Everything here exists to give that code somewhere to go, from wherever the
// cast was started: the menu's first entry, `castr pin` on its own, and the
// button on the pairing notification, which runs `castr pin <id>`.

// pin sends a pairing code. With both arguments it sends them as given; with
// fewer it finds the receiver that is waiting and asks for its code.
func (a *App) pin(args []string) int {
	switch len(args) {
	case 0:
		return a.pinWaiting("")
	case 1:
		return a.pinWaiting(args[0])
	default:
		if args[1] == StdinCode {
			return a.pinFromStdin(args[0])
		}
		return a.run(func() error { return a.Client.SubmitPin(args[0], args[1]) })
	}
}

// StdinCode in place of a code reads the code from stdin.
//
// A command line is readable by every account on the machine --
// /proc/<pid>/cmdline is world-readable, so `ps` shows it -- and doubletake
// 0.5.0 asks for a receiver's configured PASSWORD in the same place it asks
// for a pairing code. A marketplace reviewer flagged the bar panel for passing
// it as an argument. The panel now writes it to stdin, the way Omarchy's own
// network panel passes Wi-Fi passwords.
const StdinCode = "-"

// maxCodeBytes bounds a code read from stdin. A pairing code is a few
// characters and a receiver password a few dozen; reading without a limit lets
// whatever holds the other end grow this process without bound.
const maxCodeBytes = 1024

func (a *App) pinFromStdin(deviceID string) int {
	if a.In == nil {
		return a.fail("no code on stdin")
	}
	// One line, not everything until end-of-file. The panel writes the code
	// and a newline and leaves stdin open, as Omarchy's own network panel does
	// with Wi-Fi passwords; waiting for an end that never comes would leave
	// the panel on "sending" while the receiver still shows its code. Reading
	// stops at the first newline, so a second line never reaches doubletake.
	line, err := bufio.NewReader(io.LimitReader(a.In, maxCodeBytes+1)).ReadString('\n')
	if err != nil && err != io.EOF {
		return a.fail("reading the code from stdin: " + err.Error())
	}
	if len(line) > maxCodeBytes {
		// Deliberately not echoed: it may be a password.
		return a.fail("the code on stdin is too long")
	}
	code := strings.TrimSpace(line)
	if code == "" {
		// Never sent as "-". Every castr before this one did exactly that,
		// and an empty code is an error, not a code.
		return a.fail("no code on stdin")
	}
	return a.run(func() error { return a.Client.SubmitPin(deviceID, code) })
}

// pinWaiting asks for the code of the receiver that is waiting for one --
// deviceID's, if given.
func (a *App) pinWaiting(deviceID string) int {
	sessions, err := a.Client.Sessions()
	if err != nil {
		return a.fail(err.Error())
	}

	var waiting []daemon.SessionJSON
	for _, s := range sessions {
		if s.State != string(session.AwaitingPin) {
			continue
		}
		if deviceID == "" || s.DeviceID == deviceID {
			waiting = append(waiting, s)
		}
	}

	switch len(waiting) {
	case 0:
		if deviceID != "" {
			return a.fail(fmt.Sprintf("%s is not waiting for a code", deviceID))
		}
		return a.fail("no receiver is waiting for a code")
	case 1:
		return a.askForCode(waiting[0])
	}

	// More than one: guessing would send one television's code to another.
	names := make([]string, len(waiting))
	for i, s := range waiting {
		names[i] = s.Name
	}
	choice, err := a.Picker.Pick("Which receiver is showing the code?", names)
	if err != nil {
		return a.fail(err.Error())
	}
	for _, s := range waiting {
		if s.Name == strings.TrimSpace(choice) {
			return a.askForCode(s)
		}
	}
	return 0 // cancelled
}

// askForCode prompts for the code a receiver is showing, and sends it.
func (a *App) askForCode(s daemon.SessionJSON) int {
	if a.Picker.Kind() == picker.KindNone {
		// No menu program to prompt with -- so say exactly what to type
		// instead, id and all, which is the thing nobody could find out.
		return a.fail(fmt.Sprintf("%s\nOr send the code yourself: castr pin %s <code>",
			picker.MissingMessage(), s.DeviceID))
	}

	// Named, because with two receivers in the room "Code" alone is a guess.
	code, err := a.Picker.Ask("Code shown on " + s.Name)
	if err != nil {
		return a.fail(err.Error())
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return 0 // closing the prompt is a decision, not an empty code
	}
	return a.run(func() error { return a.Client.SubmitPin(s.DeviceID, code) })
}
