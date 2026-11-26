package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrCode/castr/internal/config"
)

func settingsHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.app.ConfigPath = filepath.Join(t.TempDir(), "config.toml")
	return h
}

func loaded(t *testing.T, h *harness) config.Config {
	t.Helper()
	cfg, err := config.Load(h.app.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestSetFPSWritesTheConfig(t *testing.T) {
	h := settingsHarness(t)
	if code := h.app.Run([]string{"set", "fps", "60"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	if cfg := loaded(t, h); cfg.Capture.FPS != 60 {
		t.Fatalf("fps %d, want 60", cfg.Capture.FPS)
	}
	if !strings.Contains(h.out.String(), "next cast") {
		t.Errorf("did not say when it applies:\n%s", h.out)
	}
}

func TestSetGPUCaptureFlipsHideVAPostproc(t *testing.T) {
	h := settingsHarness(t)
	if code := h.app.Run([]string{"set", "gpu-capture", "on"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	if cfg := loaded(t, h); cfg.AirPlay.HideVAPostproc {
		t.Fatal("gpu-capture on left vapostproc hidden")
	}
	if code := h.app.Run([]string{"set", "gpu-capture", "off"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	if cfg := loaded(t, h); !cfg.AirPlay.HideVAPostproc {
		t.Fatal("gpu-capture off did not hide vapostproc")
	}
}

func TestSettingsJSONIsWhatTheBarReads(t *testing.T) {
	h := settingsHarness(t)
	h.app.Run([]string{"set", "fps", "24"})
	h.app.Run([]string{"set", "gpu-capture", "on"})
	h.out.Reset()
	if code := h.app.Run([]string{"settings", "--json"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}
	var got config.Settings
	if err := json.Unmarshal(h.out.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %q", h.out)
	}
	if got.FPS != 24 || !got.GPUCapture {
		t.Fatalf("settings %+v, want 24 fps and GPU capture on", got)
	}
}

func TestSetRejectsNonsense(t *testing.T) {
	h := settingsHarness(t)
	for _, args := range [][]string{
		{"set", "fps", "0"}, {"set", "fps", "500"}, {"set", "fps", "fast"},
		{"set", "gpu-capture", "maybe"}, {"set", "volume", "11"}, {"set", "fps"},
	} {
		if code := h.app.Run(args); code == 0 {
			t.Errorf("%v succeeded", args)
		}
	}
	if cfg := loaded(t, h); cfg.Capture.FPS != config.Default().Capture.FPS {
		t.Fatalf("a refused set changed fps to %d", cfg.Capture.FPS)
	}
}
