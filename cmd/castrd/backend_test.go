package main

import (
	"testing"

	"github.com/mrCode/castr/internal/config"
)

// The codec and the cap have to survive the hand-off from the config file to
// the backend, the same hand-off that once dropped a receiver's port.
func TestTheCodecAndGeometryReachTheAirPlayBackend(t *testing.T) {
	cfg := config.Default()
	cfg.Capture.Codec = "hevc"
	cfg.Capture.Geometry = "2560x1440@60"
	cfg.Capture.GeometrySet = true

	b := newAirPlayBackend(cfg, t.TempDir())

	if b.Config.Codec != "hevc" {
		t.Errorf("codec = %q, want hevc", b.Config.Codec)
	}
	if b.Config.MaxSize != "2560x1440" {
		t.Errorf("max size = %q, want 2560x1440", b.Config.MaxSize)
	}

	plain := newAirPlayBackend(config.Default(), t.TempDir())
	if plain.Config.MaxSize != "" {
		t.Errorf("max size = %q with no geometry configured, want none", plain.Config.MaxSize)
	}
}
