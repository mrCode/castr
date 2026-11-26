package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/mrCode/castr/internal/config"
	"github.com/mrCode/castr/internal/daemon"
)

func delayHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.app.StateDir = t.TempDir()
	return h
}

func casting(id, name string) daemon.SessionJSON {
	return daemon.SessionJSON{DeviceID: id, Name: name, Mode: "mirror", State: "streaming"}
}

func TestDelaySetsAndReadsAReceiversOffset(t *testing.T) {
	h := delayHarness(t)

	if code := h.app.Run([]string{"delay", "airplay:aa:bb", "-120"}); code != 0 {
		t.Fatalf("set: exit %d: %s", code, h.errOut)
	}
	if !strings.Contains(h.out.String(), "video 120 ms later") {
		t.Errorf("set did not say what it did:\n%s", h.out)
	}

	h.out.Reset()
	if code := h.app.Run([]string{"delay", "airplay:aa:bb"}); code != 0 {
		t.Fatalf("read: exit %d: %s", code, h.errOut)
	}
	if got := strings.TrimSpace(h.out.String()); got != "-120" {
		t.Fatalf("read printed %q, want the bare number -120 the bar parses", got)
	}

	// castr-airplay reads exactly this file.
	data, err := os.ReadFile(config.AVOffsetPath(h.app.StateDir, "airplay:aa:bb"))
	if err != nil || strings.TrimSpace(string(data)) != "-120" {
		t.Fatalf("offset file = %q, %v; want -120", data, err)
	}
}

func TestDelayWithOnlyANumberSetsTheOneCast(t *testing.T) {
	h := delayHarness(t)
	h.daemon.sessions = []daemon.SessionJSON{casting("airplay:aa:bb", "Meeting Room")}

	if code := h.app.Run([]string{"delay", "+80"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	if ms, _ := config.ReadAVOffset(h.app.StateDir, "airplay:aa:bb"); ms != 80 {
		t.Fatalf("offset %d, want 80", ms)
	}
	if !strings.Contains(h.out.String(), "Meeting Room") || !strings.Contains(h.out.String(), "audio 80 ms later") {
		t.Errorf("output:\n%s", h.out)
	}
}

func TestDelayWithOnlyANumberRefusesWhenItCannotTellWhichCast(t *testing.T) {
	h := delayHarness(t)
	h.daemon.sessions = []daemon.SessionJSON{casting("airplay:aa:bb", "One"), casting("airplay:cc:dd", "Two")}

	if code := h.app.Run([]string{"delay", "50"}); code == 0 {
		t.Fatal("set an offset without knowing which of two casts")
	}
	for _, id := range []string{"airplay:aa:bb", "airplay:cc:dd"} {
		if ms, _ := config.ReadAVOffset(h.app.StateDir, id); ms != 0 {
			t.Errorf("%s changed to %d", id, ms)
		}
	}
}

func TestDelayListsEveryCast(t *testing.T) {
	h := delayHarness(t)
	h.daemon.sessions = []daemon.SessionJSON{casting("airplay:aa:bb", "One"), casting("airplay:cc:dd", "Two")}
	if err := config.WriteAVOffset(h.app.StateDir, "airplay:aa:bb", 150); err != nil {
		t.Fatal(err)
	}

	if code := h.app.Run([]string{"delay"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	out := h.out.String()
	if !strings.Contains(out, "One  +150 ms (audio 150 ms later)") || !strings.Contains(out, "Two  0 ms (no correction)") {
		t.Errorf("listing:\n%s", out)
	}
}

func TestDelayIsLimitedAndRejectsNonsense(t *testing.T) {
	h := delayHarness(t)

	if code := h.app.Run([]string{"delay", "airplay:aa:bb", "5000"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	if ms, _ := config.ReadAVOffset(h.app.StateDir, "airplay:aa:bb"); ms != config.AVOffsetLimitMS {
		t.Fatalf("offset %d, want the limit %d", ms, config.AVOffsetLimitMS)
	}
	if !strings.Contains(h.out.String(), "limited") {
		t.Errorf("did not say the value was limited:\n%s", h.out)
	}

	if code := h.app.Run([]string{"delay", "airplay:aa:bb", "soon"}); code == 0 {
		t.Fatal("accepted a delay that is not a number")
	}
}

func TestADelayOfZeroRemovesTheFile(t *testing.T) {
	h := delayHarness(t)
	path := config.AVOffsetPath(h.app.StateDir, "airplay:aa:bb")

	h.app.Run([]string{"delay", "airplay:aa:bb", "60"})
	if code := h.app.Run([]string{"delay", "airplay:aa:bb", "0"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("offset file still there after reset: %v", err)
	}
}
