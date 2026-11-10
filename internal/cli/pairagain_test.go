package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrCode/castr/internal/config"
	"github.com/mrCode/castr/internal/daemon"
)

// pairedState puts a PIN pairing with the office TV, bound to its address,
// in the mirror file under a fresh XDG_STATE_HOME.
func pairedState(t *testing.T) string {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := daemon.StateDir()
	body := `{"FE:5C:81:22:50:38":{"pairing_id":"id-1","ed25519_public":"pub-1",` +
		`"ed25519_seed":"seed-1","pairing_code":"pin","receiver_address":"10.10.10.231"}}`
	path := config.CredsPath(dir, "mirror")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestPairAgainMarksTheReceiver(t *testing.T) {
	// By the id castr lists, by the id of a receiver added by address, and
	// by the bare address.
	for _, id := range []string{"airplay:FE:5C:81:22:50:38", "airplay:manual:10.10.10.231", "10.10.10.231"} {
		dir := pairedState(t)
		h := newHarness(t)

		if code := h.app.Run([]string{"pair-again", id}); code != 0 {
			t.Fatalf("%s: exit %d: %s", id, code, h.errOut)
		}
		if _, ok := config.PairAgainFor(dir, "FE:5C:81:22:50:38", ""); !ok {
			t.Errorf("%s: the receiver was not marked", id)
		}
		// What happens next has to be said, and when not to type a code.
		if out := h.out.String(); !strings.Contains(out, "shows none") {
			t.Errorf("%s: output lacks the warning: %s", id, out)
		}
	}
}

func TestPairAgainStopsNothing(t *testing.T) {
	// Only castr's own mark is written; a running cast cannot undo it.
	pairedState(t)
	h := newHarness(t)
	h.daemon.sessions = []daemon.SessionJSON{{DeviceID: "airplay:42:56:35:AF:76:0F", Name: "Other"}}

	if code := h.app.Run([]string{"pair-again", "airplay:FE:5C:81:22:50:38"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	if len(h.daemon.seen(daemon.CmdStop)) != 0 {
		t.Error("a cast was stopped")
	}
}

func TestPairAgainRefusesWhatItCannotPairAgain(t *testing.T) {
	pairedState(t)
	for _, args := range [][]string{{"pair-again"}, {"pair-again", "chromecast:8a30"},
		{"pair-again", "airplay:11:22:33:44:55:66"}} {
		h := newHarness(t)
		if code := h.app.Run(args); code == 0 {
			t.Errorf("%v succeeded: %s", args, h.out)
		}
	}
}
