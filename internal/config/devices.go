package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/mrCode/castr/internal/discovery"
)

// DevicesFilename holds the receivers the user registered by address.
//
// mDNS is not always usable. An access point can filter multicast per device,
// so a receiver stays perfectly reachable while never answering discovery -- on
// one tested network an Apple TV served AirPlay on port 7000 and answered no
// mDNS query at all, while a MacBook on the same subnet and access point
// answered both.
//
// Without persistence that escape hatch barely worked: the daemon exits when
// idle, taking its in-memory list with it, so a manually added receiver
// vanished and the address had to be retyped for every cast. The receivers
// that need it are precisely the ones that need it EVERY time, since discovery
// will never start finding them on its own.
const DevicesFilename = "manual-devices.json"

// DevicesPath is the store inside a state directory.
func DevicesPath(stateDir string) string { return filepath.Join(stateDir, DevicesFilename) }

type storedDevice struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Address  string `json:"address"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
	Model    string `json:"model,omitempty"`
}

// LoadDevices reads the manual receivers.
//
// A missing, corrupt, or unreadable file yields no devices and no error: this
// is a convenience feature, and taking the daemon down over it would trade a
// forgotten address for no casting at all. The file is plain JSON, safe to
// delete or hand-edit.
func LoadDevices(path string) []discovery.Device {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var stored []storedDevice
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil
	}

	out := make([]discovery.Device, 0, len(stored))
	for _, s := range stored {
		if s.ID == "" || s.Address == "" {
			continue // unusable: nothing to cast to
		}
		if s.Protocol == "" {
			s.Protocol = discovery.ProtocolAirPlay
		}
		if s.Port == 0 {
			s.Port = 7000
		}
		if s.Name == "" {
			s.Name = s.ID
		}
		out = append(out, discovery.Device{ID: s.ID, Name: s.Name, Address: s.Address,
			Port: s.Port, Protocol: s.Protocol, Model: s.Model})
	}
	return out
}

// SaveDevices writes the manual receivers atomically.
//
// Atomically because the alternative is a truncated file when the daemon is
// killed mid-write, and a truncated file reads as no devices at all -- silently
// losing exactly the addresses that cannot be rediscovered.
func SaveDevices(path string, devices []discovery.Device) error {
	stored := make([]storedDevice, 0, len(devices))
	for _, d := range devices {
		stored = append(stored, storedDevice{ID: d.ID, Name: d.Name, Address: d.Address,
			Port: d.Port, Protocol: d.Protocol, Model: d.Model})
	}

	raw, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding devices: %w", err)
	}
	raw = append(raw, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".manual-devices-*")
	if err != nil {
		return fmt.Errorf("creating a temporary file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // a no-op once the rename has succeeded

	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("securing %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replacing %s: %w", path, err)
	}
	return nil
}

// CredsPath is doubletake's credentials file for one mode.
//
// PER MODE, and that is deliberate. The file holds two different things under
// each receiver: the AirPlay pairing keys, which belong to the receiver, and a
// screen-share portal restore_token, which belongs to the OUTPUT being
// captured. Mirror and extend capture different outputs, so a shared file
// replays mirror's portal grant when extending -- casting the wrong screen,
// silently. Pairing twice is annoying; casting the wrong thing is worse, and
// SyncPairing removes the annoyance without reintroducing the fault.
func CredsPath(stateDir, mode string) string {
	return filepath.Join(stateDir, "creds", mode+".json")
}

// LegacyCredsFilenames are omarchy-cast's per-mode credential files.
var LegacyCredsFilenames = map[string]string{
	"mirror": "doubletake-mirror-credentials.json",
	"extend": "doubletake-extend-credentials.json",
}

// pairingFields are the per-receiver keys. Everything else in an entry --
// restore_token above all -- describes a capture, not a receiver, and must
// never be copied between modes or carried over from another application.
var pairingFields = []string{"pairing_id", "ed25519_public", "ed25519_seed", "pairing_protocol",
	"receiver_id", "receiver_ed25519_public", "paired_at", "pairing_code", "receiver_address"}

// identityFields must ALL be present for an entry to hold a usable pairing;
// doubletake rejects one missing any of them.
var identityFields = pairingFields[:3]

// A pairing is copied as one unit: its id, both keys, and pairing_protocol --
// the pair-verify protocol it was negotiated with, which doubletake records
// from 0.5.0. Without the protocol doubletake guesses from the receiver's
// capabilities, as 0.4.x did; 0.5.0 began recording it because that guess is
// wrong for some receivers, and a wrong guess fails verification, which
// doubletake answers by pairing from scratch -- the television shows a code
// again. So a field the source lacks is REMOVED from the destination: new keys
// beside an older pairing's protocol describe a pairing that never existed.
//
// receiver_id and receiver_ed25519_public are the receiver's own identity,
// proven when the pairing was made. A doubletake that authenticates receivers
// checks every later connection against them, and pairs again -- the
// television shows a code -- when they are missing. They belong to the same
// pairing, so they travel with it and are removed with it the same way.
//
// paired_at, pairing_code and receiver_address say when the pairing was made,
// with what kind of code ("pin" or "password"; empty for none), and at which
// address. castr-airplay binds a receiver castr can name by no device ID --
// one added by address -- to that address, so it is as much a part of the
// pairing as the keys; and paired_at is how SyncPairing tells which of two
// modes' pairings with a receiver is the newer.

// MergePairing copies pairing keys from src into dst without touching either
// file's portal tokens, and returns the receivers it added. A receiver dst
// already knows keeps dst's pairing unless src's supersedes it.
func MergePairing(srcPath, dstPath string) ([]string, error) {
	return mergePairing(srcPath, dstPath, false)
}

// mergePairing is MergePairing. With strict, a dst that exists but does not
// parse is refused rather than taken for an empty file: mid-session a helper
// may be rewriting it in place, and writing over a half-written file would
// leave only src's entries -- dst's own tokens, and any pairing made only
// there, gone. MergePairing stays lenient because it runs when no helper does
// (castrd's start, migration), where such a file really is damaged; the
// helper refuses to start on it, and replacing it is the repair.
func mergePairing(srcPath, dstPath string, strict bool) ([]string, error) {
	src, err := readCreds(srcPath)
	if err != nil || len(src) == 0 {
		return nil, err
	}
	dst, err := readCreds(dstPath)
	if err != nil {
		return nil, err
	}
	if dst == nil && strict {
		if _, statErr := os.Stat(dstPath); statErr == nil {
			return nil, fmt.Errorf("%s is unreadable or half written; left alone", dstPath)
		}
	}
	if dst == nil {
		dst = map[string]map[string]any{}
	}

	now := time.Now()
	var added []string
	for receiver, entry := range src {
		// A partial pairing is not copied: it would replace a working one
		// with one doubletake refuses.
		if !hasAll(entry, identityFields) {
			continue
		}
		target, ok := dst[receiver]
		if ok && !supersedes(entry, target, now) {
			continue
		}
		if !ok {
			target = map[string]any{}
		}
		// The helper binds a receiver to an address on an existing pairing
		// too, after a pair-verify succeeds, without making it any newer. So
		// the same pairing can hold an address in one mode and none in the
		// other, with nothing to tell which is newer -- and mode order alone
		// would delete the address from the mode that learned it, at every
		// start. For the same pairing, equally old, an address is only ever
		// added or replaced, never removed. src already supersedes dst here;
		// dst superseding src as well is what "equally old" means.
		keepAddress := ok && samePairing(entry, target) && supersedes(target, entry, now)
		changed := false
		for _, field := range pairingFields {
			value, present := entry[field]
			existing, had := target[field]
			switch {
			case field == "receiver_address" && !present && keepAddress:
				// dst's address stays.
			// DeepEqual, not !=: a malformed file can hold arrays, and Go
			// panics comparing two of them -- taking the daemon down with it.
			case present && (!had || !reflect.DeepEqual(existing, value)):
				target[field] = value
				changed = true
			case !present && had:
				delete(target, field)
				changed = true
			}
		}
		if !changed && ok {
			continue
		}
		// Note what is NOT here: restore_token is never copied, because only
		// pairingFields are. It describes a captured OUTPUT, and this mode
		// captures a different one. Deleting the destination's own token would
		// be wrong for the same reason -- it is that mode's, and valid.
		dst[receiver] = target
		added = append(added, receiver)
	}
	if len(added) == 0 {
		return nil, nil
	}
	return added, writeCreds(dstPath, dst)
}

// supersedes reports whether src's pairing may replace dst's, for one receiver
// both files know.
//
// The files are synced in mode order at every castrd start, so copying blindly
// let an old mirror pairing -- made before the helper recorded receiver
// identities -- wipe the identity and the new keys extend saved when it paired
// again, at every start. For a receiver paired with no code that resets the
// identity the helper trusts on first use, which is exactly the window an
// impersonator needs; for an Apple TV it means typing a code again.
//
// So a pairing that holds the receiver's identity always beats one that does
// not, whatever time either claims; of two that both hold one, the later
// paired_at wins, a missing or unreadable one counting as oldest -- and so
// does one more than clockSkew ahead of now, which a clock set wrong or a hand
// edit would otherwise make win forever: castr would keep that stale pairing
// over every real one made later, and the receiver would ask for a code at
// every cast. Only when nothing tells them apart does mode order decide, as it
// always has: src wins.
// Asked both ways, as SyncPairing does, the winner is the same either way.
func supersedes(src, dst map[string]any, now time.Time) bool {
	srcKnown, dstKnown := hasIdentity(src), hasIdentity(dst)
	if srcKnown != dstKnown {
		return srcKnown
	}
	if !srcKnown {
		return true
	}
	return !pairedAt(src, now).Before(pairedAt(dst, now))
}

// samePairing reports whether two entries hold one pairing: the same id and
// keys, with the same receiver.
func samePairing(a, b map[string]any) bool {
	for _, field := range []string{"pairing_id", "ed25519_public", "receiver_ed25519_public"} {
		if !reflect.DeepEqual(a[field], b[field]) {
			return false
		}
	}
	return true
}

// hasIdentity reports whether an entry holds the receiver's own key.
func hasIdentity(entry map[string]any) bool {
	key, ok := entry["receiver_ed25519_public"].(string)
	return ok && key != ""
}

// clockSkew is how far ahead of now a pairing time is still believed: ordinary
// drift between the clock that paired and the one comparing, and no more.
const clockSkew = 5 * time.Minute

// pairedAt is when an entry was paired; the zero time, oldest of all, when it
// does not say, says something unreadable, or says a time too far ahead of now
// to have happened.
func pairedAt(entry map[string]any, now time.Time) time.Time {
	text, _ := entry["paired_at"].(string)
	at, err := time.Parse(time.RFC3339Nano, text)
	if err != nil || at.After(now.Add(clockSkew)) {
		return time.Time{}
	}
	return at
}

// ClearRestoreTokens drops the screen-share grants while keeping every
// pairing, and reports which receivers were affected.
//
// The portal remembers what you picked the first time and never asks again.
// Pick the panel instead of castr's output -- an easy mistake, since the panel
// is the obvious-looking choice in that dialog -- and every later cast captures
// the wrong thing at the wrong resolution, silently, forever. This is the way
// back, and it deliberately does NOT touch the pairing keys: re-picking an
// output should not mean walking to the television to retype a code.
func ClearRestoreTokens(stateDir string, modes []string) ([]string, error) {
	var cleared []string
	for _, mode := range modes {
		path := CredsPath(stateDir, mode)
		creds, err := readCreds(path)
		if err != nil || len(creds) == 0 {
			continue
		}
		changed := false
		for receiver, entry := range creds {
			if _, ok := entry["restore_token"]; !ok {
				continue
			}
			delete(entry, "restore_token")
			creds[receiver] = entry
			cleared = append(cleared, mode+"/"+receiver)
			changed = true
		}
		if changed {
			if err := writeCreds(path, creds); err != nil {
				return cleared, err
			}
		}
	}
	sort.Strings(cleared)
	return cleared, nil
}

// SyncPairing makes every mode's file know every receiver castr has paired
// with, so a receiver is only ever paired once however many modes are used.
// Every pair of modes is merged both ways, and the winning pairing for each
// receiver is the same either way (see supersedes), so the result does not
// depend on which mode comes first -- except where nothing tells the two
// pairings apart.
func SyncPairing(stateDir string, modes []string) error {
	for _, from := range modes {
		for _, to := range modes {
			if from == to {
				continue
			}
			if _, err := MergePairing(CredsPath(stateDir, from), CredsPath(stateDir, to)); err != nil {
				return err
			}
		}
	}
	return nil
}

// PullPairing brings every receiver the other modes have paired with into
// mode's own file, just before mode casts. Syncing only when castrd started
// was not enough: castrd keeps running for fifteen minutes after a cast, so
// extending straight after a first mirror asked the television for a code
// that had just been typed.
//
// It writes mode's file and nothing else. Another mode's file can belong to a
// helper that is casting right now, which rewrites it from memory whenever it
// saves -- and castr could read it half-written.
func PullPairing(stateDir, mode string, modes []string) error {
	for _, from := range modes {
		if from == mode {
			continue
		}
		if _, err := mergePairing(CredsPath(stateDir, from), CredsPath(stateDir, mode), true); err != nil {
			return err
		}
	}
	return nil
}

// ForgetPairing removes castr's pairing with one receiver from every mode's
// file, so the next cast pairs with it from scratch and asks for a code. This
// is what castr-airplay means by "remove its entry from the credentials file":
// the way to pair again with a receiver that was reset or replaced.
//
// target names the receiver by device ID, the key it is saved under, or by
// the address its pairing is bound to (receiver_address), which is how a
// receiver added by address is known. Only pairingFields go: the
// restore_token is which output to share, not part of the pairing, and an
// entry left with nothing is removed. It returns the modes it changed.
//
// The caller stops every cast first. A running helper rewrites its whole file
// from memory when it saves, and would put the pairing back.
func ForgetPairing(stateDir string, modes []string, target string) ([]string, error) {
	var changed []string
	for _, mode := range modes {
		path := CredsPath(stateDir, mode)
		creds, err := readCreds(path)
		if err != nil {
			return changed, err
		}
		touched := false
		for id, entry := range creds {
			address, _ := entry["receiver_address"].(string)
			if !strings.EqualFold(id, target) && (address == "" || address != target) {
				continue
			}
			for _, field := range pairingFields {
				if _, ok := entry[field]; ok {
					delete(entry, field)
					touched = true
				}
			}
			if len(entry) == 0 {
				delete(creds, id)
			}
		}
		if !touched {
			continue
		}
		if err := writeCreds(path, creds); err != nil {
			return changed, err
		}
		changed = append(changed, mode)
	}
	return changed, nil
}

func readCreds(path string) (map[string]map[string]any, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var out map[string]map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		// Hand-editable and replaceable: doubletake will simply pair again.
		return nil, nil
	}
	return out, nil
}

func writeCreds(path string, creds map[string]map[string]any) error {
	raw, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding credentials: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// Replaced by rename, never rewritten in place: a helper starting at that
	// moment reads the old pairings or the new ones, never half a file -- which
	// it would take for no pairings at all, and ask for a code.
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	// On disk before the rename, or a power cut can leave the new name on an
	// empty file -- read as no pairings, so every receiver asks for its code.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// LegacyName is the package castr replaces. Its files are read once, on first
// run, so an upgrading user keeps their settings and their manual receivers.
const LegacyName = "omarchy-cast"

// Migrate copies omarchy-cast's config and manual receivers into castr's own
// directories, if castr has none of its own yet.
//
// It COPIES rather than moves. omarchy-cast is still installed and still works
// while castr is proving itself; deleting its state would break a working tool
// to set up an unproven one. It reports what it copied so the caller can say so.
func Migrate(configDir, stateDir, legacyConfigDir, legacyStateDir string) ([]string, error) {
	var copied []string

	newConfig := filepath.Join(configDir, "config.toml")
	oldConfig := filepath.Join(legacyConfigDir, "config.toml")
	if done, err := copyIfAbsent(oldConfig, newConfig, 0o600); err != nil {
		return copied, err
	} else if done {
		copied = append(copied, newConfig)
	}

	newDevices := DevicesPath(stateDir)
	oldDevices := DevicesPath(legacyStateDir)
	if done, err := copyIfAbsent(oldDevices, newDevices, 0o600); err != nil {
		return copied, err
	} else if done {
		copied = append(copied, newDevices)
	}

	// AirPlay pairing keys, and ONLY those. Without them an upgrading user
	// walks to the television and retypes a code for every receiver they had
	// already paired with.
	//
	// The portal restore_token in the same entries is deliberately dropped: it
	// grants capture of an output named for the OLD application, which no
	// longer exists. Replaying it is how a cast ends up showing the wrong
	// screen with nothing in any log to say so.
	for mode, legacyName := range LegacyCredsFilenames {
		added, err := MergePairing(filepath.Join(legacyStateDir, legacyName),
			CredsPath(stateDir, mode))
		if err != nil {
			return copied, err
		}
		if len(added) > 0 {
			copied = append(copied, fmt.Sprintf("%s (%d receiver pairings)",
				CredsPath(stateDir, mode), len(added)))
		}
	}

	return copied, nil
}

// copyIfAbsent copies src to dst only when dst does not exist, so a user who
// has already configured castr never has it overwritten by a stale legacy file.
func copyIfAbsent(src, dst string, mode os.FileMode) (bool, error) {
	if _, err := os.Stat(dst); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("checking %s: %w", dst, err)
	}

	raw, err := os.ReadFile(src)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil // nothing to migrate: a fresh install, not an upgrade
	}
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", src, err)
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return false, fmt.Errorf("creating %s: %w", filepath.Dir(dst), err)
	}
	if err := os.WriteFile(dst, raw, mode); err != nil {
		return false, fmt.Errorf("writing %s: %w", dst, err)
	}
	return true, nil
}

// hasAll reports whether an entry carries every one of fields.
func hasAll(entry map[string]any, fields []string) bool {
	for _, f := range fields {
		if _, ok := entry[f]; !ok {
			return false
		}
	}
	return true
}
