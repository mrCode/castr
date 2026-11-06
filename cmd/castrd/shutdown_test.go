package main

import (
	"testing"
	"time"

	"github.com/mrCode/castr/internal/backend/airplay"
	"github.com/mrCode/castr/internal/daemon"
	"github.com/mrCode/castr/internal/discovery"
	"github.com/mrCode/castr/internal/sound"
)

// recordingSound is a daemon.Sound that only counts restores.
type recordingSound struct{ restores int }

func (s *recordingSound) Attach(string) (string, error)      { return "", nil }
func (s *recordingSound) Set(string, sound.Choice) error     { return nil }
func (s *recordingSound) Choice(string) (sound.Choice, bool) { return "", false }
func (s *recordingSound) Detach(string) error                { return nil }
func (s *recordingSound) Follow() error                      { return nil }
func (s *recordingSound) RetryRestore() error                { return nil }
func (s *recordingSound) Restore() error                     { s.restores++; return nil }

// Shutdown restores the sound for every cast, Chromecasts included: its
// session loop stops only the AirPlay ones.
func TestShutdownRestoresTheSound(t *testing.T) {
	for _, on := range []bool{true, false} {
		registry := daemon.NewRegistry(func() ([]discovery.Device, error) { return nil, nil }, time.Now)
		d := daemon.New(registry, map[string]daemon.Backend{})
		snd := &recordingSound{}
		d.Sound, d.SoundOn = snd, on
		noMonitors := func(string, ...string) (string, error) { return "[]", nil }

		shutdown(d, &airplay.Backend{}, registry, t.TempDir(), noMonitors)

		want := 0
		if on {
			want = 1
		}
		if snd.restores != want {
			t.Errorf("sound on %v: restored %d times, want %d", on, snd.restores, want)
		}
	}
}
