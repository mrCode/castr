package session

import (
	"errors"
	"testing"
	"time"

	"github.com/mrCode/castr/internal/discovery"
)

func device() discovery.Device {
	return discovery.Device{
		ID: "airplay:AA", Name: "Meeting Room", Address: "10.0.0.5",
		Port: 7000, Protocol: discovery.ProtocolAirPlay,
	}
}

// fixedClock keeps time out of the tests entirely.
func fixedClock(t time.Time) Clock { return func() time.Time { return t } }

func newSession(t *testing.T) *Session {
	t.Helper()
	return New(device(), ModeMirror, fixedClock(time.Unix(1000, 0)))
}

func mustTransition(t *testing.T, s *Session, states ...State) {
	t.Helper()
	for _, st := range states {
		if err := s.Transition(st, ""); err != nil {
			t.Fatalf("transition to %s: %v", st, err)
		}
	}
}

func TestStartsIdleAndMirrors(t *testing.T) {
	s := newSession(t)

	if s.State != Idle {
		t.Errorf("state = %s, want idle", s.State)
	}
	if s.Mode != ModeMirror {
		t.Errorf("mode = %s, want mirror", s.Mode)
	}
	if s.IsActive() {
		t.Error("an idle session must not count as active")
	}
}

func TestTheHappyPath(t *testing.T) {
	s := newSession(t)

	mustTransition(t, s, Connecting, Streaming, Stopping, Idle)
}

func TestPinPathIsAllowed(t *testing.T) {
	s := newSession(t)

	mustTransition(t, s, Connecting, AwaitingPin, Streaming)
}

func TestUndeclaredEdgesAreRejected(t *testing.T) {
	s := newSession(t)

	// Idle -> Streaming would mean reporting a stream that never connected.
	err := s.Transition(Streaming, "")

	var want ErrInvalidTransition
	if !errors.As(err, &want) {
		t.Fatalf("err = %v, want ErrInvalidTransition", err)
	}
	if s.State != Idle {
		t.Errorf("a rejected transition changed the state to %s", s.State)
	}
}

func TestFailedIsReachableFromAnywhere(t *testing.T) {
	// A backend can die at any moment, including before it ever connected.
	for _, from := range []State{Idle, Connecting, AwaitingPin, Streaming, Stopping} {
		s := newSession(t)
		s.State = from

		if err := s.Transition(Failed, "boom"); err != nil {
			t.Errorf("from %s: %v", from, err)
		}
	}
}

func TestFailureReasonIsKept(t *testing.T) {
	s := newSession(t)
	mustTransition(t, s, Connecting)

	if err := s.Transition(Failed, "doubletake exited"); err != nil {
		t.Fatal(err)
	}

	if s.Err != "doubletake exited" {
		t.Errorf("Err = %q, want the reason", s.Err)
	}
}

func TestAStaleFailureReasonDoesNotOutliveTheFailure(t *testing.T) {
	// Otherwise a retry shows the previous attempt's error while streaming.
	s := newSession(t)
	mustTransition(t, s, Connecting)
	if err := s.Transition(Failed, "network down"); err != nil {
		t.Fatal(err)
	}

	mustTransition(t, s, Connecting)

	if s.Err != "" {
		t.Errorf("Err = %q, want it cleared on leaving failed", s.Err)
	}
}

func TestAFailedSessionCanBeRetried(t *testing.T) {
	s := newSession(t)
	mustTransition(t, s, Connecting)
	if err := s.Transition(Failed, "boom"); err != nil {
		t.Fatal(err)
	}

	mustTransition(t, s, Connecting, Streaming)
}

func TestStreamingRecordsWhenItStarted(t *testing.T) {
	at := time.Unix(4242, 0)
	s := New(device(), ModeExtend, fixedClock(at))
	mustTransition(t, s, Connecting, Streaming)

	if !s.StartedAt.Equal(at) {
		t.Errorf("StartedAt = %v, want %v", s.StartedAt, at)
	}
}

func TestStartedAtIsClearedWhenTheCastEnds(t *testing.T) {
	s := newSession(t)
	mustTransition(t, s, Connecting, Streaming, Stopping, Idle)

	if !s.StartedAt.IsZero() {
		t.Errorf("StartedAt = %v, want zero once idle", s.StartedAt)
	}
}

func TestActiveStatesKeepTheDaemonAlive(t *testing.T) {
	// The idle watchdog uses this. If Streaming were ever excluded, the daemon
	// would exit underneath a live cast and take the session with it.
	cases := map[State]bool{
		Idle: false, Connecting: true, AwaitingPin: true,
		Streaming: true, Stopping: false, Failed: false,
	}

	for state, want := range cases {
		s := newSession(t)
		s.State = state

		if got := s.IsActive(); got != want {
			t.Errorf("IsActive(%s) = %v, want %v", state, got, want)
		}
	}
}

func TestValidMode(t *testing.T) {
	for _, m := range []string{ModeMirror, ModeExtend} {
		if !ValidMode(m) {
			t.Errorf("ValidMode(%q) = false", m)
		}
	}
	for _, m := range []string{"", "MIRROR", "clone", "extended"} {
		if ValidMode(m) {
			t.Errorf("ValidMode(%q) = true", m)
		}
	}
}

func TestModeDefaultsToMirror(t *testing.T) {
	if got := New(device(), "", nil).Mode; got != ModeMirror {
		t.Errorf("mode = %q, want mirror", got)
	}
}
