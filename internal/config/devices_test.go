package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrCode/castr/internal/discovery"
)

func meetingRoom() discovery.Device {
	return discovery.Device{ID: "meeting", Name: "Meeting Room", Address: "10.10.10.231",
		Port: 7000, Protocol: discovery.ProtocolAirPlay, Model: "AppleTV11,1"}
}

func TestAManualReceiverSurvivesADaemonRestart(t *testing.T) {
	// The whole reason this file exists: the daemon exits when idle, and the
	// receivers that need a hand-typed address are precisely the ones
	// discovery will never start finding on its own.
	path := DevicesPath(t.TempDir())
	if err := SaveDevices(path, []discovery.Device{meetingRoom()}); err != nil {
		t.Fatal(err)
	}

	got := LoadDevices(path)

	if len(got) != 1 {
		t.Fatalf("loaded %d devices, want 1", len(got))
	}
	if got[0] != meetingRoom() {
		t.Errorf("got  = %+v\nwant = %+v", got[0], meetingRoom())
	}
}

func TestAMissingStoreIsNotAnError(t *testing.T) {
	if got := LoadDevices(filepath.Join(t.TempDir(), "absent.json")); got != nil {
		t.Errorf("got %v, want no devices from a store that does not exist", got)
	}
}

func TestACorruptStoreIsIgnoredRatherThanFatal(t *testing.T) {
	// It is a convenience feature. Taking the daemon down over it trades a
	// forgotten address for no casting at all.
	path := DevicesPath(t.TempDir())
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := LoadDevices(path); got != nil {
		t.Errorf("got %v, want nothing from a corrupt store", got)
	}
}

func TestEntriesWithNothingToCastToAreSkipped(t *testing.T) {
	path := DevicesPath(t.TempDir())
	body := `[{"id":"good","name":"Good","address":"10.0.0.5"},
	          {"id":"","address":"10.0.0.6"},
	          {"id":"no-address","name":"Nowhere"}]`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	got := LoadDevices(path)

	if len(got) != 1 || got[0].ID != "good" {
		t.Errorf("got %+v, want only the entry that can actually be cast to", got)
	}
}

func TestASparseEntryGetsWorkableDefaults(t *testing.T) {
	// The user typed an address into a menu prompt. Everything else is ours
	// to fill in, and an entry with port 0 can never connect.
	path := DevicesPath(t.TempDir())
	if err := os.WriteFile(path, []byte(`[{"id":"tv","address":"10.0.0.5"}]`), 0o600); err != nil {
		t.Fatal(err)
	}

	got := LoadDevices(path)

	if len(got) != 1 {
		t.Fatalf("got %d devices", len(got))
	}
	if got[0].Port != 7000 {
		t.Errorf("port = %d, want the AirPlay default", got[0].Port)
	}
	if got[0].Protocol != discovery.ProtocolAirPlay {
		t.Errorf("protocol = %q, want airplay", got[0].Protocol)
	}
	if got[0].Name != "tv" {
		t.Errorf("name = %q, want it to fall back to the id so the menu shows something", got[0].Name)
	}
}

func TestSavingReplacesRatherThanAppends(t *testing.T) {
	path := DevicesPath(t.TempDir())
	if err := SaveDevices(path, []discovery.Device{meetingRoom()}); err != nil {
		t.Fatal(err)
	}

	other := discovery.Device{ID: "other", Name: "Other", Address: "10.0.0.9",
		Port: 7000, Protocol: discovery.ProtocolAirPlay}
	if err := SaveDevices(path, []discovery.Device{other}); err != nil {
		t.Fatal(err)
	}

	got := LoadDevices(path)
	if len(got) != 1 || got[0].ID != "other" {
		t.Errorf("got %+v, want only the receivers from the second save", got)
	}
}

func TestSavingLeavesNoTemporaryFilesBehind(t *testing.T) {
	// The write is atomic via a temp file and a rename. A leftover temp file
	// per save would fill the state directory over a daemon's lifetime.
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		if err := SaveDevices(DevicesPath(dir), []discovery.Device{meetingRoom()}); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".manual-devices-") {
			t.Errorf("left a temporary file behind: %s", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Errorf("state directory holds %d files, want just the store", len(entries))
	}
}

func TestTheStoreIsNotReadableByOtherUsers(t *testing.T) {
	path := DevicesPath(t.TempDir())
	if err := SaveDevices(path, []discovery.Device{meetingRoom()}); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("mode = %v, want no group or other access", perm)
	}
}

// --- migration ---

type dirs struct{ config, state, legacyConfig, legacyState string }

func newDirs(t *testing.T) dirs {
	t.Helper()
	root := t.TempDir()
	d := dirs{
		config:       filepath.Join(root, "config", "castr"),
		state:        filepath.Join(root, "state", "castr"),
		legacyConfig: filepath.Join(root, "config", LegacyName),
		legacyState:  filepath.Join(root, "state", LegacyName),
	}
	for _, p := range []string{d.legacyConfig, d.legacyState} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func TestAnUpgradeKeepsTheOldConfigAndReceivers(t *testing.T) {
	d := newDirs(t)
	body := "[airplay]\ntarget_latency_ms = 50\n"
	if err := os.WriteFile(filepath.Join(d.legacyConfig, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveDevices(DevicesPath(d.legacyState), []discovery.Device{meetingRoom()}); err != nil {
		t.Fatal(err)
	}

	copied, err := Migrate(d.config, d.state, d.legacyConfig, d.legacyState)
	if err != nil {
		t.Fatal(err)
	}

	if len(copied) != 2 {
		t.Errorf("copied %v, want both the config and the receivers", copied)
	}
	cfg, err := Load(filepath.Join(d.config, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AirPlay.TargetLatencyMS != 50 {
		t.Errorf("target_latency_ms = %d, want the user's own setting carried over",
			cfg.AirPlay.TargetLatencyMS)
	}
	if got := LoadDevices(DevicesPath(d.state)); len(got) != 1 || got[0].ID != "meeting" {
		t.Errorf("receivers = %+v, want the hand-typed one carried over", got)
	}
}

func TestMigrationLeavesTheOldInstallWorking(t *testing.T) {
	// omarchy-cast is still installed and still works while castr proves
	// itself. Moving its state would break a working tool to set up an
	// unproven one.
	d := newDirs(t)
	legacy := filepath.Join(d.legacyConfig, "config.toml")
	if err := os.WriteFile(legacy, []byte("[capture]\nfps = 24\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Migrate(d.config, d.state, d.legacyConfig, d.legacyState); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(legacy); err != nil {
		t.Errorf("the old config is gone: %v", err)
	}
}

func TestMigrationNeverOverwritesAConfigCastrAlreadyHas(t *testing.T) {
	// Running it on every start is fine; clobbering the user's own settings
	// with a stale legacy file on every start is not.
	d := newDirs(t)
	if err := os.MkdirAll(d.config, 0o700); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(d.config, "config.toml")
	if err := os.WriteFile(mine, []byte("[capture]\nfps = 60\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.legacyConfig, "config.toml"),
		[]byte("[capture]\nfps = 24\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	copied, err := Migrate(d.config, d.state, d.legacyConfig, d.legacyState)
	if err != nil {
		t.Fatal(err)
	}

	if len(copied) != 0 {
		t.Errorf("copied %v over an existing castr config", copied)
	}
	cfg, err := Load(mine)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Capture.FPS != 60 {
		t.Errorf("fps = %d, want the user's own castr setting untouched", cfg.Capture.FPS)
	}
}

func TestAFreshInstallHasNothingToMigrate(t *testing.T) {
	d := newDirs(t)

	copied, err := Migrate(d.config, d.state, d.legacyConfig, d.legacyState)

	if err != nil {
		t.Fatalf("a fresh install failed to start: %v", err)
	}
	if len(copied) != 0 {
		t.Errorf("copied %v with no legacy install present", copied)
	}
}

func TestAFailedSaveLeavesTheExistingStoreIntact(t *testing.T) {
	// This is what atomic-write-then-rename buys. Writing the target directly
	// truncates it first, so a save that fails halfway loses exactly the
	// addresses that cannot be rediscovered.
	dir := t.TempDir()
	path := DevicesPath(dir)
	if err := SaveDevices(path, []discovery.Device{meetingRoom()}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil { // no new files may be created
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	other := discovery.Device{ID: "other", Name: "Other", Address: "10.0.0.9",
		Port: 7000, Protocol: discovery.ProtocolAirPlay}
	if err := SaveDevices(path, []discovery.Device{other}); err == nil {
		t.Skip("this user can create files in a mode-500 directory; cannot test the failure")
	}

	if got := LoadDevices(path); len(got) != 1 || got[0].ID != "meeting" {
		t.Errorf("store = %+v, want the previous contents untouched by a failed save", got)
	}
}

func TestAFailedRenameDoesNotStrandATemporaryFile(t *testing.T) {
	// The cleanup only matters on the failure paths -- on success the rename
	// has already moved the file away. Without one provoked failure, deleting
	// the cleanup entirely goes unnoticed.
	dir := t.TempDir()
	path := DevicesPath(dir)
	if err := os.Mkdir(path, 0o700); err != nil { // a rename onto a directory fails
		t.Fatal(err)
	}

	if err := SaveDevices(path, []discovery.Device{meetingRoom()}); err == nil {
		t.Fatal("saving over a directory reported success")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".manual-devices-") {
			t.Errorf("a failed save stranded %s", e.Name())
		}
	}
}

// --- credentials: pairing keys are shared, portal tokens never are ---

func putCreds(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readCredsMap(t *testing.T, path string) map[string]map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out map[string]map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}
	return out
}

const pairedTV = `{"AA:BB:CC:DD:EE:FF":{"pairing_id":"id-1","ed25519_public":"pub-1",` +
	`"ed25519_seed":"seed-1","restore_token":"token-for-mirror"}}`

func TestPairingKeysAreSharedBetweenModes(t *testing.T) {
	// Otherwise the user walks to the television and retypes a code the first
	// time they extend to a receiver they already mirror to.
	dir := t.TempDir()
	putCreds(t, CredsPath(dir, "mirror"), pairedTV)

	if err := SyncPairing(dir, []string{"mirror", "extend"}); err != nil {
		t.Fatal(err)
	}

	got := readCredsMap(t, CredsPath(dir, "extend"))
	entry, ok := got["AA:BB:CC:DD:EE:FF"]
	if !ok {
		t.Fatal("extend never learned the receiver")
	}
	if entry["ed25519_seed"] != "seed-1" {
		t.Errorf("seed = %v, want the pairing key carried across", entry["ed25519_seed"])
	}
}

func TestAPortalTokenIsNeverSharedBetweenModes(t *testing.T) {
	// The token grants capture of a specific OUTPUT. Mirror and extend capture
	// different outputs, so replaying mirror's grant while extending casts the
	// wrong screen -- and nothing anywhere reports it.
	dir := t.TempDir()
	putCreds(t, CredsPath(dir, "mirror"), pairedTV)

	if err := SyncPairing(dir, []string{"mirror", "extend"}); err != nil {
		t.Fatal(err)
	}

	entry := readCredsMap(t, CredsPath(dir, "extend"))["AA:BB:CC:DD:EE:FF"]
	if _, leaked := entry["restore_token"]; leaked {
		t.Error("mirror's portal token leaked into extend; extend would capture mirror's output")
	}
}

func TestSyncingDoesNotDisturbAModesOwnToken(t *testing.T) {
	dir := t.TempDir()
	putCreds(t, CredsPath(dir, "mirror"), pairedTV)
	putCreds(t, CredsPath(dir, "extend"),
		`{"AA:BB:CC:DD:EE:FF":{"pairing_id":"id-1","ed25519_public":"pub-1",`+
			`"ed25519_seed":"seed-1","restore_token":"token-for-extend"}}`)

	if err := SyncPairing(dir, []string{"mirror", "extend"}); err != nil {
		t.Fatal(err)
	}

	entry := readCredsMap(t, CredsPath(dir, "extend"))["AA:BB:CC:DD:EE:FF"]
	if entry["restore_token"] != "token-for-extend" {
		t.Errorf("extend's own token = %v, want it untouched", entry["restore_token"])
	}
}

func TestMigrationCarriesPairingsButNotTheOldApplicationsToken(t *testing.T) {
	// omarchy-cast's token grants capture of an output named for omarchy-cast,
	// which castr never creates. Replaying it captured the wrong screen.
	d := newDirs(t)
	putCreds(t, filepath.Join(d.legacyState, LegacyCredsFilenames["mirror"]), pairedTV)

	if _, err := Migrate(d.config, d.state, d.legacyConfig, d.legacyState); err != nil {
		t.Fatal(err)
	}

	entry := readCredsMap(t, CredsPath(d.state, "mirror"))["AA:BB:CC:DD:EE:FF"]
	if entry == nil {
		t.Fatal("the pairing was not carried over")
	}
	if entry["ed25519_seed"] != "seed-1" {
		t.Errorf("seed = %v, want the pairing carried over", entry["ed25519_seed"])
	}
	if _, leaked := entry["restore_token"]; leaked {
		t.Error("the old application's portal token was carried over")
	}
}

func TestForgettingAShareChoiceKeepsThePairing(t *testing.T) {
	// Re-picking which output to share must not mean walking to the television
	// and retyping a code. Those are different things stored in one file.
	dir := t.TempDir()
	putCreds(t, CredsPath(dir, "mirror"), pairedTV)

	cleared, err := ClearRestoreTokens(dir, []string{"mirror", "extend"})
	if err != nil {
		t.Fatal(err)
	}

	if len(cleared) != 1 {
		t.Errorf("cleared = %v, want the one receiver that had a grant", cleared)
	}
	entry := readCredsMap(t, CredsPath(dir, "mirror"))["AA:BB:CC:DD:EE:FF"]
	if _, ok := entry["restore_token"]; ok {
		t.Error("the share choice survived; the prompt would never appear again")
	}
	if entry["ed25519_seed"] != "seed-1" {
		t.Errorf("seed = %v, want the pairing untouched", entry["ed25519_seed"])
	}
}

func TestForgettingOneModeLeavesTheOtherAlone(t *testing.T) {
	// Mirror and extend capture different outputs. Getting mirror wrong is no
	// reason to make the user re-pick extend as well.
	dir := t.TempDir()
	putCreds(t, CredsPath(dir, "mirror"), pairedTV)
	putCreds(t, CredsPath(dir, "extend"),
		`{"AA:BB:CC:DD:EE:FF":{"pairing_id":"id-1","restore_token":"token-for-extend"}}`)

	if _, err := ClearRestoreTokens(dir, []string{"mirror"}); err != nil {
		t.Fatal(err)
	}

	if _, ok := readCredsMap(t, CredsPath(dir, "mirror"))["AA:BB:CC:DD:EE:FF"]["restore_token"]; ok {
		t.Error("mirror's grant survived")
	}
	if readCredsMap(t, CredsPath(dir, "extend"))["AA:BB:CC:DD:EE:FF"]["restore_token"] != "token-for-extend" {
		t.Error("extend's grant was cleared too")
	}
}

func TestForgettingNothingIsNotAnError(t *testing.T) {
	cleared, err := ClearRestoreTokens(t.TempDir(), []string{"mirror", "extend"})

	if err != nil {
		t.Fatalf("err = %v, want a quiet no-op", err)
	}
	if len(cleared) != 0 {
		t.Errorf("cleared = %v from an empty state directory", cleared)
	}
}
