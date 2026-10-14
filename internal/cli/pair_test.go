package cli

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/mrCode/castr/internal/daemon"
	"github.com/mrCode/castr/internal/ui"
)

// A receiver that has never seen this machine shows a code and castr waits for
// it. The bar panel has a field for it -- but a cast started from the menu or
// the command line had nowhere to type it. The receiver asked for a code, a
// banner said to enter it, and nothing anywhere would take it. Pairing never
// finished, so the receiver asked again on the next cast, every time.

func waitingSession() daemon.SessionJSON {
	return daemon.SessionJSON{DeviceID: "airplay:aa:bb", Name: "Living Room TV",
		Mode: "mirror", State: "awaiting_pin"}
}

func pinsSent(h *harness) []daemon.Request {
	return h.daemon.seen(daemon.CmdPin)
}

// Reopening the menu -- the same keybind that started the cast -- is the first
// thing anyone tries. The code entry has to be the first thing they see.
func TestTheMenuOffersTheCodeFirstWhileAReceiverWaits(t *testing.T) {
	h := newHarness(t, "") // cancel at the first prompt
	h.daemon.devices = []daemon.DeviceJSON{tv()}
	h.daemon.sessions = []daemon.SessionJSON{waitingSession()}

	h.app.Run([]string{"menu"})

	if len(h.menu.shown) == 0 {
		t.Fatal("the menu was never shown")
	}
	first := h.menu.shown[0][0]
	if ui.PairingSession(first) != "Living Room TV" {
		t.Errorf("first entry is %q; it should be the code for Living Room TV", first)
	}
}

func TestChoosingTheCodeEntryAsksForTheCodeAndSendsIt(t *testing.T) {
	h := newHarness(t, ui.PairingLine("Living Room TV"), "4821")
	h.daemon.sessions = []daemon.SessionJSON{waitingSession()}

	if code := h.app.Run([]string{"menu"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}

	sent := pinsSent(h)
	if len(sent) != 1 {
		t.Fatalf("sent %d codes, want 1", len(sent))
	}
	if sent[0].DeviceID != "airplay:aa:bb" || sent[0].Pin != "4821" {
		t.Errorf("sent %+v, want airplay:aa:bb / 4821", sent[0])
	}
	// The prompt must say which television to look at: with two receivers in
	// the room, "Code" alone is a guess.
	if len(h.menu.prompts) < 2 || !strings.Contains(h.menu.prompts[1], "Living Room TV") {
		t.Errorf("the code prompt was %q; it should name the receiver", h.menu.prompts)
	}
}

// `castr pin` used to demand a receiver id and the code together, and nothing
// told anyone the id. With no arguments it now finds the receiver that is
// waiting and asks for its code.
func TestCastrPinAloneAsksForTheWaitingReceiversCode(t *testing.T) {
	h := newHarness(t, "4821")
	h.daemon.sessions = []daemon.SessionJSON{waitingSession()}

	if code := h.app.Run([]string{"pin"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	sent := pinsSent(h)
	if len(sent) != 1 || sent[0].DeviceID != "airplay:aa:bb" || sent[0].Pin != "4821" {
		t.Fatalf("sent %+v, want one code for airplay:aa:bb", sent)
	}
}

// What the notification's button runs: the receiver is named, the code is not.
func TestCastrPinWithOnlyAnIdAsksForTheCode(t *testing.T) {
	h := newHarness(t, "4821")
	h.daemon.sessions = []daemon.SessionJSON{waitingSession()}

	if code := h.app.Run([]string{"pin", "airplay:aa:bb"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	if sent := pinsSent(h); len(sent) != 1 || sent[0].Pin != "4821" {
		t.Fatalf("sent %+v, want the typed code", sent)
	}
}

// The original form still works, for scripts and for anyone who knows the id.
func TestCastrPinWithIdAndCodeStillSendsDirectly(t *testing.T) {
	h := newHarness(t)

	if code := h.app.Run([]string{"pin", "airplay:aa:bb", "4821"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	if h.menu.calls != 0 {
		t.Error("a code given on the command line opened a prompt anyway")
	}
	if sent := pinsSent(h); len(sent) != 1 || sent[0].Pin != "4821" {
		t.Fatalf("sent %+v", sent)
	}
}

// Two receivers waiting at once: guessing would send one television's code to
// the other, so the user chooses.
func TestCastrPinAsksWhichReceiverWhenSeveralAreWaiting(t *testing.T) {
	other := waitingSession()
	other.DeviceID, other.Name = "airplay:cc:dd", "Bedroom TV"
	h := newHarness(t, "Bedroom TV", "1357")
	h.daemon.sessions = []daemon.SessionJSON{waitingSession(), other}

	if code := h.app.Run([]string{"pin"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	sent := pinsSent(h)
	if len(sent) != 1 || sent[0].DeviceID != "airplay:cc:dd" || sent[0].Pin != "1357" {
		t.Fatalf("sent %+v, want 1357 for Bedroom TV", sent)
	}
}

func TestCastrPinWithNothingWaitingSaysSo(t *testing.T) {
	h := newHarness(t)
	streaming := waitingSession()
	streaming.State = "streaming"
	h.daemon.sessions = []daemon.SessionJSON{streaming}

	if code := h.app.Run([]string{"pin"}); code == 0 {
		t.Fatal("castr pin succeeded with no receiver waiting for a code")
	}
	if !strings.Contains(h.errOut.String(), "waiting for a code") {
		t.Errorf("message %q does not say nothing is waiting", h.errOut)
	}
	if h.menu.calls != 0 {
		t.Error("a code prompt opened with nothing to send it to")
	}
}

// Closing the prompt is a decision, not an empty code.
func TestCancellingTheCodePromptSendsNothing(t *testing.T) {
	h := newHarness(t, "")
	h.daemon.sessions = []daemon.SessionJSON{waitingSession()}

	if code := h.app.Run([]string{"pin"}); code != 0 {
		t.Fatalf("cancelling exited %d", code)
	}
	if sent := pinsSent(h); len(sent) != 0 {
		t.Fatalf("a cancelled prompt sent %+v", sent)
	}
}

// A code on the command line is readable by every account on the machine --
// /proc/<pid>/cmdline is world-readable, so `ps` shows it. A marketplace
// reviewer flagged exactly that in the bar panel, and doubletake 0.5.0 asks
// for a receiver's configured PASSWORD in the same place, which is a real
// secret. `castr pin <id> -` takes the code on stdin instead, the way Omarchy's
// own network panel passes Wi-Fi passwords.

func TestTheCodeCanArriveOnStdin(t *testing.T) {
	h := newHarness(t)
	h.app.In = strings.NewReader("4821\n")

	if code := h.app.Run([]string{"pin", "airplay:aa:bb", "-"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	sent := pinsSent(h)
	if len(sent) != 1 || sent[0].DeviceID != "airplay:aa:bb" || sent[0].Pin != "4821" {
		t.Fatalf("sent %+v, want 4821 for airplay:aa:bb", sent)
	}
	if h.menu.calls != 0 {
		t.Error("a code given on stdin opened a prompt anyway")
	}
}

// The panel writes the code and a newline, but a script might not.
func TestAStdinCodeWithoutANewlineIsAccepted(t *testing.T) {
	h := newHarness(t)
	h.app.In = strings.NewReader("hunter2")

	if code := h.app.Run([]string{"pin", "airplay:aa:bb", "-"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	if sent := pinsSent(h); len(sent) != 1 || sent[0].Pin != "hunter2" {
		t.Fatalf("sent %+v", sent)
	}
}

// Every castr before this one sends a literal "-" to the television as the
// code. That must never happen here: an empty stdin is an error, not a code.
func TestDashIsNeverSentAsACode(t *testing.T) {
	for _, in := range []string{"", "\n", "   \n"} {
		h := newHarness(t)
		h.app.In = strings.NewReader(in)

		if code := h.app.Run([]string{"pin", "airplay:aa:bb", "-"}); code == 0 {
			t.Errorf("stdin %q: succeeded with no code", in)
		}
		for _, r := range pinsSent(h) {
			t.Errorf("stdin %q: sent %q to the receiver", in, r.Pin)
		}
	}
}

// A code or password must not turn up in an error message, which lands in
// logs, terminals and the panel.
func TestAStdinCodeIsNeverEchoedBack(t *testing.T) {
	h := newHarness(t)
	h.daemon.failWith[daemon.CmdPin] = "receiver rejected the credential"
	h.app.In = strings.NewReader("correct-horse-battery\n")

	h.app.Run([]string{"pin", "airplay:aa:bb", "-"})

	for _, out := range []string{h.out.String(), h.errOut.String()} {
		if strings.Contains(out, "correct-horse-battery") {
			t.Fatalf("the secret was printed: %q", out)
		}
	}
}

// A code is a few characters and a password a few dozen. Reading without a
// limit lets whatever is on the other end of stdin grow this process forever.
func TestAnEndlessStdinIsRefused(t *testing.T) {
	h := newHarness(t)
	h.app.In = strings.NewReader(strings.Repeat("x", 1<<20))

	if code := h.app.Run([]string{"pin", "airplay:aa:bb", "-"}); code == 0 {
		t.Fatal("a megabyte on stdin was accepted as a code")
	}
	if sent := pinsSent(h); len(sent) != 0 {
		t.Fatalf("sent %d bytes as a code", len(sent[0].Pin))
	}
}

// The code is written to doubletake's stdin, a line at a time. A second line
// smuggled in here would arrive as the answer to doubletake's NEXT prompt.
func TestOnlyTheFirstLineOfStdinIsSent(t *testing.T) {
	h := newHarness(t)
	h.app.In = strings.NewReader("4821\nsomething-else\n")

	if code := h.app.Run([]string{"pin", "airplay:aa:bb", "-"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	if sent := pinsSent(h); len(sent) != 1 || sent[0].Pin != "4821" {
		t.Fatalf("sent %+v, want exactly the first line", sent)
	}
}

// The panel writes the code and a newline and leaves stdin open, as Omarchy's
// own network panel does with Wi-Fi passwords. Reading until end-of-file would
// wait forever for an end that never comes, and the panel would sit on
// "sending" with the receiver still showing its code.
func TestTheCodeIsReadWithoutWaitingForStdinToClose(t *testing.T) {
	h := newHarness(t)
	r, w := io.Pipe()
	defer w.Close()
	h.app.In = r
	go w.Write([]byte("4821\n")) // and never closed

	done := make(chan int, 1)
	go func() { done <- h.app.Run([]string{"pin", "airplay:aa:bb", "-"}) }()

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d: %s", code, h.errOut)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("castr pin waited for stdin to close after the code arrived")
	}
	if sent := pinsSent(h); len(sent) != 1 || sent[0].Pin != "4821" {
		t.Fatalf("sent %+v", sent)
	}
}
