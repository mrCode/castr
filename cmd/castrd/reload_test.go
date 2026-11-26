package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrCode/castr/internal/config"
	"github.com/mrCode/castr/internal/discovery"
)

func writeAt(t *testing.T, path, text string, mod time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func TestAChangedConfigIsPickedUpByTheNextCast(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	base := time.Now().Add(-time.Hour)
	writeAt(t, path, "[capture]\nfps = 30\n", base)
	initial, _ := config.Load(path)
	live := newLiveConfig(path, initial)
	var seen []int
	live.onChange = func(c config.Config) { seen = append(seen, c.Capture.FPS) }

	live.refresh()
	if len(seen) != 0 {
		t.Fatalf("an unchanged file was reloaded: %v", seen)
	}

	writeAt(t, path, "[capture]\nfps = 60\n\n[airplay]\nhide_vapostproc = false\n", base.Add(time.Minute))
	live.refresh()
	if len(seen) != 1 || seen[0] != 60 || live.current().AirPlay.HideVAPostproc {
		t.Fatalf("after a change: seen %v, hide_vapostproc %v", seen, live.current().AirPlay.HideVAPostproc)
	}

	writeAt(t, path, "[capture\nfps = ", base.Add(2*time.Minute))
	live.refresh()
	if live.current().Capture.FPS != 60 || len(seen) != 1 {
		t.Fatalf("a broken file replaced the settings: fps %d, seen %v", live.current().Capture.FPS, seen)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	live.refresh()
	if live.current().Capture.FPS != config.Default().Capture.FPS {
		t.Fatalf("a deleted file kept fps %d, want the default", live.current().Capture.FPS)
	}
}

type recordingBackend struct{ started []string }

func (r *recordingBackend) Start(_ context.Context, d discovery.Device, _ string) error {
	r.started = append(r.started, d.ID)
	return nil
}
func (r *recordingBackend) Stop(context.Context, discovery.Device) error { return nil }
func (r *recordingBackend) SubmitPin(context.Context, discovery.Device, string) error {
	return nil
}

func TestEveryCastStartRefreshesTheConfigFirst(t *testing.T) {
	var order []string
	inner := &recordingBackend{}
	b := reloadingBackend{Backend: inner, refresh: func() { order = append(order, "refresh") }}
	if err := b.Start(context.Background(), discovery.Device{ID: "tv"}, "mirror"); err != nil {
		t.Fatal(err)
	}
	if len(order) != 1 || len(inner.started) != 1 {
		t.Fatalf("refreshes %v, starts %v", order, inner.started)
	}
}

func TestTheSpawnedChildSeesTheCurrentGPUSetting(t *testing.T) {
	cfg := config.Default()
	hidden := childEnv(cfg, t.TempDir())
	cfg.AirPlay.HideVAPostproc = false
	shown := childEnv(cfg, t.TempDir())
	if len(hidden) == len(shown) {
		t.Fatal("childEnv ignored hide_vapostproc")
	}
}
