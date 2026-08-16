// Package session holds what a single cast is doing.
//
// The daemon owns these; every client -- CLI, TUI, bar widget -- reads them
// over IPC. One source of truth is why those surfaces never disagree.
package session

import (
	"fmt"
	"time"

	"github.com/mrCode/castr/internal/discovery"
)

// Cast modes live here, next to the Session that carries one, so the core
// packages never import a backend just to know what a mode is.
const (
	ModeMirror = "mirror"
	ModeExtend = "extend"
)

// Modes lists every valid mode, for validating a request.
var Modes = []string{ModeMirror, ModeExtend}

// ValidMode reports whether s names a cast mode.
func ValidMode(s string) bool {
	for _, m := range Modes {
		if m == s {
			return true
		}
	}
	return false
}

// State is where a cast has got to.
type State string

const (
	Idle State = "idle"
	// Connecting covers everything up to pixels actually flowing.
	Connecting State = "connecting"
	// AwaitingPin means the receiver is showing a code the user must enter.
	AwaitingPin State = "awaiting_pin"
	// Streaming is reported ONLY once the backend has seen capture start.
	//
	// doubletake prints "mirror session ready" roughly four seconds before
	// "screen capture started". Treating the former as ready reported a
	// stream that did not yet exist, and would have masked a capture failure
	// as success. Whatever decides to call Transition(Streaming) must wait for
	// the real marker.
	Streaming State = "streaming"
	Stopping  State = "stopping"
	Failed    State = "failed"
)

// allowed lists the legal edges. Failed is deliberately absent: it is
// reachable from anywhere, since a backend can die at any moment.
var allowed = map[State][]State{
	Idle:        {Connecting},
	Connecting:  {AwaitingPin, Streaming, Stopping},
	AwaitingPin: {Streaming, Stopping},
	Streaming:   {Stopping},
	Stopping:    {Idle},
	// A failed session may be retried, or simply cleared.
	Failed: {Idle, Connecting},
}

// active are the states that mean "a cast is under way", which is what keeps
// the daemon's idle watchdog from exiting underneath one.
var active = map[State]bool{Connecting: true, AwaitingPin: true, Streaming: true}

// ErrInvalidTransition is returned for an edge that is not declared.
//
// Backends emit from background goroutines, so a late or duplicate emit is
// normal and must not be treated as a crash: callers log it and carry on.
type ErrInvalidTransition struct {
	From, To State
}

func (e ErrInvalidTransition) Error() string {
	return fmt.Sprintf("invalid transition: %s -> %s", e.From, e.To)
}

// Clock is injected so tests never depend on wall time.
type Clock func() time.Time

// Session is one cast to one receiver.
type Session struct {
	Device discovery.Device
	Mode   string
	State  State
	// Err carries the reason for Failed, and is cleared by any other state so
	// a stale message cannot outlive the failure it described.
	Err string
	// StartedAt is set when streaming begins and cleared when it ends.
	StartedAt time.Time

	now Clock
}

// New returns an idle session for a device.
func New(device discovery.Device, mode string, now Clock) *Session {
	if now == nil {
		now = time.Now
	}
	if mode == "" {
		mode = ModeMirror
	}
	return &Session{Device: device, Mode: mode, State: Idle, now: now}
}

// IsActive reports whether a cast is under way.
func (s *Session) IsActive() bool { return active[s.State] }

// Transition moves to a new state, or returns ErrInvalidTransition.
func (s *Session) Transition(to State, reason string) error {
	if to != Failed && !s.permits(to) {
		return ErrInvalidTransition{From: s.State, To: to}
	}

	if to == Failed {
		s.Err = reason
	} else {
		s.Err = ""
	}

	switch to {
	case Streaming:
		s.StartedAt = s.now()
	case Idle, Failed:
		s.StartedAt = time.Time{}
	}

	s.State = to
	return nil
}

func (s *Session) permits(to State) bool {
	for _, candidate := range allowed[s.State] {
		if candidate == to {
			return true
		}
	}
	return false
}
