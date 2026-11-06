package capture

import (
	"os"
	"strings"
	"syscall"
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

// element returns the properties given to the first element named name, up to
// the next link.
func element(args []string, name string) []string {
	for i, arg := range args {
		if arg != name {
			continue
		}
		end := i + 1
		for end < len(args) && args[end] != "!" {
			end++
		}
		return args[i+1 : end]
	}
	return nil
}

// The screen's clock follows the PipeWire stream, and that stream stops or
// steps back whenever the portal pauses and renegotiates it. A pipeline
// running on it waits on a time that never comes: the capture freeze of
// 2026-11-06. With sound or without, the pipeline runs on the system clock.
func TestTheChromecastPipelineRunsOnTheSystemClock(t *testing.T) {
	withSound := hlsOptions()
	withSound.Audio = true
	withSound.AudioSource = "castr_cast_2.monitor"

	for name, opts := range map[string]Options{"with sound": withSound, "silent": hlsOptions()} {
		props := element(opts.Args(), "pipewiresrc")
		if props == nil {
			t.Fatalf("%s: no pipewiresrc in %q", name, opts.Args())
		}
		if !strings.Contains(strings.Join(props, " "), "provide-clock=false") {
			t.Errorf("%s: pipewiresrc %q, want provide-clock=false", name, props)
		}
	}
}

func TestThePipelineDiesWithCastrd(t *testing.T) {
	// castrd killed with SIGKILL runs no cleanup; the kernel sends the
	// pipeline SIGTERM instead. The group stays, so a stop still ends it all.
	opts := hlsOptions()
	opts.FD = os.Stdin
	cmd, err := opts.Command("gst-launch-1.0")
	if err != nil {
		t.Fatal(err)
	}
	if a := cmd.SysProcAttr; a == nil || a.Pdeathsig != syscall.SIGTERM || !a.Setpgid {
		t.Errorf("SysProcAttr %+v, want Pdeathsig SIGTERM and Setpgid", a)
	}
}
