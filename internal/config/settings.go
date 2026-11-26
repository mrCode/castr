package config

// the settings `castr set` and the bar
// change. They are written into config.toml one line at a time, so every other
// line -- comments included -- stays as the user left it, and the result must
// load before it replaces the file.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Settings is what `castr settings` reports.
type Settings struct {
	FPS        int  `json:"fps"`
	GPUCapture bool `json:"gpu_capture"`
}

// CurrentSettings reads the user-facing settings out of a configuration.
func CurrentSettings(cfg Config) Settings {
	return Settings{FPS: cfg.Capture.FPS, GPUCapture: !cfg.AirPlay.HideVAPostproc}
}

var sectionLine = regexp.MustCompile(`^\s*\[([^\[\]]+)\]\s*(#.*)?$`)

// SetValue writes `key = value` under [section] of the TOML file at path. value
// is a TOML literal (60, true, "x"). An existing key is replaced in place; a
// missing one goes right under its section header; a missing section is added
// at the end.
func SetValue(path, section, key, value string) error {
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	text := strings.TrimRight(string(raw), "\n")
	var lines []string
	if text != "" {
		lines = strings.Split(text, "\n")
	}
	keyLine := regexp.MustCompile(`^(\s*)` + regexp.QuoteMeta(key) + `\s*=`)
	entry := key + " = " + value

	current, headerAt, done := "", -1, false
	for i, line := range lines {
		if m := sectionLine.FindStringSubmatch(line); m != nil {
			current = strings.TrimSpace(m[1])
			if current == section && headerAt < 0 {
				headerAt = i
			}
			continue
		}
		if current == section {
			if m := keyLine.FindStringSubmatch(line); m != nil {
				lines[i] = m[1] + entry
				done = true
				break
			}
		}
	}
	if !done {
		if headerAt >= 0 {
			lines = append(lines[:headerAt+1], append([]string{entry}, lines[headerAt+1:]...)...)
		} else {
			if len(lines) > 0 {
				lines = append(lines, "")
			}
			lines = append(lines, "["+section+"]", entry)
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.toml")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if _, err := Load(tmpName); err != nil {
		return fmt.Errorf("not saved, the result would not load: %w", err)
	}
	if info, err := os.Stat(path); err == nil {
		_ = os.Chmod(tmpName, info.Mode().Perm())
	}
	return os.Rename(tmpName, path)
}
