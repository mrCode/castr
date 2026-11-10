package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrCode/castr/internal/config"
	"github.com/mrCode/castr/internal/daemon"
)

// pairedState puts a pairing with the office TV, by device ID and address, in
// both modes' files under a fresh XDG_STATE_HOME.
func pairedState(t *testing.T) string {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := daemon.StateDir()
	body := `{"FE:5C:81:22:50:38":{"pairing_id":"id-1","ed25519_public":"pub-1",` +
		`"ed25519_seed":"seed-1","receiver_address":"10.10.10.231","restore_token":"tok"}}`
	for _, mode := range []string{"mirror", "extend"} {
		path := config.CredsPath(dir, mode)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func pairedIn(t *testing.T, dir, mode string) bool {
	t.Helper()
	raw, err := os.ReadFile(config.CredsPath(dir, mode))
	if err != nil {
		t.Fatal(err)
	}
	var creds map[string]map[string]any
	if err := json.Unmarshal(raw, &creds); err != nil {
		t.Fatal(err)
	}
	_, ok := creds["FE:5C:81:22:50:38"]["ed25519_seed"]
	return ok
}

func TestUnpairForgetsTheReceiverInBothModes(t *testing.T) {
	// By the id castr lists, by the id of a receiver added by address, and
	// by the bare address.
	for _, id := range []string{"airplay:FE:5C:81:22:50:38", "airplay:manual:10.10.10.231", "10.10.10.231"} {
		dir := pairedState(t)
		h := newHarness(t)

		if code := h.app.Run([]string{"unpair", id}); code != 0 {
			t.Fatalf("%s: exit %d: %s", id, code, h.errOut)
		}
		if pairedIn(t, dir, "mirror") || pairedIn(t, dir, "extend") {
			t.Errorf("%s: the pairing survived", id)
		}
		// What happens next has to be said: the next cast asks for a code,
		// and a receiver that shows none is not the user's.
		if out := h.out.String(); !strings.Contains(out, "code") || !strings.Contains(out, "shows no code") {
			t.Errorf("%s: output does not say what happens next: %s", id, out)
		}
	}
}

func TestUnpairStopsEveryCastFirst(t *testing.T) {
	// A running helper rewrites its file from memory, and would put the
	// pairing straight back -- whichever receiver it is casting to.
	dir := pairedState(t)
	h := newHarness(t)
	h.daemon.sessions = []daemon.SessionJSON{{DeviceID: "airplay:42:56:35:AF:76:0F", Name: "Other"}}

	if code := h.app.Run([]string{"unpair", "airplay:FE:5C:81:22:50:38"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	if stops := h.daemon.seen(daemon.CmdStop); len(stops) != 1 || stops[0].DeviceID != "airplay:42:56:35:AF:76:0F" {
		t.Errorf("stops = %v, want the running cast stopped", stops)
	}
	if pairedIn(t, dir, "mirror") {
		t.Error("the pairing survived")
	}
}

func TestUnpairingAnUnknownReceiverSaysSo(t *testing.T) {
	dir := pairedState(t)
	h := newHarness(t)

	if code := h.app.Run([]string{"unpair", "airplay:11:22:33:44:55:66"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	if !strings.Contains(h.out.String(), "no pairing") {
		t.Errorf("output = %q, want it to say there was nothing to forget", h.out)
	}
	if !pairedIn(t, dir, "mirror") {
		t.Error("another receiver's pairing was forgotten")
	}
}

func TestUnpairNeedsAnAirPlayReceiver(t *testing.T) {
	pairedState(t)
	for _, args := range [][]string{{"unpair"}, {"unpair", "chromecast:8a30"}} {
		h := newHarness(t)
		if code := h.app.Run(args); code == 0 {
			t.Errorf("%v succeeded", args)
		}
		if len(h.daemon.seen(daemon.CmdStop)) != 0 {
			t.Errorf("%v stopped casts before refusing", args)
		}
	}
}
