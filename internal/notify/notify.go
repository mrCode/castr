// Package notify shows desktop notifications.
//
// The daemon and the CLI both used to have their own copy of this, and both
// sent every message at critical urgency. On mako a critical notification never
// expires, so an afternoon of casting left a column of banners to click away
// one by one -- and a single failed start produced two of them, because the
// daemon notified the failed transition while the CLI separately notified the
// error that the very same failure returned to it.
//
// Two rules keep that from happening again:
//
//   - Only something the user must ACT on is urgent. Urgent means sticky, and
//     sticky is a cost paid by the user, not by us.
//   - Every notification carries mako's synchronous hint, so a new one replaces
//     the previous instead of stacking. Casting is one ongoing activity; its
//     status belongs in one banner, not a queue.
package notify

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/mrCode/castr/internal/discovery"
	"github.com/mrCode/castr/internal/session"
)

// App names us to the notification daemon.
const App = "castr"

// Binary is the program that shows notifications.
const Binary = "notify-send"

// SynchronousHint makes mako (and swaync) replace rather than stack when it
// matches. Other daemons ignore it harmlessly.
var SynchronousHint = "string:x-canonical-private-synchronous:" + App

// ExpireMS is long enough to read a device name, short enough not to sit in
// the corner.
const ExpireMS = 5000

// Runner executes notify-send. Injected: no test puts a banner on the screen.
type Runner func(argv []string) error

// Prompter runs notify-send and returns what it printed: the name of the
// action the user chose, or nothing if the banner was dismissed or expired.
type Prompter func(argv []string) (string, error)

// The actions a pairing banner registers. "default" is what clicking a banner
// invokes on most daemons; PairAction is a labelled button for daemons that
// draw buttons. Either one opens the code prompt.
const (
	DefaultAction = "default"
	PairAction    = "pair"
)

// ExecArgvHint is Omarchy's click-to-run hint: a JSON argv its shell executes,
// without a shell, when the banner is clicked. Omarchy draws no action buttons
// and invokes only "default" -- and only while the sender is still running --
// so this hint is the path that works there, and it keeps working after the
// sender has gone, even across a shell restart.
const ExecArgvHint = "omarchy-exec-argv"

// Notifier decides what is worth telling the user, and how loudly.
type Notifier struct {
	Run Runner

	// PairCommand is the command that asks for a receiver's pairing code --
	// `castr pin <id>`. The pairing banner runs it when clicked. Unset, the
	// banner still tells the user where the code goes, but cannot take them
	// there.
	PairCommand func(device discovery.Device) []string

	// Prompt and Start make the pairing banner clickable on daemons other
	// than Omarchy's: Prompt waits for the banner to be answered, and Start
	// runs PairCommand if it was clicked.
	Prompt Prompter
	Start  func(argv []string)
}

// Argv builds the command for one message.
func Argv(message string, urgent bool) []string {
	return argv(message, urgent, nil)
}

// PairArgv builds a pairing banner that runs command when clicked.
//
// It carries the command two ways, for two kinds of notification daemon: as
// Omarchy's exec hint, and as actions that make notify-send print the choice
// -- --wait keeps notify-send running until the banner is answered, so that it
// can.
func PairArgv(message string, command []string) []string {
	extra := append(execHint(command),
		"--action="+DefaultAction+"=Enter code",
		"--action="+PairAction+"=Enter code",
		"--wait")
	return argv(message, true, extra)
}

// pairArgvNoWait is the pairing banner for when nothing can wait for a click:
// the Omarchy hint alone. Actions with no one listening do nothing -- and
// --wait with no one listening holds whoever sent the banner until it closes,
// which for the daemon means every cast stalls behind an unanswered prompt.
func pairArgvNoWait(message string, command []string) []string {
	return argv(message, true, execHint(command))
}

// execHint carries command in Omarchy's exec hint.
func execHint(command []string) []string {
	if len(command) == 0 {
		return nil
	}
	hint, err := json.Marshal(command)
	if err != nil {
		return nil
	}
	return []string{"-h", "string:" + ExecArgvHint + ":" + string(hint)}
}

func argv(message string, urgent bool, extra []string) []string {
	a := []string{Binary, "-a", App, "-h", SynchronousHint}
	if urgent {
		// Deliberately sticky, and deliberately rare.
		a = append(a, "-u", "critical")
	} else {
		a = append(a, "-u", "normal", "-t", strconv.Itoa(ExpireMS))
	}
	a = append(a, extra...)
	return append(a, App, Escape(message))
}

// Escape makes text safe to use as a notification body.
//
// Bodies are rendered as markup, hyperlinks included, by common notification
// daemons -- and castr's messages carry receiver names, which are whatever a
// device on the network chooses to advertise. Unescaped, anything nearby could
// put a clickable link in a castr banner by naming itself `<a href=...>`. This
// is the notification's version of the rich-text bug a marketplace reviewer
// found in the bar panel.
func Escape(s string) string {
	return markup.Replace(s)
}

var markup = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// Send shows a message. Best effort: a machine with no notification daemon
// must still cast.
func (n Notifier) Send(message string, urgent bool) {
	if n.Run == nil {
		return
	}
	_ = n.Run(Argv(message, urgent))
}

// OnState is the daemon's Notify hook.
//
// Most state changes are NOT announced. The user pressed a key and is watching
// the screen; narrating "connecting", then "streaming", then "idle" at them is
// the notification flood this project already shipped once.
func (n Notifier) OnState(device discovery.Device, state session.State, reason string) {
	message, urgent, worth := Message(device, state, reason)
	if !worth {
		return
	}
	if state == session.AwaitingPin && n.PairCommand != nil {
		n.pairBanner(device, message)
		return
	}
	n.Send(message, urgent)
}

// pairBanner shows a banner that, clicked, asks for the receiver's code.
func (n Notifier) pairBanner(device discovery.Device, message string) {
	command := n.PairCommand(device)

	if n.Prompt == nil || n.Start == nil {
		// No way to wait for a click. Omarchy needs none -- the hint runs the
		// command itself -- so the banner is still worth sending, without the
		// parts that only make sense with something waiting.
		if n.Run != nil {
			_ = n.Run(pairArgvNoWait(message, command))
		}
		return
	}
	banner := PairArgv(message, command)

	// Not waited for. The banner stays up until it is answered, which can be
	// minutes, and the daemon has casts to run in the meantime.
	go func() {
		out, err := n.Prompt(banner)
		if err != nil {
			return
		}
		// On Omarchy the hint has already run the command and the banner was
		// dismissed, so notify-send prints nothing here -- which is what keeps
		// a click from opening two prompts.
		switch strings.TrimSpace(out) {
		case DefaultAction, PairAction:
			n.Start(command)
		}
	}()
}

// Message decides what to say about a state change, and whether to say
// anything at all.
//
// The returned bool is the whole point: it is false for every state the user
// can already see the result of.
func Message(device discovery.Device, state session.State, reason string) (string, bool, bool) {
	switch state {
	case session.Failed:
		if reason == "" {
			reason = "unknown error"
		}
		// Urgent because a cast that dies is exactly the thing the user is NOT
		// watching for -- they had already turned back to their work.
		return fmt.Sprintf("Casting to %s failed: %s", device.Name, reason), true, true

	case session.AwaitingPin:
		// The code is on the television and nothing else on this screen says
		// so. Urgent, because the cast is held until the user types it -- and
		// the banner says WHERE, because "enter the PIN" with nowhere to enter
		// it left a receiver that asked on every cast, pairing never finished.
		return fmt.Sprintf("Enter the pairing code shown on %s: open the cast menu, "+
			"or run castr pin", device.Name), true, true

	default:
		// Connecting, streaming, stopping, idle: all visible on the receiver
		// or on the bar, and none of them need a banner.
		return "", false, false
	}
}
