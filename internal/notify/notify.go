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
	"fmt"
	"strconv"

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

// Notifier decides what is worth telling the user, and how loudly.
type Notifier struct {
	Run Runner
}

// Argv builds the command for one message.
func Argv(message string, urgent bool) []string {
	argv := []string{Binary, "-a", App, "-h", SynchronousHint}
	if urgent {
		// Deliberately sticky, and deliberately rare.
		argv = append(argv, "-u", "critical")
	} else {
		argv = append(argv, "-u", "normal", "-t", strconv.Itoa(ExpireMS))
	}
	return append(argv, App, message)
}

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
	n.Send(message, urgent)
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
		// The PIN is on the television and nothing else on this screen says so.
		return fmt.Sprintf("%s: enter the PIN shown on the receiver", device.Name), false, true

	default:
		// Connecting, streaming, stopping, idle: all visible on the receiver
		// or on the bar, and none of them need a banner.
		return "", false, false
	}
}
