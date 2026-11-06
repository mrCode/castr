package sound

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// StateFile, in the daemon's state dir, exists exactly while castr has
// changed the laptop's sound. It is written before the default output is
// changed, so a daemon that dies at any point after leaves enough behind for
// Recover to put the sound back.
const StateFile = "sound.json"

type savedState struct {
	PreviousDefault string `json:"previous_default"`
	LaptopSink      string `json:"laptop_sink"`
}

func statePath(dir string) string { return filepath.Join(dir, StateFile) }

// saveState writes the state file atomically, as config.SaveDevices does: a
// half-written file after a crash would lose the sink to restore.
func saveState(dir string, s savedState) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding the sound state: %w", err)
	}
	raw = append(raw, '\n')

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".sound-*")
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
	if err := os.Rename(tmpName, statePath(dir)); err != nil {
		return fmt.Errorf("replacing %s: %w", statePath(dir), err)
	}
	return nil
}

// loadState reports whether a state file exists, and what it says. An
// unreadable file still counts as present: something was routed, even if the
// sink to go back to is lost.
func loadState(dir string) (savedState, bool, error) {
	raw, err := os.ReadFile(statePath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return savedState{}, false, nil
	}
	if err != nil {
		return savedState{}, true, fmt.Errorf("reading %s: %w", statePath(dir), err)
	}
	var s savedState
	if err := json.Unmarshal(raw, &s); err != nil {
		return savedState{}, true, nil
	}
	return s, true, nil
}

func clearState(dir string) error {
	if err := os.Remove(statePath(dir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing %s: %w", statePath(dir), err)
	}
	return nil
}

// Recover restores routing left by a daemon that died. It finds leftovers by
// the state file or by castr modules, so it works with either missing; with
// neither, it changes nothing.
func Recover(run Runner, stateDir string) error {
	saved, pending, loadErr := loadState(stateDir)
	if !pending {
		mods, err := listModules(run)
		if err != nil {
			return err
		}
		if len(castrModuleIDs(mods)) == 0 {
			return nil
		}
	}
	return errors.Join(loadErr, restore(run, stateDir, saved.LaptopSink, saved.PreviousDefault, nil))
}
