package cli

import (
	"strings"
	"testing"

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
