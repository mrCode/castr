package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

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
var pairingFields = []string{"pairing_id", "ed25519_public", "ed25519_seed"}

// MergePairing copies pairing keys from src into dst without touching either
// file's portal tokens, and returns the receivers it added.
func MergePairing(srcPath, dstPath string) ([]string, error) {
	src, err := readCreds(srcPath)
	if err != nil || len(src) == 0 {
		return nil, err
	}
	dst, err := readCreds(dstPath)
	if err != nil {
		return nil, err
	}
	if dst == nil {
		dst = map[string]map[string]any{}
	}

	var added []string
	for receiver, entry := range src {
		target, ok := dst[receiver]
		if !ok {
			target = map[string]any{}
		}
		changed := false
		for _, field := range pairingFields {
			value, present := entry[field]
			if !present {
				continue
			}
			if existing, ok := target[field]; !ok || existing != value {
				target[field] = value
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
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
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
