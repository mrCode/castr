package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

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

// CredsPath is doubletake's pairing-credentials file: one for every mode and
// every receiver, because doubletake keys its contents by receiver.
func CredsPath(stateDir string) string {
	return filepath.Join(stateDir, "creds", "pairing.json")
}

// LegacyCredsFilenames are omarchy-cast's per-mode credential files. It split
// them by mode on the mistaken belief that they held a portal token; both hold
// the same receiver keys, so either one migrates cleanly.
var LegacyCredsFilenames = []string{
	"doubletake-mirror-credentials.json",
	"doubletake-extend-credentials.json",
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

	// AirPlay pairing credentials. Without these an upgrading user has to walk
	// to the television and retype a code for every receiver they had already
	// paired with. omarchy-cast kept one file per mode holding the same keys;
	// either will do, so take the first that exists.
	newCreds := CredsPath(stateDir)
	for _, name := range LegacyCredsFilenames {
		done, err := copyIfAbsent(filepath.Join(legacyStateDir, name), newCreds, 0o600)
		if err != nil {
			return copied, err
		}
		if done {
			copied = append(copied, newCreds)
			break
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
