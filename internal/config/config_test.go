package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAMissingFileIsNotAnError(t *testing.T) {
	// A fresh install has no config. Demanding one would fail before the user
	// ever cast anything.
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.toml"))

	if err != nil {
		t.Fatalf("err = %v, want the defaults", err)
	}
	if cfg != Default() {
		t.Errorf("cfg = %+v, want the defaults", cfg)
	}
}

func TestSettingOneKeyKeepsTheDefaultsForTheRest(t *testing.T) {
	// Decoding into a zero struct gave fps 0 and an empty port range to anyone
	// who set only a bitrate -- a config file that made castr worse.
	cfg, err := Load(write(t, "[airplay]\nbitrate = 6000\n"))
	if err != nil {
		t.Fatal(err)
	}

	if cfg.AirPlay.Bitrate != 6000 {
		t.Errorf("bitrate = %d, want the value from the file", cfg.AirPlay.Bitrate)
	}
	if cfg.Capture.FPS != Default().Capture.FPS {
		t.Errorf("fps = %d, want the default %d", cfg.Capture.FPS, Default().Capture.FPS)
	}
	if cfg.AirPlay.PortRange != Default().AirPlay.PortRange {
		t.Errorf("port_range = %q, want the default", cfg.AirPlay.PortRange)
	}
	if !cfg.AirPlay.Audio {
		t.Error("audio was turned off by a file that never mentioned it")
	}
}

func TestEveryDocumentedKeyIsActuallyRead(t *testing.T) {
	cfg, err := Load(write(t, `
[capture]
fps = 50
encoder = "vaapi"

[airplay]
port_range = "50000-50020"
bitrate = 8000
code = "1234"
hide_vapostproc = false
ready_timeout = 90
target_latency_ms = 90
audio = false
`))
	if err != nil {
		t.Fatal(err)
	}

	want := Config{
		Capture: Capture{FPS: 50, Encoder: "vaapi"},
		AirPlay: AirPlay{PortRange: "50000-50020", Bitrate: 8000, Code: "1234",
			HideVAPostproc: false, ReadyTimeout: 90, TargetLatencyMS: 90, Audio: false},
		// Untouched by this file, so it keeps the defaults: settings are
		// decoded ONTO them, not into a zero struct.
		Chromecast: Default().Chromecast,
	}
	if cfg != want {
		t.Errorf("cfg  = %+v\nwant = %+v", cfg, want)
	}
}

func TestTheDefaultReadyTimeoutIsAtLeastSixtySeconds(t *testing.T) {
	// Capture began 23s after "mirror session ready" on a real Apple TV, and
	// extend adds a portal round-trip, so 30 timed out repeatedly.
	if got := Default().AirPlay.ReadyTimeout; got < 60 {
		t.Errorf("ready_timeout default = %v, want at least 60", got)
	}
}

func TestAudioIsOnByDefault(t *testing.T) {
	if !Default().AirPlay.Audio {
		t.Error("audio defaults off; a cast with no sound is a bug report")
	}
}

func TestAnInvalidEncoderIsRejectedByName(t *testing.T) {
	// Otherwise it reaches BuildArgv, which falls back to auto, and the user's
	// deliberate choice is silently ignored.
	_, err := Load(write(t, "[capture]\nencoder = \"quicksync\"\n"))

	if err == nil {
		t.Fatal("an unknown encoder was accepted")
	}
	if !strings.Contains(err.Error(), "quicksync") {
		t.Errorf("err = %q, want it to name the bad value", err)
	}
}

func TestAMalformedFileIsReportedRatherThanHalfApplied(t *testing.T) {
	_, err := Load(write(t, "[capture\nfps = 30\n"))

	if err == nil {
		t.Fatal("a malformed file was accepted")
	}
}

func TestAnInvalidFileFallsBackToTheDefaultsRatherThanAHalfConfig(t *testing.T) {
	cfg, _ := Load(write(t, "[capture]\nfps = 60\nencoder = \"nonsense\"\n"))

	if cfg != Default() {
		t.Errorf("cfg = %+v, want the defaults, not a partly-applied file", cfg)
	}
}

func TestPortRangesThatCannotWorkAreRejected(t *testing.T) {
	// A receiver connects BACK into these ports. A range that is backwards,
	// out of bounds, or too narrow fails at handshake time with an error that
	// names ports and never mentions the config file.
	cases := []struct {
		name  string
		value string
		says  string // the word that tells the user what is actually wrong
	}{
		{"not a range", "60000", "must look like"},
		{"backwards", "60010-60000", "backwards"},
		{"too narrow for one cast", "60000-60001", "narrow"},
		{"above the port space", "60000-70000", "65535"},
		{"not numbers", "sixty-thousand", "port number"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Validated directly rather than through Load: Load wraps the
			// message in the config PATH, and t.TempDir() puts the subtest
			// name in that path -- so asserting on the wrapped string matched
			// the directory name instead of the diagnosis, and passed with the
			// check deleted.
			cfg := Default()
			cfg.AirPlay.PortRange = tc.value

			err := cfg.Validate()

			if err == nil {
				t.Fatalf("port_range %q was accepted", tc.value)
			}
			// A backwards range is also "too narrow", but being told so sends
			// the user looking for the wrong problem.
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("err = %q, want it to mention %q", err, tc.says)
			}
		})
	}
}

func TestAUsablePortRangeIsAccepted(t *testing.T) {
	cfg, err := Load(write(t, "[airplay]\nport_range = \"60000-60010\"\n"))

	if err != nil {
		t.Fatalf("a perfectly good port range was rejected: %v", err)
	}
	if cfg.AirPlay.PortRange != "60000-60010" {
		t.Errorf("port_range = %q", cfg.AirPlay.PortRange)
	}
}

func TestNonsensicalNumbersAreRejected(t *testing.T) {
	cases := map[string]string{
		"zero fps":           "[capture]\nfps = 0\n",
		"negative fps":       "[capture]\nfps = -30\n",
		"zero ready timeout": "[airplay]\nready_timeout = 0\n",
		"negative latency":   "[airplay]\ntarget_latency_ms = -1\n",
		"negative bitrate":   "[airplay]\nbitrate = -8000\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(write(t, body)); err == nil {
				t.Error("accepted a value that cannot work")
			}
		})
	}
}

func TestTheConfigDirectoryFollowsXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg-test")

	if got := Dir(); got != "/tmp/xdg-test/castr" {
		t.Errorf("Dir() = %q, want it under XDG_CONFIG_HOME", got)
	}
}

func TestALatencyTargetLowEnoughToBreakCastsIsRaised(t *testing.T) {
	// 50 looked like a responsiveness tweak and was a stability bug: the Apple
	// TV closed the stream after ~30s with "writev: broken pipe", which reads
	// as a network fault and sent two sessions chasing firewalls. At 100 the
	// same cast ran five and a half minutes unbroken.
	//
	// Raised rather than rejected, because rejecting throws away the encoder
	// and fps in the same file -- and this value arrives by migration from an
	// app where it was tuned before anyone knew what it did.
	cfg, err := Load(write(t, "[capture]\nencoder = \"vaapi\"\n\n[airplay]\ntarget_latency_ms = 50\n"))

	if cfg.AirPlay.TargetLatencyMS < MinSafeLatencyMS {
		t.Errorf("target_latency_ms = %d, want it raised to at least %d",
			cfg.AirPlay.TargetLatencyMS, MinSafeLatencyMS)
	}
	if cfg.Capture.Encoder != "vaapi" {
		t.Errorf("encoder = %q, want the rest of the file kept", cfg.Capture.Encoder)
	}
	if err == nil {
		t.Error("the repair was silent; the user should be told")
	} else if !strings.Contains(err.Error(), "hangs up") {
		t.Errorf("note = %q, want it to say what actually happens", err)
	}
}

func TestTheDefaultAndZeroAreBothAccepted(t *testing.T) {
	// 0 means "let doubletake decide", which is always safe.
	for _, v := range []int{0, MinSafeLatencyMS, 100, 250} {
		cfg := Default()
		cfg.AirPlay.TargetLatencyMS = v
		if err := cfg.Validate(); err != nil {
			t.Errorf("target_latency_ms = %d rejected: %v", v, err)
		}
	}
}

func TestChromecastDefaultsAreUsable(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the defaults do not validate: %v", err)
	}
	if cfg.Chromecast.Port == 0 {
		t.Error("no default port; a firewall rule cannot be written for a port nobody knows")
	}
	// A receiver refuses a stream above its decoder's limits with no useful
	// error, so the default must be a size receivers actually accept.
	if cfg.Chromecast.Width > 1920 || cfg.Chromecast.Height > 1080 {
		t.Errorf("default capture is %dx%d, larger than receivers reliably decode",
			cfg.Chromecast.Width, cfg.Chromecast.Height)
	}
}

func TestChromecastSizeMustBeSetAsAPair(t *testing.T) {
	cfg := Default()
	cfg.Chromecast.Height = 0
	if err := cfg.Validate(); err == nil {
		t.Fatal("a width with no height was accepted; the capsfilter would be malformed")
	}
}
