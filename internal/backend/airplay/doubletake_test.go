package airplay

import (
	"strings"
	"testing"
)

func config() Config {
	return Config{
		PortRange: "60000-60010", FPS: 30, Encoder: "vaapi",
		TargetLatencyMS: 100, Audio: true,
	}
}

func argvString(argv []string) string { return strings.Join(argv, " ") }

func TestNeverUsesDaemonMode(t *testing.T) {
	// doubletake's daemon config carries no port fields, so -port-range is
	// silently dropped and the receiver's reverse handshake lands on random
	// ephemeral ports that a default-DROP firewall discards. Measured: the
	// same Apple TV stalls under -daemonize and mirrors fine under -target.
	got := argvString(BuildArgv(config(), "10.0.0.5", ""))

	if strings.Contains(got, "-daemonize") {
		t.Fatalf("argv uses daemon mode: %q", got)
	}
	if !strings.Contains(got, "-target 10.0.0.5") {
		t.Errorf("argv = %q, want -target", got)
	}
}

func TestPortRangeIsAlwaysPassed(t *testing.T) {
	// Without it the receiver connects back on ephemeral ports no firewall
	// rule can anticipate, and AirPlay hangs with no error.
	got := argvString(BuildArgv(config(), "10.0.0.5", ""))

	if !strings.Contains(got, "-port-range 60000-60010") {
		t.Errorf("argv = %q, want the configured port range", got)
	}
}

func TestEncoderVocabularyIsTranslated(t *testing.T) {
	cases := map[string]string{
		"auto": "auto", "vaapi": "vaapi", "nvenc": "nvenc",
		"x264":     "none", // castr calls it x264; doubletake calls it none
		"nonsense": "auto", // never pass through an unknown value
	}

	for encoder, want := range cases {
		cfg := config()
		cfg.Encoder = encoder

		got := argvString(BuildArgv(cfg, "10.0.0.5", ""))

		if !strings.Contains(got, "-hwaccel "+want) {
			t.Errorf("encoder %q gave %q, want -hwaccel %s", encoder, got, want)
		}
	}
}

func TestOptionalFlagsAreOmittedWhenUnset(t *testing.T) {
	cfg := config()
	cfg.Bitrate = 0

	got := argvString(BuildArgv(cfg, "10.0.0.5", ""))

	if strings.Contains(got, "-bitrate") {
		t.Errorf("argv = %q, want no -bitrate when it is zero", got)
	}
	if strings.Contains(got, "-creds") {
		t.Errorf("argv = %q, want no -creds when none is given", got)
	}
}

func TestAudioIsDisabledOnlyWhenAskedFor(t *testing.T) {
	cfg := config()
	cfg.Audio = false

	if !strings.Contains(argvString(BuildArgv(cfg, "10.0.0.5", "")), "-no-audio") {
		t.Error("want -no-audio when audio is off")
	}
	if strings.Contains(argvString(BuildArgv(config(), "10.0.0.5", "")), "-no-audio") {
		t.Error("want audio streamed by default")
	}
}

func TestCredentialsPathIsPassedWhenGiven(t *testing.T) {
	// Each mode captures a different virtual output, so each needs its own
	// token. Reusing the default would replay one pointing at the real panel.
	got := argvString(BuildArgv(config(), "10.0.0.5", "/state/mirror.json"))

	if !strings.Contains(got, "-creds /state/mirror.json") {
		t.Errorf("argv = %q, want the credentials path", got)
	}
}

// -- output scanning --------------------------------------------------------

func TestSessionReadyIsNotStreaming(t *testing.T) {
	// The four-second trap: this line arrives before the capture pipeline runs
	// and before the share prompt has necessarily been answered.
	var s Scanner
	s.Absorb("connected to: Meeting Room\n")
	s.Absorb("FairPlay setup complete\n")
	s.Absorb("mirror session ready (data port: 49277)\n")

	if s.Ready() {
		t.Error("'mirror session ready' must not count as streaming")
	}
}

func TestCaptureStartedIsStreaming(t *testing.T) {
	var s Scanner
	s.Absorb("mirror session ready (data port: 49277)\n")
	s.Absorb("screen capture started\n")

	if !s.Ready() {
		t.Error("'screen capture started' means pixels are flowing")
	}
}

func TestPinPromptIsSeenWithoutANewline(t *testing.T) {
	// doubletake prints it with fmt.Print, so a line-based scanner never sees
	// it and the session hangs until the ready timeout.
	var s Scanner
	s.Absorb("pairing required. " + PinPrompt + ": ")

	if !s.NeedsPin() {
		t.Error("the PIN prompt was missed because it has no trailing newline")
	}
}

func TestPortalFailureIsReportedVerbatim(t *testing.T) {
	// castr used to discard this and guess, and the guess blamed the firewall
	// while doubletake had already printed the real reason.
	var s Scanner
	s.Absorb("mirror session ready (data port: 49217)\n")
	s.Absorb("screen capture failed: screencast portal: session response: " +
		"timeout waiting for portal response\n")

	got := s.PortalFailure()

	if !strings.Contains(got, "screencast portal") {
		t.Errorf("PortalFailure() = %q, want doubletake's own message", got)
	}
}

func TestNoPortalFailureWhenThingsAreFine(t *testing.T) {
	var s Scanner
	s.Absorb("screen capture started\naudio capture started\n")

	if got := s.PortalFailure(); got != "" {
		t.Errorf("PortalFailure() = %q, want empty", got)
	}
}

func TestMarkersSplitAcrossChunksAreStillFound(t *testing.T) {
	// Output arrives in arbitrary chunks from a pipe, not tidy lines.
	var s Scanner
	s.Absorb("screen capt")
	s.Absorb("ure started\n")

	if !s.Ready() {
		t.Error("a marker split across reads was missed")
	}
}

func TestOutputIsBounded(t *testing.T) {
	// A chatty child must not grow the buffer without limit.
	var s Scanner
	for i := 0; i < 500; i++ {
		s.Absorb(strings.Repeat("noise ", 100))
	}

	if len(s.text) > maxBuffer {
		t.Errorf("buffer = %d bytes, want at most %d", len(s.text), maxBuffer)
	}
}

func TestReadinessSurvivesBufferTrimming(t *testing.T) {
	// Trimming must not silently drop a marker the caller still needs. The
	// recent tail is what matters, so a late marker stays visible.
	var s Scanner
	s.Absorb(strings.Repeat("noise ", 5000))
	s.Absorb(ReadyMarker + "\n")

	if !s.Ready() {
		t.Error("the ready marker was trimmed away")
	}
}

func TestTailIsWhitespaceNormalised(t *testing.T) {
	var s Scanner
	s.Absorb("error:   something\n\n  went    wrong\n")

	if got := s.Tail(200); strings.Contains(got, "\n") || strings.Contains(got, "  ") {
		t.Errorf("Tail() = %q, want a flat single-spaced line", got)
	}
}
