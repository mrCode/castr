// Package airplay supervises doubletake, which does the actual AirPlay work.
//
// castr does not implement AirPlay. It runs one doubletake child per session,
// reads its output to learn what happened, and cleans up after it.
package airplay

import (
	"strconv"
	"strings"
	"sync"
)

// Binary is the program we supervise.
const Binary = "doubletake"

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
const PinPrompt = "Enter the PIN shown on Apple TV"

// Config is the subset of castr's configuration doubletake needs.
type Config struct {
	PortRange       string
	FPS             int
	Encoder         string
	Bitrate         int
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
func BuildArgv(cfg Config, address, credsPath string) []string {
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

	if cfg.Bitrate > 0 {
		argv = append(argv, "-bitrate", strconv.Itoa(cfg.Bitrate))
	}
	if cfg.TargetLatencyMS > 0 {
		argv = append(argv, "-target-latency-ms", strconv.Itoa(cfg.TargetLatencyMS))
	}
	if !cfg.Audio {
		argv = append(argv, "-no-audio")
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

	return argv
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
func (s *Scanner) NeedsPin() bool { return s.contains(PinPrompt) }

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
