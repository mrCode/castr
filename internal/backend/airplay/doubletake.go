// Package airplay supervises castr-airplay, which does the actual AirPlay work.
//
// castr-airplay is doubletake, built by castr's own package from castr's branch
// of it (github.com/mrCode/doubletake, tags castr-airplay-*) and installed
// where nothing else looks. castr ships it rather than depending on whatever
// doubletake is installed because castr's security depends on it: doubletake
// upstream does not authenticate a receiver it has paired with, and the fix
// (omarroth/doubletake#67) had no answer for weeks. One child runs per
// session; castr reads its output to learn what happened, and cleans up after
// it.
package airplay

import (
	"os"
	"strconv"
	"strings"
	"sync"
)

// helperPath is where castr's package installs castr-airplay.
const helperPath = "/usr/lib/castr/castr-airplay"

// Binary is the program we supervise. CASTR_AIRPLAY overrides it, for running
// castr from a source checkout; nothing on PATH is ever picked up in its
// place, since any doubletake found there need not authenticate receivers.
var Binary = binary(os.Getenv)

func binary(getenv func(string) string) string {
	if path := getenv("CASTR_AIRPLAY"); path != "" {
		return path
	}
	return helperPath
}

// ReadyMarker is the ONLY line that means pixels are flowing.
//
// doubletake prints "mirror session ready" roughly four seconds earlier, as
// soon as the RTSP session is set up and before the capture pipeline starts --
// and before the screen-share prompt has necessarily been answered. Treating
// that as ready reported STREAMING for a stream that did not exist, and would
// have masked a capture failure as success.
const ReadyMarker = "screen capture started"

// ProgressMarkers are useful for diagnostics and never mean ready.
var ProgressMarkers = []string{"mirror session ready", "FairPlay setup complete"}

// PinPrompt is printed with no trailing newline, so output must be scanned as
// chunks rather than lines.
//
// This is the wording of doubletake up to 0.4.x. PinPrompts carries every
// wording doubletake has used since, because the daemon is pinned to none of
// them: 0.5.0 says "receiver" rather than "Apple TV", and prompts for a
// configured password with its own sentence. Missing a prompt leaves the
// session in connecting forever and `castr pin` refusing the code the
// television is showing.
const PinPrompt = "Enter the PIN shown on Apple TV"

// PinPrompts are the prompts doubletake prints when it is blocked on stdin
// waiting for a pairing code or password. Any of them means the receiver
// wants a credential from the user.
var PinPrompts = []string{
	PinPrompt,
	"Enter the PIN shown on the receiver", // doubletake 0.5.0
	"Enter the receiver's configured password or pairing PIN",          // 0.5.0, display failed
	"Enter the receiver's configured password",                         // 0.5.0, password-only receiver
	"Enter the code shown on the receiver, or its configured password", // 0.5.0, late Digest challenge
}

// The latency castr picks when the user has not set target_latency_ms. The
// value is a helper argument, so it is fixed when the cast starts.
//
// Measured on an Apple TV over busy office Wi-Fi: at 100 ms the TV's sound had
// small glitches, at 150 it was almost clean (200 likewise). Without sound
// there is nothing to glitch, so 100 is kept for responsiveness.
const (
	AutoLatencyNoSoundMS   = 100
	AutoLatencyWithSoundMS = 150
)

// Config is the subset of castr's configuration doubletake needs.
type Config struct {
	PortRange string
	FPS       int
	Encoder   string
	Bitrate   int
	// TargetLatencyMS is the user's setting; 0 means not set, and BuildArgv
	// then picks AutoLatencyNoSoundMS or AutoLatencyWithSoundMS by Audio.
	TargetLatencyMS int
	Audio           bool
}

// hwaccel maps castr's encoder vocabulary onto doubletake's.
var hwaccel = map[string]string{
	"auto": "auto", "vaapi": "vaapi", "nvenc": "nvenc", "x264": "none",
}

// BuildArgv assembles the command for one session.
//
// It always uses -target, never -daemonize. doubletake's daemon config carries
// no port fields, so under -daemonize the -port-range flag is silently dropped
// and the receiver's reverse handshake lands on random ephemeral ports, which
// a default-DROP firewall discards: SETUP stalls or returns HTTP 401. Measured
// on the same Apple TV with identical flags:
//
//	-daemonize   UDP 36760-36762, TCP 45771   -> stalls, fails
//	-target      UDP 60000-60002, TCP 60003   -> mirrors successfully
//
// port is the receiver's AirPlay port as discovered. Most listen on 7000,
// doubletake's default, but not all: a REDMI display advertises 47000 and
// refuses 7000 (issue #2). 0 means unknown and leaves doubletake's default.
//
// deviceID is castr's id for the receiver. Its AirPlay device ID -- what the
// receiver advertised over mDNS when castr found it -- is passed as -device-id,
// so the receiver's saved pairing applies even if whatever answers at the
// address names itself differently, and such a device is refused. Without it,
// an impersonator could name itself as a receiver never seen before and be
// paired with from scratch.
//
// audioSource is the PulseAudio source carrying this cast's sound, the
// monitor of the output castr routed the cast to. Empty leaves the helper to
// capture the default output's monitor, as it always has. It is ignored when
// audio is off.
func BuildArgv(cfg Config, deviceID, address string, port int, credsPath, audioSource string) []string {
	accel, ok := hwaccel[cfg.Encoder]
	if !ok {
		accel = "auto"
	}

	argv := []string{
		Binary,
		"-target", address,
		"-port-range", cfg.PortRange,
		"-fps", strconv.Itoa(cfg.FPS),
		"-hwaccel", accel,
	}
	if port > 0 {
		argv = append(argv, "-port", strconv.Itoa(port))
	}

	if cfg.Bitrate > 0 {
		argv = append(argv, "-bitrate", strconv.Itoa(cfg.Bitrate))
	}
	latency := cfg.TargetLatencyMS
	if latency <= 0 {
		latency = AutoLatencyNoSoundMS
		if cfg.Audio {
			latency = AutoLatencyWithSoundMS
		}
	}
	argv = append(argv, "-target-latency-ms", strconv.Itoa(latency))
	if !cfg.Audio {
		argv = append(argv, "-no-audio")
	} else if audioSource != "" {
		argv = append(argv, "-audio-source", audioSource)
	}

	// AirPlay PAIRING credentials -- the keys handed over after the user types
	// the code shown on the receiver. The file is keyed by receiver, so one
	// file serves every receiver and both modes.
	//
	// castr split this per mode at first, on the mistaken belief that it held
	// a screen-share portal token. It does not, and the split meant pairing
	// with the same television twice: once for mirror, again for extend.
	if credsPath != "" {
		argv = append(argv, "-creds", credsPath)
	}
	if id := airPlayDeviceID(deviceID, address); id != "" {
		argv = append(argv, "-device-id", id)
	}

	return argv
}

// airPlayDeviceID is the receiver's advertised device ID within castr's id,
// "airplay:<deviceid>". A receiver that advertised none is known by its
// address instead, which says nothing about which device it is, so nothing
// is passed.
//
// A receiver the user added by address is "airplay:manual:<address>". That is
// castr's name for an address, not an ID any receiver advertised, and no
// pairing is ever saved under it, so nothing is passed for it either: the
// helper then binds the receiver to its address itself.
func airPlayDeviceID(deviceID, address string) string {
	id, ok := strings.CutPrefix(deviceID, "airplay:")
	if !ok || id == "" || id == address || strings.HasPrefix(id, "manual:") {
		return ""
	}
	return id
}

// maxBuffer bounds what we keep of a child's output. Enough for a diagnostic
// tail, bounded so a chatty child cannot grow without limit.
const maxBuffer = 16384

// Scanner accumulates a child's merged output and reports what it has seen.
//
// It scans accumulated text rather than whole lines because the PIN prompt
// arrives without a newline and would otherwise never be noticed.
//
// Safe for concurrent use: one goroutine reads the child's output into it
// while another waits on what it has seen.
type Scanner struct {
	mu   sync.Mutex
	text string
}

// Absorb adds a chunk of output.
func (s *Scanner) Absorb(chunk string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.text += chunk
	if len(s.text) > maxBuffer {
		s.text = s.text[len(s.text)-maxBuffer:]
	}
}

// Ready reports whether capture has actually started.
func (s *Scanner) Ready() bool { return s.contains(ReadyMarker) }

// NeedsPin reports whether the receiver is waiting for a pairing code.
func (s *Scanner) NeedsPin() bool {
	for _, prompt := range PinPrompts {
		if s.contains(prompt) {
			return true
		}
	}
	return false
}

func (s *Scanner) contains(needle string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Contains(s.text, needle)
}

// PortalFailure returns doubletake's own screen-capture error, if it printed
// one, and the empty string otherwise.
//
// This exists because castr used to discard it and guess. The guess was wrong
// three times in one session -- twice while doubletake had already printed the
// real reason, which is what an unanswered share prompt looks like:
//
//	screen capture failed: screencast portal: timeout waiting for portal response
func (s *Scanner) PortalFailure() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, line := range strings.Split(s.text, "\n") {
		if strings.Contains(line, "screen capture failed") ||
			(strings.Contains(line, "portal") && strings.Contains(line, "capture")) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// Tail returns the last of the output, for diagnostics.
func (s *Scanner) Tail(limit int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	flat := strings.Join(strings.Fields(s.text), " ")
	if len(flat) > limit {
		return flat[len(flat)-limit:]
	}
	return flat
}
