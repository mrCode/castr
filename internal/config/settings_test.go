package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if text != "" {
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSetValueReplacesAKeyAndKeepsComments(t *testing.T) {
	path := writeConfig(t, "# mine\n[capture]\n# why 60\nfps = 60\nencoder = \"auto\"\n\n[airplay]\naudio = true\n")
	if err := SetValue(path, "capture", "fps", "24"); err != nil {
		t.Fatal(err)
	}
	got := read(t, path)
	want := "# mine\n[capture]\n# why 60\nfps = 24\nencoder = \"auto\"\n\n[airplay]\naudio = true\n"
	if got != want {
		t.Fatalf("file:\n%s\nwant:\n%s", got, want)
	}
	if cfg, err := Load(path); err != nil || cfg.Capture.FPS != 24 || !cfg.AirPlay.Audio {
		t.Fatalf("loaded %+v, %v", cfg.Capture, err)
	}
}

func TestSetValueAddsAKeyToItsSection(t *testing.T) {
	path := writeConfig(t, "[capture]\nfps = 30\n\n[airplay]\naudio = true\n")
	if err := SetValue(path, "airplay", "hide_vapostproc", "false"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil || cfg.AirPlay.HideVAPostproc || !cfg.AirPlay.Audio || cfg.Capture.FPS != 30 {
		t.Fatalf("loaded %+v %+v, %v", cfg.AirPlay, cfg.Capture, err)
	}
	if strings.Count(read(t, path), "[airplay]") != 1 {
		t.Fatalf("section duplicated:\n%s", read(t, path))
	}
}

func TestSetValueCreatesTheFileAndSection(t *testing.T) {
	path := writeConfig(t, "")
	if err := SetValue(path, "capture", "fps", "60"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); got != "[capture]\nfps = 60\n" {
		t.Fatalf("file:\n%q", got)
	}
}

func TestSetValueOnlyTouchesItsOwnSection(t *testing.T) {
	// chromecast has no fps, but a same-named key elsewhere must not be hit.
	path := writeConfig(t, "[chromecast]\nport = 8010\n\n[airplay]\nport_range = \"60000-60010\"\n")
	if err := SetValue(path, "airplay", "port_range", "\"61000-61010\""); err != nil {
		t.Fatal(err)
	}
	got := read(t, path)
	if !strings.Contains(got, "port = 8010") || !strings.Contains(got, "port_range = \"61000-61010\"") {
		t.Fatalf("file:\n%s", got)
	}
}

func TestSetValueRefusesWhatWouldNotLoad(t *testing.T) {
	original := "[capture]\nfps = 30\n"
	path := writeConfig(t, original)
	if err := SetValue(path, "capture", "fps", "\"sixty"); err == nil {
		t.Fatal("wrote a value that breaks the file")
	}
	if got := read(t, path); got != original {
		t.Fatalf("the file changed after a refused write:\n%s", got)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("a temporary file was left behind: %v", entries)
	}
}

func TestCurrentSettingsReadsGPUCaptureAsTheInverse(t *testing.T) {
	if s := CurrentSettings(Default()); s.GPUCapture || s.FPS != 30 {
		t.Fatalf("defaults read as %+v, want 30 fps and GPU capture off", s)
	}
}
