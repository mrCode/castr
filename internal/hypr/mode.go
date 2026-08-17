package hypr

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// StreamGeometry is what doubletake negotiates. Its fallback capture path has
// no scaler, so a higher-resolution source makes the receiver drop the
// connection outright.
const StreamGeometry = "1920x1080@60"

// SavedModeFilename remembers the panel's real mode across a daemon restart.
//
// Without it, a daemon that is killed mid-cast leaves the panel at 1080p60
// forever, and the user has no idea what it used to be. Which, on a laptop
// whose only modes are 2560x1600@240 and @60, is a display four times slower
// with nothing on screen explaining why.
const SavedModeFilename = "panel-mode.json"

// SavedMode is a monitor's mode, as hyprctl wants it back.
type SavedMode struct {
	Name    string  `json:"name"`
	Width   int     `json:"width"`
	Height  int     `json:"height"`
	Refresh float64 `json:"refresh"`
	X       int     `json:"x"`
	Y       int     `json:"y"`
	Scale   float64 `json:"scale"`
}

// Spec renders the saved mode as a monitor configuration.
func (m SavedMode) Spec() MonitorSpec {
	return MonitorSpec{
		Output:   m.Name,
		Mode:     fmt.Sprintf("%dx%d@%.5f", m.Width, m.Height, m.Refresh),
		Position: fmt.Sprintf("%dx%d", m.X, m.Y),
		Scale:    m.Scale,
	}
}

// RestorePanel puts a switched panel back, and forgets the saved mode only
// once it really has.
//
// NOTHING IN CASTR SWITCHES A PANEL ANY MORE. This exists to repair state an
// older version -- or omarchy-cast, which did the same -- could have left
// behind: a laptop stuck at 1080p60 with nothing on screen to say why. The
// daemon calls RestorePanelIfPending once at startup and that is all.
func RestorePanel(run Runner, stateDir string) error {
	saved, err := loadMode(stateDir)
	if err != nil {
		return err
	}
	if saved == nil {
		return nil // nothing was switched; restoring would fight the user
	}

	if err := Apply(run, saved.Spec()); err != nil {
		// The file stays, so the next daemon start can try again rather than
		// leaving the panel wrong forever.
		return fmt.Errorf("restoring %s: %w", saved.Name, err)
	}
	clearMode(stateDir)
	return nil
}

// RestorePanelIfPending puts the panel back after a daemon that died mid-cast.
// Called at startup, where there is nothing to report to.
func RestorePanelIfPending(run Runner, stateDir string) bool {
	saved, err := loadMode(stateDir)
	if err != nil || saved == nil {
		return false
	}
	return RestorePanel(run, stateDir) == nil
}

func modePath(stateDir string) string { return filepath.Join(stateDir, SavedModeFilename) }

func loadMode(stateDir string) (*SavedMode, error) {
	raw, err := os.ReadFile(modePath(stateDir))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the saved panel mode: %w", err)
	}
	var mode SavedMode
	if err := json.Unmarshal(raw, &mode); err != nil {
		// Unreadable is the same as absent: better to leave the panel as it is
		// than to apply a mode parsed out of nonsense.
		clearMode(stateDir)
		return nil, nil
	}
	if mode.Name == "" || mode.Width == 0 || mode.Height == 0 {
		clearMode(stateDir)
		return nil, nil
	}
	return &mode, nil
}

func clearMode(stateDir string) { _ = os.Remove(modePath(stateDir)) }

// StreamWidth and StreamHeight are the geometry above, split out for callers
// that need the numbers.
var StreamWidth, StreamHeight = func() (int, int) {
	var w, h int
	fmt.Sscanf(strings.Split(StreamGeometry, "@")[0], "%dx%d", &w, &h)
	return w, h
}()
