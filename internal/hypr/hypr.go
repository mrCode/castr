// Package hypr drives Hyprland outputs.
//
// This is the only package that runs hyprctl. Everything it does exists to
// avoid one mistake the Python version made for months: forcing the PANEL to
// 1920x1080 to give the encoder a 1080p source. On a laptop offering only
// 2560x1600 at 240Hz or 60Hz there is no native 1080p mode, so the compositor
// synthesised one at 60Hz -- and the user typed on a display four times slower
// for the whole cast, blaming the network. The cast was fine.
//
// Instead we create a virtual 1920x1080 output. For extend it stands alone and
// becomes a second desktop; for mirror it mirrors the panel. Either way the
// panel keeps its own mode.
package hypr

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Runner executes a command and returns stdout. Injected so no test ever
// touches the real compositor -- the Python suite created real outputs on a
// developer's desktop more than once.
type Runner func(name string, args ...string) (string, error)

// Output names. Mirror and extend must never share one: they can be active at
// the same time to different receivers, and a shared name means one session's
// teardown removes the other's live output.
const (
	OutputExtend = "castr"
	OutputMirror = "castr-mirror"

	geometry = "1920x1080@60"
)

// ours lists the names castr creates, and therefore the only ones it may
// remove. Sweeping every HEADLESS* output -- which the Python version did at
// first -- destroys outputs castr never made: wayvnc and Sunshine both create
// them, and a user's live remote-desktop output vanished on any invocation.
var ours = []string{OutputExtend, OutputMirror}

// Monitor is the part of hyprctl's monitor JSON we use.
type Monitor struct {
	Name    string  `json:"name"`
	Width   int     `json:"width"`
	Height  int     `json:"height"`
	Refresh float64 `json:"refreshRate"`
	// Position matters on restore: putting a mode back without it moves the
	// panel to 0x0 and shuffles every other monitor on a multi-monitor desk.
	X        int     `json:"x"`
	Y        int     `json:"y"`
	Scale    float64 `json:"scale"`
	Focused  bool    `json:"focused"`
	ID       int     `json:"id"`
	MirrorOf string  `json:"mirrorOf"`
	Disabled bool    `json:"disabled"`
}

// Monitors lists every output Hyprland knows, including mirrored ones.
//
// `monitors all` rather than `monitors`: a mirrored output does not appear in
// the plain listing at all, so code that looked there concluded its own output
// had not been created.
func Monitors(run Runner) ([]Monitor, error) {
	out, err := run("hyprctl", "-j", "monitors", "all")
	if err != nil {
		return nil, fmt.Errorf("hyprctl monitors: %w", err)
	}
	var monitors []Monitor
	if err := json.Unmarshal([]byte(out), &monitors); err != nil {
		return nil, fmt.Errorf("parsing hyprctl monitors: %w", err)
	}
	return monitors, nil
}

// Focused returns the monitor a mirrored output should copy.
func Focused(run Runner) (string, error) {
	// Deliberately the plain listing: a mirrored output is never the one the
	// user is working on, and must not be picked as the mirror source.
	out, err := run("hyprctl", "-j", "monitors")
	if err != nil {
		return "", fmt.Errorf("hyprctl monitors: %w", err)
	}
	var monitors []Monitor
	if err := json.Unmarshal([]byte(out), &monitors); err != nil {
		return "", fmt.Errorf("parsing hyprctl monitors: %w", err)
	}
	if len(monitors) == 0 {
		return "", fmt.Errorf("no monitors")
	}
	for _, m := range monitors {
		if m.Focused {
			return m.Name, nil
		}
	}
	return monitors[0].Name, nil
}

// CreateOutput makes a virtual 1920x1080 output and returns the name Hyprland
// actually used. mirrorOf, when set, makes it show that monitor's content.
//
// Every failure after the output exists removes it again. Returning an error
// while leaving it behind strands a 1920x1080 monitor on the user's desktop
// *and* tells them nothing could be created -- wrong on both counts.
func CreateOutput(run Runner, want, mirrorOf string) (string, error) {
	if want == "" {
		want = OutputExtend
	}

	// An output of this name may already exist -- a leftover from a daemon that
	// died mid-cast. 0.56.2 refuses to create over it ("Name already taken"),
	// so reuse it instead of failing the cast for a name collision with
	// ourselves.
	existing, err := hasOutput(run, want)
	if err != nil {
		return "", err
	}
	if !existing {
		if err := mutate(run, "output", "create", "headless", want); err != nil {
			return "", fmt.Errorf("creating output: %w", err)
		}
	}

	name, err := resolveName(run, want)
	if err != nil {
		remove(run, want)
		return "", err
	}
	_ = existing

	if err := Apply(run, MonitorSpec{Output: name, Mode: geometry,
		Position: "auto", Scale: 1, Mirror: mirrorOf}); err != nil {
		// Created but not configured: unusable as a desktop, so do not leave it.
		remove(run, name)
		return "", fmt.Errorf("configuring output %s: %w", name, err)
	}

	// Asked for, and then CHECKED. hyprctl answered "ok" to a mirror that was
	// never applied for a whole Hyprland release, and the only visible symptom
	// was an Apple TV showing an empty desktop.
	if mirrorOf != "" {
		if err := verifyMirror(run, name, mirrorOf); err != nil {
			remove(run, name)
			return "", err
		}
	}

	return name, nil
}

// verifyMirror confirms the output really is mirroring the source.
//
// hyprctl reports mirrorOf as the source monitor's ID, not its name, so the
// name has to be resolved to an id first.
func verifyMirror(run Runner, name, mirrorOf string) error {
	monitors, err := Monitors(run)
	if err != nil {
		return err
	}

	sourceID := ""
	for _, m := range monitors {
		if m.Name == mirrorOf {
			sourceID = strconv.Itoa(m.ID)
		}
	}

	for _, m := range monitors {
		if m.Name != name {
			continue
		}
		if m.MirrorOf == mirrorOf || (sourceID != "" && m.MirrorOf == sourceID) {
			return nil
		}
		return fmt.Errorf(
			"output %s was created but is not mirroring %s (mirrorOf=%q); "+
				"the receiver would show an empty desktop",
			name, mirrorOf, m.MirrorOf)
	}
	return fmt.Errorf("output %s vanished after being configured", name)
}

// hasOutput reports whether an output of this name is already known.
func hasOutput(run Runner, name string) (bool, error) {
	monitors, err := Monitors(run)
	if err != nil {
		return false, err
	}
	for _, m := range monitors {
		if m.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// resolveName confirms the output exists and reports the name it really has.
//
// It prefers the requested name over diffing the monitor list before and
// after. The diff looks safer and is not: `monitors all` also lists stale
// monitor RULES from earlier runs, so an output just created can appear in the
// "before" set, leaving the diff empty and creation reported as failure having
// succeeded. The diff survives only for what it was written for -- Hyprland
// ignoring the requested name and inventing HEADLESS-N.
func resolveName(run Runner, want string) (string, error) {
	monitors, err := Monitors(run)
	if err != nil {
		return "", err
	}
	var headless string
	for _, m := range monitors {
		if m.Name == want {
			return want, nil
		}
		if strings.HasPrefix(m.Name, "HEADLESS") {
			headless = m.Name
		}
	}
	if headless != "" {
		// Naming is undocumented; if a Hyprland version drops it, the portal
		// restore token references a name that changes every run.
		return headless, nil
	}
	return "", fmt.Errorf("hyprctl reported success but %s does not exist", want)
}

// RemoveOutput deletes a virtual output. Reports whether it actually went.
//
// UN-MIRRORS FIRST, and that order is the whole point. On Hyprland 0.56.2 a
// mirrored headless output cannot be removed: `output remove` answers "output
// not found" for an output `monitors` is listing, exits 0, and the output stays
// on the desk forever stealing windows. Clearing the mirror makes it removable.
func RemoveOutput(run Runner, name string) error {
	// Best effort: an output that was never mirroring is already removable, and
	// a failure here must not stop the removal itself.
	_ = Apply(run, MonitorSpec{Output: name, Mode: geometry,
		Position: "auto", Scale: 1, Mirror: MirrorNone})

	if err := mutate(run, "output", "remove", name); err != nil {
		return fmt.Errorf("removing output %s: %w", name, err)
	}
	return nil
}

func remove(run Runner, name string) { _ = RemoveOutput(run, name) }

// CleanupStrays removes our own leftover outputs after a crash, skipping any
// name a live session still owns.
//
// The skip is not a nicety. A second daemon swept the virtual output of a cast
// running in another process, and the owning daemon logged nothing because the
// damage happened elsewhere -- the cast simply died.
func CleanupStrays(run Runner, inUse []string) (int, error) {
	monitors, err := Monitors(run)
	if err != nil {
		return 0, err
	}

	busy := map[string]bool{}
	for _, n := range inUse {
		busy[n] = true
	}

	removed := 0
	for _, m := range monitors {
		if !isOurs(m.Name) || busy[m.Name] {
			continue
		}
		if err := RemoveOutput(run, m.Name); err == nil {
			removed++
		}
	}
	return removed, nil
}

func isOurs(name string) bool {
	for _, n := range ours {
		if n == name {
			return true
		}
	}
	return false
}
