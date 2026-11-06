package capture

import (
	"strings"
	"testing"
)

func hlsOptions() Options {
	return Options{Serial: 4617, FPS: 30, Encoder: "x264enc", Container: HLS, Dir: "/tmp/cast"}
}

// The silent track a receiver gets when there is no sound, exactly as it was
// before casts carried sound: a Chromecast refuses a video-only HLS stream.
const silentTrack = "audiotestsrc wave=silence is-live=true ! audioconvert ! audioresample " +
	"! audio/x-raw,rate=44100,channels=2 ! avenc_aac ! aacparse ! hls.audio"

func TestAChromecastCarriesTheCastsSound(t *testing.T) {
	opts := hlsOptions()
	opts.Audio = true
	opts.AudioSource = "castr_cast_2.monitor"

	got := strings.Join(opts.Args(), " ")

	if !strings.Contains(got, "pulsesrc device=castr_cast_2.monitor") {
		t.Errorf("args = %q, want the cast's own source captured", got)
	}
	// The pipeline must not run on the sound card's clock: a capture driven by
	// a source-provided clock froze (the capture freeze of 2026-11-06).
	if !strings.Contains(got, "provide-clock=false") {
		t.Errorf("args = %q, want the audio source kept off the pipeline clock", got)
	}
	if strings.Contains(got, "audiotestsrc") {
		t.Errorf("args = %q, want the silent track replaced", got)
	}
	want := "pulsesrc device=castr_cast_2.monitor do-timestamp=true provide-clock=false " +
		"! audioconvert ! audioresample ! audio/x-raw,rate=44100,channels=2 " +
		"! avenc_aac ! aacparse ! hls.audio"
	if !strings.Contains(got, want) {
		t.Errorf("args = %q, want the audio branch %q", got, want)
	}
}

func TestAChromecastWithoutSoundKeepsItsSilentTrack(t *testing.T) {
	off := hlsOptions()
	off.Audio = false
	off.AudioSource = "castr_cast_2.monitor"

	noSource := hlsOptions()
	noSource.Audio = true

	for name, opts := range map[string]Options{"audio off": off, "no source": noSource} {
		got := strings.Join(opts.Args(), " ")
		if !strings.Contains(got, "audiotestsrc wave=silence") || !strings.Contains(got, "hls.audio") {
			t.Errorf("%s: args = %q, want the silent track", name, got)
		}
		if !strings.Contains(got, silentTrack) {
			t.Errorf("%s: args = %q, want the silent track unchanged:\n%s", name, got, silentTrack)
		}
		if strings.Contains(got, "pulsesrc") {
			t.Errorf("%s: args = %q, want no sound captured", name, got)
		}
	}
}
