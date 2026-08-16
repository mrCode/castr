// Package config reads castr's TOML configuration and the receivers the user
// registered by hand.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Encoders castr knows how to ask doubletake for.
var Encoders = []string{"auto", "vaapi", "nvenc", "x264"}

// Capture holds the settings shared by every backend.
type Capture struct {
	FPS     int    `toml:"fps"`
	Encoder string `toml:"encoder"`
}

// AirPlay holds doubletake's settings.
type AirPlay struct {
	// PortRange is honoured because the AirPlay backend runs doubletake
	// directly, where -port-range works. It is silently ignored in
	// doubletake's daemon mode, which is why castr does not use daemon mode.
	//
	// Fixed rather than ephemeral: the receiver connects BACK into these
	// ports, so a random port in the ephemeral range cannot be allowed
	// through a firewall ahead of time.
	PortRange string `toml:"port_range"`

	// Bitrate of 0 leaves the choice to doubletake.
	Bitrate int `toml:"bitrate"`

	// Code is the receiver's fixed pairing code, if it has one.
	Code string `toml:"code"`

	// HideVAPostproc makes doubletake fall back to videoconvert.
	//
	// Its capture pipeline uses vapostproc to import the portal's DMA-BUF. On
	// Hyprland the buffer is padded -- 16 MiB for a 2560x1600 RGBA frame
	// against a 15.6 MiB descriptor -- and GStreamer's VA allocator refuses
	// it, producing a silent black screen on the receiver. Costs some CPU.
	HideVAPostproc bool `toml:"hide_vapostproc"`

	// ReadyTimeout is how long to wait for "screen capture started".
	//
	// 30s was too tight: measured on an AppleTV11,1, capture began 23s after
	// "mirror session ready", and extend adds a portal round-trip on top, so
	// extend timed out repeatedly on a machine where mirror just squeaked
	// through. Raising it costs nothing when a cast succeeds -- the wait ends
	// at the marker, not at the ceiling.
	ReadyTimeout float64 `toml:"ready_timeout"`

	// TargetLatencyMS is doubletake's -target-latency-ms: the end-to-end
	// delay the sender aims for, which the receiver buffers to. Lower means a
	// more responsive cursor and less typing lag, at the cost of tolerance for
	// network jitter -- too low and the receiver stutters.
	TargetLatencyMS int `toml:"target_latency_ms"`

	// Audio off removes audio/video sync from the pipeline, which is worth
	// trying when mirroring a desktop for work rather than playing media.
	Audio bool `toml:"audio"`
}

// Config is everything castr reads from disk.
type Config struct {
	Capture Capture `toml:"capture"`
	AirPlay AirPlay `toml:"airplay"`
}

// Default returns the configuration castr uses with no file present.
func Default() Config {
	return Config{
		Capture: Capture{FPS: 30, Encoder: "auto"},
		AirPlay: AirPlay{
			PortRange:       "60000-60010",
			Bitrate:         0,
			HideVAPostproc:  true,
			ReadyTimeout:    60,
			TargetLatencyMS: 100,
			Audio:           true,
		},
	}
}

// Dir is where config.toml lives.
func Dir() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join(os.TempDir(), "castr")
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "castr")
}

// Path is the config file.
func Path() string { return filepath.Join(Dir(), "config.toml") }

// Load reads a config file, filling anything absent from the defaults.
//
// A missing file is not an error: castr works with no configuration at all,
// and demanding one would make a fresh install fail before it ever cast.
func Load(path string) (Config, error) {
	cfg := Default()

	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("reading %s: %w", path, err)
	}

	// Decoded ONTO the defaults, so a file that sets one key keeps sensible
	// values for the rest. Decoding into a zero struct gave fps 0 and an empty
	// port range to anyone who set only a bitrate.
	if err := toml.Unmarshal(raw, &cfg); err != nil {
		return Default(), fmt.Errorf("parsing %s: %w", path, err)
	}

	if err := cfg.Validate(); err != nil {
		return Default(), fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Validate rejects settings that would fail later in a way nobody could trace
// back to the config file.
func (c Config) Validate() error {
	if !validEncoder(c.Capture.Encoder) {
		return fmt.Errorf("invalid encoder %q; expected one of %s",
			c.Capture.Encoder, strings.Join(Encoders, ", "))
	}
	if c.Capture.FPS <= 0 {
		return fmt.Errorf("fps must be positive, got %d", c.Capture.FPS)
	}
	if err := validatePortRange(c.AirPlay.PortRange); err != nil {
		return err
	}
	if c.AirPlay.ReadyTimeout <= 0 {
		return fmt.Errorf("ready_timeout must be positive, got %v", c.AirPlay.ReadyTimeout)
	}
	if c.AirPlay.TargetLatencyMS < 0 {
		return fmt.Errorf("target_latency_ms cannot be negative, got %d", c.AirPlay.TargetLatencyMS)
	}
	if c.AirPlay.Bitrate < 0 {
		return fmt.Errorf("bitrate cannot be negative, got %d", c.AirPlay.Bitrate)
	}
	return nil
}

func validEncoder(name string) bool {
	for _, e := range Encoders {
		if e == name {
			return true
		}
	}
	return false
}

// minPorts is what one session actually consumes: doubletake was observed
// using UDP 60000-60002 and TCP 60003 for a single cast. A range narrower than
// that fails at handshake time with an error that names ports, not config.
const minPorts = 4

func validatePortRange(s string) error {
	lo, hi, ok := strings.Cut(s, "-")
	if !ok {
		return fmt.Errorf("port_range %q must look like 60000-60010", s)
	}
	low, err := strconv.Atoi(strings.TrimSpace(lo))
	if err != nil {
		return fmt.Errorf("port_range %q: %v is not a port number", s, lo)
	}
	high, err := strconv.Atoi(strings.TrimSpace(hi))
	if err != nil {
		return fmt.Errorf("port_range %q: %v is not a port number", s, hi)
	}
	if low < 1 || high > 65535 {
		return fmt.Errorf("port_range %q is outside 1-65535", s)
	}
	if high < low {
		return fmt.Errorf("port_range %q runs backwards", s)
	}
	if high-low+1 < minPorts {
		return fmt.Errorf("port_range %q is too narrow; one cast needs at least %d ports",
			s, minPorts)
	}
	return nil
}
