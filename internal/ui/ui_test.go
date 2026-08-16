package ui

import (
	"strings"
	"testing"

	"github.com/mrCode/castr/internal/daemon"
	"github.com/mrCode/castr/internal/session"
)

func device(id, name, model string) daemon.DeviceJSON {
	return daemon.DeviceJSON{ID: id, Name: name, Model: model,
		Address: "10.0.0.5", Port: 7000, Protocol: "airplay"}
}

func cast(id, name, mode, state string) daemon.SessionJSON {
	return daemon.SessionJSON{DeviceID: id, Name: name, Mode: mode, State: state}
}

func TestAnIdWithColonsSurvivesTheRoundTrip(t *testing.T) {
	// AirPlay ids embed a MAC address, so anything that splits on ":" finds
	// the wrong thing. This is the whole reason the pattern anchors on the
	// trailing brackets.
	id := "airplay:aa:bb:cc:dd:ee:ff"
	entries := Entries([]daemon.DeviceJSON{device(id, "Meeting Room", "AppleTV11,1")}, nil)

	got := ParseSelection(entries[0])

	if got != id {
		t.Errorf("parsed %q from %q, want %q", got, entries[0], id)
	}
}

func TestAReceiverEntryShowsWhatItIs(t *testing.T) {
	entries := Entries([]daemon.DeviceJSON{device("airplay:aa", "Meeting Room", "AppleTV11,1")}, nil)

	line := entries[0]
	for _, want := range []string{"Meeting Room", "AirPlay", "AppleTV11,1"} {
		if !strings.Contains(line, want) {
			t.Errorf("entry %q does not mention %q", line, want)
		}
	}
}

func TestAReceiverWithNoModelGetsNoEmptySeparator(t *testing.T) {
	entries := Entries([]daemon.DeviceJSON{device("airplay:aa", "TV", "")}, nil)

	if strings.Contains(entries[0], "·") {
		t.Errorf("entry %q has a separator with nothing after it", entries[0])
	}
}

func TestStoppingIsTheFirstThingOfferedWhileCasting(t *testing.T) {
	// Right-clicking the bar indicator is undiscoverable, and one user
	// reported stopping was impossible when it was merely invisible.
	entries := Entries(
		[]daemon.DeviceJSON{device("airplay:aa", "Meeting Room", "")},
		[]daemon.SessionJSON{cast("airplay:aa", "Meeting Room", session.ModeMirror, "streaming")})

	if !strings.HasPrefix(entries[0], StopEntry) {
		t.Errorf("first entry = %q, want the stop entry", entries[0])
	}
	if got := StoppingSession(entries[0]); got != "Meeting Room" {
		t.Errorf("StoppingSession(%q) = %q, want the receiver name", entries[0], got)
	}
}

func TestAReceiverLineIsNotMistakenForAStopLine(t *testing.T) {
	entries := Entries([]daemon.DeviceJSON{device("airplay:aa", "TV", "")}, nil)

	if got := StoppingSession(entries[0]); got != "" {
		t.Errorf("StoppingSession(%q) = %q, want nothing", entries[0], got)
	}
}

func TestTheManualEscapeHatchIsAlwaysOffered(t *testing.T) {
	// Some receivers answer AirPlay and never answer mDNS at all.
	entries := Entries(nil, nil)

	if len(entries) != 1 || entries[0] != ManualEntry {
		t.Errorf("entries = %v, want the manual entry even with nothing discovered", entries)
	}
}

func TestReceiversAreListedInAStableOrder(t *testing.T) {
	// The menu must not reshuffle between invocations, or muscle memory picks
	// the wrong receiver.
	devices := []daemon.DeviceJSON{
		device("airplay:c", "zulu", ""), device("airplay:a", "Alpha", ""),
		device("airplay:b", "mike", ""),
	}
	for i := 0; i < 10; i++ {
		entries := Entries(devices, nil)
		if !strings.HasPrefix(entries[0], "Alpha") ||
			!strings.HasPrefix(entries[1], "mike") ||
			!strings.HasPrefix(entries[2], "zulu") {
			t.Fatalf("order = %v, want case-insensitive alphabetical every time", entries)
		}
	}
}

func TestBothModesAreOfferedAndParseBack(t *testing.T) {
	if len(ModeEntries) != 2 {
		t.Fatalf("mode entries = %v", ModeEntries)
	}
	if got := ParseMode(MirrorEntry); got != session.ModeMirror {
		t.Errorf("ParseMode(mirror) = %q", got)
	}
	if got := ParseMode(ExtendEntry); got != session.ModeExtend {
		t.Errorf("ParseMode(extend) = %q", got)
	}
	if got := ParseMode("something else"); got != "" {
		t.Errorf("ParseMode(other) = %q, want nothing", got)
	}
}

func TestTheExtendEntryNamesTheOutputToPickAtThePortal(t *testing.T) {
	// Picking the wrong output at the portal prompt silently produces a
	// mirror, and the portal REMEMBERS it for every later cast.
	if !strings.Contains(ExtendEntry, "castr") {
		t.Errorf("extend entry = %q, want it to name the output", ExtendEntry)
	}
}

// --- the bar indicator ---

func TestTheIndicatorStaysVisibleWhenIdle(t *testing.T) {
	// Hiding it makes it impossible to find, which defeats an indicator you
	// are supposed to click.
	got := Render(nil)

	if got.Text == "" {
		t.Error("nothing is rendered when idle")
	}
	if got.Class != "idle" {
		t.Errorf("class = %q, want idle", got.Class)
	}
	if !strings.Contains(got.Tooltip, HintIdle) {
		t.Errorf("tooltip = %q, want the click hint", got.Tooltip)
	}
}

func TestAStreamingCastNamesItsReceiverAndMode(t *testing.T) {
	got := Render([]daemon.SessionJSON{cast("a", "Meeting Room", session.ModeExtend, "streaming")})

	if got.Class != "streaming" {
		t.Errorf("class = %q", got.Class)
	}
	for _, want := range []string{"Meeting Room", session.ModeExtend} {
		if !strings.Contains(got.Tooltip, want) {
			t.Errorf("tooltip %q does not mention %q", got.Tooltip, want)
		}
	}
}

func TestAFailureOutranksEverythingElse(t *testing.T) {
	// It is the only state the user has to act on.
	got := Render([]daemon.SessionJSON{
		cast("a", "One", session.ModeMirror, "streaming"),
		{DeviceID: "b", Name: "Two", Mode: session.ModeMirror,
			State: string(session.Failed), Error: "no route to host"},
	})

	if got.Class != "failed" {
		t.Errorf("class = %q, want failed", got.Class)
	}
	if !strings.Contains(got.Tooltip, "no route to host") {
		t.Errorf("tooltip = %q, want the reason", got.Tooltip)
	}
}

func TestAFailureWithNoReasonStillSaysSomething(t *testing.T) {
	got := Render([]daemon.SessionJSON{
		{DeviceID: "a", Name: "TV", State: string(session.Failed)}})

	if strings.Contains(got.Tooltip, "Cast failed: \n") {
		t.Errorf("tooltip = %q, want a placeholder rather than a blank reason", got.Tooltip)
	}
}

func TestAwaitingAPinTellsTheUserToLookAtTheReceiver(t *testing.T) {
	// The PIN is on the television, not on this screen, and nothing else says so.
	got := Render([]daemon.SessionJSON{
		cast("a", "Meeting Room", session.ModeMirror, string(session.AwaitingPin))})

	if got.Class != "connecting" {
		t.Errorf("class = %q, want connecting", got.Class)
	}
	if !strings.Contains(strings.ToLower(got.Tooltip), "pin") {
		t.Errorf("tooltip = %q, want it to mention the PIN", got.Tooltip)
	}
}

func TestConnectingIsDistinguishedFromStreaming(t *testing.T) {
	got := Render([]daemon.SessionJSON{
		cast("a", "TV", session.ModeMirror, string(session.Connecting))})

	if got.Class != "connecting" {
		t.Errorf("class = %q, want connecting so the bar can colour it", got.Class)
	}
}

func TestTwoCastsAreCounted(t *testing.T) {
	got := Render([]daemon.SessionJSON{
		cast("a", "One", session.ModeMirror, "streaming"),
		cast("b", "Two", session.ModeExtend, "streaming"),
	})

	if !strings.Contains(got.Text, "2") {
		t.Errorf("text = %q, want the count", got.Text)
	}
	if !strings.Contains(got.Tooltip, "One") || !strings.Contains(got.Tooltip, "Two") {
		t.Errorf("tooltip = %q, want both receivers", got.Tooltip)
	}
}

func TestASessionWithNoModeDoesNotRenderEmptyParentheses(t *testing.T) {
	got := Render([]daemon.SessionJSON{cast("a", "TV", "", "streaming")})

	if strings.Contains(got.Tooltip, "()") {
		t.Errorf("tooltip = %q, want no empty parentheses", got.Tooltip)
	}
}

func TestTheModeLabelIsStableWithTwoModes(t *testing.T) {
	for i := 0; i < 10; i++ {
		got := Render([]daemon.SessionJSON{
			cast("a", "One", session.ModeMirror, "streaming"),
			cast("b", "Two", session.ModeExtend, "streaming"),
		})
		if !strings.Contains(got.Tooltip, "(extend/mirror)") {
			t.Fatalf("tooltip = %q, want a stable mode label", got.Tooltip)
		}
	}
}
