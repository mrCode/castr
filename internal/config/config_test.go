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
geometry = "2560x1440@60"
codec = "hevc"

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
		Capture: Capture{FPS: 50, Encoder: "vaapi", Geometry: "2560x1440@60",
			GeometrySet: true, Codec: "hevc"},
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

func TestUnsetLatencyStaysUnsetAndIsSilent(t *testing.T) {
	// "Not set" must reach the backend as 0 so it can pick per cast; it must
	// not be mistaken for a too-low value and raised, or warned about.
	cfg, err := Load(write(t, "[airplay]\naudio = true\n"))
	if err != nil {
		t.Fatalf("unset target_latency_ms produced a note: %v", err)
	}
	if cfg.AirPlay.TargetLatencyMS != 0 {
		t.Errorf("target_latency_ms = %d, want 0 (unset)", cfg.AirPlay.TargetLatencyMS)
	}
	if Default().AirPlay.TargetLatencyMS != 0 {
		t.Errorf("default target_latency_ms = %d, want 0 (unset)", Default().AirPlay.TargetLatencyMS)
	}
}

func TestExplicitLatencyIsKept(t *testing.T) {
	cfg, err := Load(write(t, "[airplay]\ntarget_latency_ms = 120\n"))
	if err != nil || cfg.AirPlay.TargetLatencyMS != 120 {
		t.Errorf("got %d, %v; want 120, nil", cfg.AirPlay.TargetLatencyMS, err)
	}
}

func TestGeometryDefaultsToToday(t *testing.T) {
	// Defaults change nothing: with no geometry in the file, extend stays at
	// 1920x1080@60 and the helper is given no cap.
	if got := Default().Capture.Geometry; got != "1920x1080@60" {
		t.Errorf("default geometry = %q, want 1920x1080@60", got)
	}

	cfg, err := Load(write(t, "[capture]\nfps = 30\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Capture.Geometry != "1920x1080@60" {
		t.Errorf("geometry = %q, want the default", cfg.Capture.Geometry)
	}
	if got := cfg.Capture.MaxSize(); got != "" {
		t.Errorf("max size = %q for a file that never mentioned geometry, want none", got)
	}
}

func TestAnExplicitGeometryIsKeptAndCapsTheStream(t *testing.T) {
	cfg, err := Load(write(t, "[capture]\ngeometry = \"2560x1440@120\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Capture.Geometry != "2560x1440@120" {
		t.Errorf("geometry = %q, want the value from the file", cfg.Capture.Geometry)
	}
	if got := cfg.Capture.MaxSize(); got != "2560x1440" {
		t.Errorf("max size = %q, want 2560x1440", got)
	}

	// Written out as the default is still the user's choice, and caps.
	cfg, err = Load(write(t, "[capture]\ngeometry = \"1920x1080@60\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Capture.MaxSize(); got != "1920x1080" {
		t.Errorf("max size = %q, want 1920x1080 for an explicit geometry", got)
	}
}

func TestParseGeometry(t *testing.T) {
	g, err := ParseGeometry("3840x2160@30")
	if err != nil {
		t.Fatal(err)
	}
	if g != (Geometry{Width: 3840, Height: 2160, Refresh: 30}) {
		t.Errorf("parsed %+v", g)
	}
	if got := g.String(); got != "3840x2160@30" {
		t.Errorf("String() = %q, want WxH@R", got)
	}
	for _, edge := range []string{"640x360@24", "7680x4320@240"} {
		if _, err := ParseGeometry(edge); err != nil {
			t.Errorf("%q rejected: %v", edge, err)
		}
	}
	for _, bad := range []string{"", "auto", "4k", "1920x1080", "1920x1080@", "x1080@60",
		"639x360@60", "640x359@60", "7681x4320@60", "640x4321@60", "640x360@23",
		"640x360@241", "1920x1080@59.94", "+1920x1080@60", "1920x-1080@60"} {
		if _, err := ParseGeometry(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestAnInvalidGeometryFallsBackWithAWarning(t *testing.T) {
	// Repaired, not rejected: a bad geometry must not throw away the encoder
	// in the same file, and must not stop anyone casting.
	for _, bad := range []string{"4k", "100x100@60", "1920x1080@500", "auto"} {
		t.Run(bad, func(t *testing.T) {
			cfg, err := Load(write(t,
				"[capture]\nencoder = \"vaapi\"\ngeometry = \""+bad+"\"\n"))

			if cfg.Capture.Geometry != "1920x1080@60" {
				t.Errorf("geometry = %q, want the 1920x1080@60 fallback", cfg.Capture.Geometry)
			}
			if got := cfg.Capture.MaxSize(); got != "" {
				t.Errorf("max size = %q, want none after falling back", got)
			}
			if cfg.Capture.Encoder != "vaapi" {
				t.Errorf("encoder = %q, want the rest of the file kept", cfg.Capture.Encoder)
			}
			if err == nil {
				t.Fatal("the fallback was silent; the user should be told")
			}
			if !strings.Contains(err.Error(), `"`+bad+`"`) {
				t.Errorf("note = %q, want it to name the value %q", err, bad)
			}
		})
	}
}

func TestCodecIsValidatedAndDefaultsToAuto(t *testing.T) {
	if got := Default().Capture.Codec; got != "auto" {
		t.Errorf("default codec = %q, want auto", got)
	}

	for in, want := range map[string]string{
		"auto": "auto", "h264": "h264", "hevc": "hevc", "HEVC": "hevc", "H264": "h264",
	} {
		cfg, err := Load(write(t, "[capture]\ncodec = \""+in+"\"\n"))
		if err != nil {
			t.Errorf("codec %q: %v", in, err)
		}
		if cfg.Capture.Codec != want {
			t.Errorf("codec %q gave %q, want %q", in, cfg.Capture.Codec, want)
		}
	}

	cfg, err := Load(write(t, "[capture]\nencoder = \"vaapi\"\ncodec = \"av1\"\n"))
	if cfg.Capture.Codec != "auto" {
		t.Errorf("codec = %q, want the auto fallback", cfg.Capture.Codec)
	}
	if cfg.Capture.Encoder != "vaapi" {
		t.Errorf("encoder = %q, want the rest of the file kept", cfg.Capture.Encoder)
	}
	if err == nil {
		t.Fatal("the fallback was silent; the user should be told")
	}
	if !strings.Contains(err.Error(), `"av1"`) {
		t.Errorf("note = %q, want it to name the value", err)
	}
}
