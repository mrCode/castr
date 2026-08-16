package cli

import (
	"strings"
	"testing"

	"github.com/mrCode/castr/internal/daemon"
	"github.com/mrCode/castr/internal/session"
	"github.com/mrCode/castr/internal/ui"
)

func TestTheMenuCastsWhatWasPicked(t *testing.T) {
	h := newHarness(t,
		"Meeting Room (AirPlay · AppleTV11,1) [airplay:aa:bb]",
		ui.MirrorEntry)
	h.daemon.devices = []daemon.DeviceJSON{tv()}

	if code := h.app.Run([]string{"menu"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}

	got := h.daemon.seen(daemon.CmdStart)
	if len(got) != 1 {
		t.Fatalf("starts = %d", len(got))
	}
	if got[0].DeviceID != "airplay:aa:bb" {
		t.Errorf("device = %q, want the id parsed out of the chosen line", got[0].DeviceID)
	}
	if got[0].Mode != session.ModeMirror {
		t.Errorf("mode = %q", got[0].Mode)
	}
}

func TestTheMenuAsksForAModeAfterTheReceiver(t *testing.T) {
	h := newHarness(t,
		"Meeting Room (AirPlay) [airplay:aa:bb]",
		ui.ExtendEntry)
	h.daemon.devices = []daemon.DeviceJSON{tv()}

	h.app.Run([]string{"menu"})

	if h.menu.calls != 2 {
		t.Fatalf("menu shown %d times, want a receiver prompt then a mode prompt", h.menu.calls)
	}
	if got := h.daemon.seen(daemon.CmdStart); len(got) != 1 || got[0].Mode != session.ModeExtend {
		t.Errorf("start = %+v, want extend", got)
	}
}

func TestCancellingTheReceiverPromptCastsNothing(t *testing.T) {
	// Escape is a normal way to leave a menu.
	h := newHarness(t, "")
	h.daemon.devices = []daemon.DeviceJSON{tv()}

	if code := h.app.Run([]string{"menu"}); code != 0 {
		t.Errorf("exit %d, want cancelling to be silent", code)
	}
	if len(h.daemon.seen(daemon.CmdStart)) != 0 {
		t.Error("cancelling started a cast")
	}
	if h.errOut.Len() != 0 {
		t.Errorf("stderr = %q, want nothing on a cancel", h.errOut)
	}
}

func TestCancellingTheModePromptCastsNothing(t *testing.T) {
	h := newHarness(t, "Meeting Room (AirPlay) [airplay:aa:bb]", "")
	h.daemon.devices = []daemon.DeviceJSON{tv()}

	if code := h.app.Run([]string{"menu"}); code != 0 {
		t.Errorf("exit %d", code)
	}
	if len(h.daemon.seen(daemon.CmdStart)) != 0 {
		t.Error("cancelling the mode prompt still started a cast")
	}
}

func TestALiveCastCanBeStoppedFromTheMenu(t *testing.T) {
	// Right-clicking the bar is undiscoverable; one user reported stopping was
	// impossible when it was merely invisible.
	h := newHarness(t, ui.StopEntry+" (Meeting Room)")
	h.daemon.devices = []daemon.DeviceJSON{tv()}
	h.daemon.sessions = []daemon.SessionJSON{
		{DeviceID: "airplay:aa:bb", Name: "Meeting Room", Mode: "mirror", State: "streaming"}}

	if code := h.app.Run([]string{"menu"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}

	got := h.daemon.seen(daemon.CmdStop)
	if len(got) != 1 || got[0].DeviceID != "airplay:aa:bb" {
		t.Errorf("stops = %+v, want the live session stopped", got)
	}
	if len(h.daemon.seen(daemon.CmdStart)) != 0 {
		t.Error("choosing stop started a cast instead")
	}
}

func TestStoppingIsOfferedFirstSoItIsAlwaysReachable(t *testing.T) {
	h := newHarness(t, "")
	h.daemon.devices = []daemon.DeviceJSON{tv()}
	h.daemon.sessions = []daemon.SessionJSON{
		{DeviceID: "airplay:aa:bb", Name: "Meeting Room", State: "streaming"}}

	h.app.Run([]string{"menu"})

	if len(h.menu.shown) == 0 || len(h.menu.shown[0]) == 0 {
		t.Fatal("nothing was shown")
	}
	if !strings.HasPrefix(h.menu.shown[0][0], ui.StopEntry) {
		t.Errorf("first entry = %q, want the stop entry", h.menu.shown[0][0])
	}
}

func TestACastThatEndedWhileTheMenuWasOpenIsNotAnError(t *testing.T) {
	// The user opens the menu, the receiver drops the connection, the user
	// then picks Stop. Complaining about an id they never saw helps nobody.
	h := newHarness(t, ui.StopEntry+" (Gone)")
	h.daemon.devices = []daemon.DeviceJSON{tv()}

	if code := h.app.Run([]string{"menu"}); code != 0 {
		t.Errorf("exit %d, want this handled quietly", code)
	}
	if !strings.Contains(h.out.String(), "already stopped") {
		t.Errorf("output = %q", h.out)
	}
}

func TestTheManualEntryPromptsForAnAddressAndCastsToIt(t *testing.T) {
	// The escape hatch for receivers mDNS cannot see -- an Apple TV that
	// served AirPlay and answered no mDNS query at all.
	h := newHarness(t, ui.ManualEntry, "10.10.10.231", ui.MirrorEntry)

	if code := h.app.Run([]string{"menu"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}

	added := h.daemon.seen(daemon.CmdAdd)
	if len(added) != 1 || added[0].Device.Address != "10.10.10.231" {
		t.Fatalf("adds = %+v, want the typed address registered", added)
	}
	started := h.daemon.seen(daemon.CmdStart)
	if len(started) != 1 || started[0].DeviceID != added[0].Device.ID {
		t.Errorf("start = %+v, want it to cast to the receiver just added", started)
	}
}

func TestAnEmptyAddressCancelsRatherThanRegisteringNothing(t *testing.T) {
	h := newHarness(t, ui.ManualEntry, "")

	if code := h.app.Run([]string{"menu"}); code != 0 {
		t.Errorf("exit %d, want cancelling to be silent", code)
	}
	if len(h.daemon.seen(daemon.CmdAdd)) != 0 {
		t.Error("registered a receiver with no address")
	}
}

func TestNoMenuProgramIsReportedWithAWayOut(t *testing.T) {
	// The Quarto update removed walker; hardcoding it turned a system update
	// into a cast keybind that died with "executable file not found".
	h := newHarness(t)
	h.menu.installed = false

	code := h.app.Run([]string{"menu"})

	if code == 0 {
		t.Error("exit 0 with no menu program")
	}
	msg := h.errOut.String()
	if !strings.Contains(msg, "castr list") {
		t.Errorf("stderr = %q, want it to point at the CLI", msg)
	}
}

func TestAMenuLineWithNoIdIsReportedRatherThanSilentlyIgnored(t *testing.T) {
	h := newHarness(t, "something the menu invented")
	h.daemon.devices = []daemon.DeviceJSON{tv()}

	code := h.app.Run([]string{"menu"})

	if code == 0 {
		t.Error("exit 0 for a line no receiver matches")
	}
}

func TestTheMenuStillWorksWithNothingDiscovered(t *testing.T) {
	// A network with no receivers must still offer the manual entry, or the
	// escape hatch is unreachable exactly when it is needed.
	h := newHarness(t, "")

	if code := h.app.Run([]string{"menu"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}

	if len(h.menu.shown) == 0 {
		t.Fatal("no menu was shown")
	}
	last := h.menu.shown[0][len(h.menu.shown[0])-1]
	if last != ui.ManualEntry {
		t.Errorf("entries = %v, want the manual entry offered", h.menu.shown[0])
	}
}
