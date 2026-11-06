package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	body := "[capture]\nencoder = \"vaapi\"\n\n[airplay]\ntarget_latency_ms = 50\n"
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
	// Loaded with a note, not an error: the real omarchy-cast config on the
	// machine this was ported from carried target_latency_ms = 50, which is
	// below the floor and gets raised. Everything else must survive.
	cfg, note := Load(filepath.Join(d.config, "config.toml"))
	if cfg.AirPlay.TargetLatencyMS < MinSafeLatencyMS {
		t.Errorf("target_latency_ms = %d, want it raised on migration",
			cfg.AirPlay.TargetLatencyMS)
	}
	if note == nil {
		t.Error("the value was changed silently; the user should be told")
	}
	if cfg.Capture.Encoder != "vaapi" {
		t.Errorf("encoder = %q, want the rest of the migrated file kept", cfg.Capture.Encoder)
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

// doubletake 0.5.0 added a fourth part to a pairing: pairing_protocol, the
// pair-verify protocol ("hap" or "raw") it was negotiated with. Without it,
// doubletake guesses from the receiver's capabilities -- the behaviour of 0.4.x,
// and wrong for some receivers, which is why 0.5.0 started recording it. A
// wrong guess fails verification, and doubletake answers that by pairing from
// scratch: the receiver shows a code again. So the protocol is part of the
// pairing and has to travel with the keys.

const pairedTVWithProtocol = `{"AA:BB:CC:DD:EE:FF":{"pairing_id":"id-1","ed25519_public":"pub-1",` +
	`"ed25519_seed":"seed-1","pairing_protocol":"raw","restore_token":"token-for-mirror"}}`

func TestThePairingProtocolTravelsWithTheKeys(t *testing.T) {
	dir := t.TempDir()
	putCreds(t, CredsPath(dir, "mirror"), pairedTVWithProtocol)

	if err := SyncPairing(dir, []string{"mirror", "extend"}); err != nil {
		t.Fatal(err)
	}

	entry := readCredsMap(t, CredsPath(dir, "extend"))["AA:BB:CC:DD:EE:FF"]
	if entry["pairing_protocol"] != "raw" {
		t.Errorf("extend got pairing_protocol %v, want raw; it would guess, and a wrong "+
			"guess makes the receiver ask for a code again", entry["pairing_protocol"])
	}
}

// The worse case. extend paired once before, under one protocol; mirror has
// since paired again, under another. Copying mirror's new keys while leaving
// extend's old protocol beside them describes a pairing that never existed.
func TestNewKeysNeverSitBesideAnOldPairingsProtocol(t *testing.T) {
	dir := t.TempDir()
	putCreds(t, CredsPath(dir, "extend"), `{"AA:BB:CC:DD:EE:FF":{"pairing_id":"old",`+
		`"ed25519_public":"old-pub","ed25519_seed":"old-seed","pairing_protocol":"hap"}}`)
	putCreds(t, CredsPath(dir, "mirror"), pairedTVWithProtocol)

	if _, err := MergePairing(CredsPath(dir, "mirror"), CredsPath(dir, "extend")); err != nil {
		t.Fatal(err)
	}

	entry := readCredsMap(t, CredsPath(dir, "extend"))["AA:BB:CC:DD:EE:FF"]
	if entry["ed25519_seed"] != "seed-1" || entry["pairing_protocol"] != "raw" {
		t.Errorf("extend holds seed=%v protocol=%v; want mirror's whole pairing, seed-1/raw",
			entry["ed25519_seed"], entry["pairing_protocol"])
	}
}

// A pairing written by doubletake 0.4.x has no protocol at all. Copying its
// keys must REMOVE any protocol the destination had, so doubletake guesses for
// these keys exactly as it does in the file they came from -- not trusts a
// protocol recorded for some other pairing.
func TestAPairingWithNoProtocolClearsAStaleOne(t *testing.T) {
	dir := t.TempDir()
	putCreds(t, CredsPath(dir, "extend"), `{"AA:BB:CC:DD:EE:FF":{"pairing_id":"old",`+
		`"ed25519_public":"old-pub","ed25519_seed":"old-seed","pairing_protocol":"hap"}}`)
	putCreds(t, CredsPath(dir, "mirror"), pairedTV) // no pairing_protocol

	if _, err := MergePairing(CredsPath(dir, "mirror"), CredsPath(dir, "extend")); err != nil {
		t.Fatal(err)
	}

	entry := readCredsMap(t, CredsPath(dir, "extend"))["AA:BB:CC:DD:EE:FF"]
	if p, stale := entry["pairing_protocol"]; stale {
		t.Errorf("extend kept protocol %v beside keys it was never negotiated for", p)
	}
	if entry["ed25519_seed"] != "seed-1" {
		t.Errorf("extend's seed = %v, want the copied pairing", entry["ed25519_seed"])
	}
}

// A doubletake that authenticates receivers records the receiver's own
// identity with the pairing, and checks every later connection against it.
// Left behind, every mode but the one that paired would ask for a code again.
const pairedTVWithIdentity = `{"AA:BB:CC:DD:EE:FF":{"pairing_id":"id-1","ed25519_public":"pub-1",` +
	`"ed25519_seed":"seed-1","pairing_protocol":"hap","receiver_id":"tv-id",` +
	`"receiver_ed25519_public":"tv-key","restore_token":"token-for-mirror"}}`

func TestTheReceiversIdentityTravelsWithThePairing(t *testing.T) {
	dir := t.TempDir()
	putCreds(t, CredsPath(dir, "mirror"), pairedTVWithIdentity)

	if err := SyncPairing(dir, []string{"mirror", "extend"}); err != nil {
		t.Fatal(err)
	}

	entry := readCredsMap(t, CredsPath(dir, "extend"))["AA:BB:CC:DD:EE:FF"]
	if entry["receiver_id"] != "tv-id" || entry["receiver_ed25519_public"] != "tv-key" {
		t.Errorf("extend got receiver %v / %v, want tv-id / tv-key",
			entry["receiver_id"], entry["receiver_ed25519_public"])
	}
}

// castr-airplay also records when a pairing was made, what kind of code made
// it, and the address it was made at. The address is what binds a receiver
// added by address, which has no device ID castr could name; the time is how
// two modes' pairings with the same receiver are told apart. All of it
// describes the pairing, so it travels with the keys.
const pairedTVWithHistory = `{"AA:BB:CC:DD:EE:FF":{"pairing_id":"id-1","ed25519_public":"pub-1",` +
	`"ed25519_seed":"seed-1","pairing_protocol":"hap","receiver_id":"tv-id",` +
	`"receiver_ed25519_public":"tv-key","paired_at":"2026-11-01T10:00:00Z",` +
	`"pairing_code":"pin","receiver_address":"10.0.0.5","restore_token":"token-for-mirror"}}`

func TestWhenWhereAndHowAReceiverWasPairedTravelWithThePairing(t *testing.T) {
	dir := t.TempDir()
	putCreds(t, CredsPath(dir, "mirror"), pairedTVWithHistory)

	if err := SyncPairing(dir, []string{"mirror", "extend"}); err != nil {
		t.Fatal(err)
	}

	entry := readCredsMap(t, CredsPath(dir, "extend"))["AA:BB:CC:DD:EE:FF"]
	for field, want := range map[string]string{"paired_at": "2026-11-01T10:00:00Z",
		"pairing_code": "pin", "receiver_address": "10.0.0.5"} {
		if entry[field] != want {
			t.Errorf("extend got %s %v, want %s", field, entry[field], want)
		}
	}
	if _, leaked := entry["restore_token"]; leaked {
		t.Error("mirror's portal token travelled with the pairing")
	}
}

// A pairing that wins still copies as one unit: a receiver_id the winner
// lacks is removed, not left beside keys it was not recorded with. Without
// its key a receiver_id is no identity, so a pairing that has none can win
// over it -- by mode order, as it always has.
func TestAPairingWithoutAReceiverIdentityClearsAStaleOne(t *testing.T) {
	dir := t.TempDir()
	putCreds(t, CredsPath(dir, "extend"), `{"AA:BB:CC:DD:EE:FF":{"pairing_id":"old",`+
		`"ed25519_public":"old-pub","ed25519_seed":"old-seed","receiver_id":"old-tv-id"}}`)
	putCreds(t, CredsPath(dir, "mirror"), pairedTVWithProtocol) // re-paired, no identity

	if _, err := MergePairing(CredsPath(dir, "mirror"), CredsPath(dir, "extend")); err != nil {
		t.Fatal(err)
	}

	entry := readCredsMap(t, CredsPath(dir, "extend"))["AA:BB:CC:DD:EE:FF"]
	if v, stale := entry["receiver_id"]; stale {
		t.Errorf("extend kept receiver_id %v beside keys it was not recorded with", v)
	}
	if entry["ed25519_seed"] != "seed-1" {
		t.Errorf("extend's seed = %v, want mirror's pairing", entry["ed25519_seed"])
	}
}

// --- which mode's pairing wins ---
//
// castrd syncs the mode files at every start, in mode order: mirror into
// extend first. Copying blindly let an old mirror pairing, made before the
// helper recorded receiver identities, wipe the identity and the new keys
// extend saved when it paired again -- at every start. For a receiver paired
// with no code that resets the identity the helper trusts on first use, which
// is the window an impersonator needs; for an Apple TV it means a code again.

// pairing is one receiver's entry; identity and pairedAt are left out when "".
func pairing(name, identity, pairedAt, token string) string {
	body := fmt.Sprintf(`"pairing_id":"%s-id","ed25519_public":"%s-pub","ed25519_seed":"%s-seed",`+
		`"pairing_protocol":"hap","receiver_address":"10.0.0.5","restore_token":"%s"`,
		name, name, name, token)
	if identity != "" {
		body += fmt.Sprintf(`,"receiver_id":"tv-id","receiver_ed25519_public":"%s"`, identity)
	}
	if pairedAt != "" {
		body += fmt.Sprintf(`,"paired_at":"%s"`, pairedAt)
	}
	return `{"AA:BB:CC:DD:EE:FF":{` + body + `}}`
}

// syncBothWays syncs in both mode orders, each from the same starting files:
// the winner must not depend on which mode the loop happens to visit first.
func syncBothWays(t *testing.T, mirror, extend string, check func(t *testing.T, dir string)) {
	t.Helper()
	for _, modes := range [][]string{{"mirror", "extend"}, {"extend", "mirror"}} {
		t.Run(strings.Join(modes, "-then-"), func(t *testing.T) {
			dir := t.TempDir()
			putCreds(t, CredsPath(dir, "mirror"), mirror)
			putCreds(t, CredsPath(dir, "extend"), extend)

			if err := SyncPairing(dir, modes); err != nil {
				t.Fatal(err)
			}
			check(t, dir)
		})
	}
}

// bothHold checks that both modes hold the named pairing, and their own tokens.
func bothHold(name, identity string) func(t *testing.T, dir string) {
	return func(t *testing.T, dir string) {
		t.Helper()
		for _, mode := range []string{"mirror", "extend"} {
			entry := readCredsMap(t, CredsPath(dir, mode))["AA:BB:CC:DD:EE:FF"]
			if entry["ed25519_seed"] != name+"-seed" {
				t.Errorf("%s holds seed %v, want %s's pairing", mode, entry["ed25519_seed"], name)
			}
			if identity != "" && entry["receiver_ed25519_public"] != identity {
				t.Errorf("%s holds receiver key %v, want %s", mode,
					entry["receiver_ed25519_public"], identity)
			}
			if entry["restore_token"] != "token-for-"+mode {
				t.Errorf("%s holds token %v, want its own", mode, entry["restore_token"])
			}
		}
	}
}

func TestAStaleModeNeverWipesTheIdentityAnotherModeRePairedWith(t *testing.T) {
	syncBothWays(t,
		pairing("old", "", "", "token-for-mirror"),
		pairing("new", "tv-key", "2026-11-01T10:00:00Z", "token-for-extend"),
		bothHold("new", "tv-key"))
}

func TestARePairedMirrorReachesAStaleExtend(t *testing.T) {
	syncBothWays(t,
		pairing("new", "tv-key", "2026-11-01T10:00:00Z", "token-for-mirror"),
		pairing("old", "", "", "token-for-extend"),
		bothHold("new", "tv-key"))
}

// The identity rule is not about recency: a pairing with no identity is never
// trusted over one with, whatever time it claims.
func TestAnIdentityBeatsNoIdentityEvenWhenOlder(t *testing.T) {
	syncBothWays(t,
		pairing("bare", "", "2026-11-05T10:00:00Z", "token-for-mirror"),
		pairing("known", "tv-key", "2026-01-01T10:00:00Z", "token-for-extend"),
		bothHold("known", "tv-key"))
}

// An empty key is no key: it proves nothing about the receiver.
func TestAnEmptyReceiverKeyIsNoIdentity(t *testing.T) {
	syncBothWays(t,
		`{"AA:BB:CC:DD:EE:FF":{"pairing_id":"blank-id","ed25519_public":"blank-pub",`+
			`"ed25519_seed":"blank-seed","receiver_id":"","receiver_ed25519_public":"",`+
			`"paired_at":"2026-11-05T10:00:00Z","restore_token":"token-for-mirror"}}`,
		pairing("known", "tv-key", "2026-01-01T10:00:00Z", "token-for-extend"),
		bothHold("known", "tv-key"))
}

func TestOfTwoIdentitiesTheNewerPairingWinsInExtend(t *testing.T) {
	syncBothWays(t,
		pairing("old", "old-key", "2026-10-01T10:00:00Z", "token-for-mirror"),
		pairing("new", "new-key", "2026-11-01T10:00:00Z", "token-for-extend"),
		bothHold("new", "new-key"))
}

func TestOfTwoIdentitiesTheNewerPairingWinsInMirror(t *testing.T) {
	syncBothWays(t,
		pairing("new", "new-key", "2026-11-01T10:00:00.5Z", "token-for-mirror"),
		pairing("old", "old-key", "2026-11-01T10:00:00Z", "token-for-extend"),
		bothHold("new", "new-key"))
}

// The time is compared, not the text: the same moment in another zone is not
// newer because its digits sort later.
func TestPairingTimesAreComparedAsTimes(t *testing.T) {
	syncBothWays(t,
		pairing("new", "new-key", "2026-11-01T10:00:00Z", "token-for-mirror"),
		pairing("old", "old-key", "2026-11-01T11:00:00+03:00", "token-for-extend"),
		bothHold("new", "new-key"))
}

func TestAMissingOrUnreadablePairingTimeCountsAsOldest(t *testing.T) {
	for name, when := range map[string]string{"missing": "", "unreadable": "last tuesday"} {
		t.Run(name, func(t *testing.T) {
			syncBothWays(t,
				pairing("dated", "dated-key", "2000-01-01T00:00:00Z", "token-for-mirror"),
				pairing("undated", "undated-key", when, "token-for-extend"),
				bothHold("dated", "dated-key"))
		})
	}
}

// A pairing time in the future -- a clock set wrong, or a hand edit -- would
// otherwise win forever: castr would keep that stale pairing over every real
// one made later, and the receiver would ask for a code at every cast.
func TestAPairingTimeInTheFutureCountsAsOldest(t *testing.T) {
	syncBothWays(t,
		pairing("dated", "dated-key", "2000-01-01T00:00:00Z", "token-for-mirror"),
		pairing("future", "future-key", "2999-01-01T00:00:00Z", "token-for-extend"),
		bothHold("dated", "dated-key"))
}

// A few minutes ahead is ordinary clock drift between the pairing machine
// and this one, or the helper's clock against castrd's; more is not.
func TestAPairingTimeIsTrustedUpToFiveMinutesAhead(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	entry := func(at time.Time) map[string]any {
		return map[string]any{"receiver_ed25519_public": "key",
			"paired_at": at.Format(time.RFC3339Nano)}
	}
	older := entry(now.Add(-time.Hour))

	for _, tc := range []struct {
		ahead time.Duration
		wins  bool
	}{
		{0, true},
		{5 * time.Minute, true},
		{5*time.Minute + time.Second, false},
		{24 * time.Hour, false},
	} {
		if got := supersedes(entry(now.Add(tc.ahead)), older, now); got != tc.wins {
			t.Errorf("a pairing %v ahead of now: supersedes = %v, want %v", tc.ahead, got, tc.wins)
		}
		// Counted as oldest, it loses the other way round too.
		if got := supersedes(older, entry(now.Add(tc.ahead)), now); got == tc.wins {
			t.Errorf("against a pairing %v ahead of now, an hour-old one: supersedes = %v, want %v",
				tc.ahead, got, !tc.wins)
		}
	}
}

// With nothing to tell the two apart, mode order decides, as it always has.
func TestWithNeitherIdentityModeOrderStillDecides(t *testing.T) {
	dir := t.TempDir()
	putCreds(t, CredsPath(dir, "mirror"), pairing("first", "", "", "token-for-mirror"))
	putCreds(t, CredsPath(dir, "extend"), pairing("second", "", "2026-11-01T10:00:00Z",
		"token-for-extend"))

	if err := SyncPairing(dir, []string{"mirror", "extend"}); err != nil {
		t.Fatal(err)
	}
	bothHold("first", "")(t, dir)
}

func TestWithEquallyOldIdentitiesModeOrderStillDecides(t *testing.T) {
	dir := t.TempDir()
	putCreds(t, CredsPath(dir, "mirror"), pairing("first", "first-key", "2026-11-01T10:00:00Z",
		"token-for-mirror"))
	putCreds(t, CredsPath(dir, "extend"), pairing("second", "second-key", "2026-11-01T10:00:00Z",
		"token-for-extend"))

	if err := SyncPairing(dir, []string{"mirror", "extend"}); err != nil {
		t.Fatal(err)
	}
	bothHold("first", "first-key")(t, dir)
}

// Migration merges too, at every start: omarchy-cast's pairings predate
// receiver identities, and must not wipe one castr's helper has recorded.
func TestALegacyPairingNeverWipesAnIdentity(t *testing.T) {
	d := newDirs(t)
	putCreds(t, filepath.Join(d.legacyState, LegacyCredsFilenames["mirror"]), pairedTV)
	putCreds(t, CredsPath(d.state, "mirror"),
		pairing("new", "tv-key", "2026-11-01T10:00:00Z", "token-for-mirror"))

	if _, err := Migrate(d.config, d.state, d.legacyConfig, d.legacyState); err != nil {
		t.Fatal(err)
	}

	entry := readCredsMap(t, CredsPath(d.state, "mirror"))["AA:BB:CC:DD:EE:FF"]
	if entry["ed25519_seed"] != "new-seed" || entry["receiver_ed25519_public"] != "tv-key" {
		t.Errorf("mirror holds seed %v / receiver key %v, want castr's own pairing kept",
			entry["ed25519_seed"], entry["receiver_ed25519_public"])
	}
}

// doubletake rejects a pairing missing any of its keys. Copying a partial one
// would replace a working pairing with one that cannot be used.
func TestAnIncompletePairingIsNotCopied(t *testing.T) {
	dir := t.TempDir()
	putCreds(t, CredsPath(dir, "extend"), `{"AA:BB:CC:DD:EE:FF":{"pairing_id":"good",`+
		`"ed25519_public":"good-pub","ed25519_seed":"good-seed"}}`)
	putCreds(t, CredsPath(dir, "mirror"), `{"AA:BB:CC:DD:EE:FF":{"pairing_id":"half"}}`)

	if _, err := MergePairing(CredsPath(dir, "mirror"), CredsPath(dir, "extend")); err != nil {
		t.Fatal(err)
	}

	entry := readCredsMap(t, CredsPath(dir, "extend"))["AA:BB:CC:DD:EE:FF"]
	if entry["pairing_id"] != "good" || entry["ed25519_seed"] != "good-seed" {
		t.Errorf("a half pairing replaced a working one: %v", entry)
	}
}

// A malformed file must not take the daemon down. Values arriving as arrays
// cannot be compared with !=, and comparing them used to be the only check.
func TestAnOddlyShapedCredentialsFileDoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	// Arrays on BOTH sides: Go panics comparing two values of the same
	// uncomparable type, and only then -- an array against a string is merely
	// unequal, so a test with a string on one side proves nothing.
	putCreds(t, CredsPath(dir, "extend"), `{"AA:BB:CC:DD:EE:FF":{"pairing_id":["x"],`+
		`"ed25519_public":["y"],"ed25519_seed":["z"]}}`)
	putCreds(t, CredsPath(dir, "mirror"), `{"AA:BB:CC:DD:EE:FF":{"pairing_id":["p"],`+
		`"ed25519_public":["q"],"ed25519_seed":["r"]}}`)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("merging panicked: %v", r)
		}
	}()
	if _, err := MergePairing(CredsPath(dir, "mirror"), CredsPath(dir, "extend")); err != nil {
		t.Fatal(err)
	}
}
