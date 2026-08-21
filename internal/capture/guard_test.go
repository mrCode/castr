package capture

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeGraph returns a different answer on each look, so a test can describe a
// pipeline that starts empty and then links -- the ordinary case.
type fakeGraph struct {
	answers [][]Node
	err     error
	looks   int
}

func (f *fakeGraph) SourcesFeeding(int) ([]Node, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.looks++
	if len(f.answers) == 0 {
		return nil, nil
	}
	i := f.looks - 1
	if i >= len(f.answers) {
		i = len(f.answers) - 1
	}
	return f.answers[i], nil
}

func guard(g Graph) *Guard {
	return &Guard{
		Graph: g, Granted: 76,
		Timeout: time.Second, Interval: time.Millisecond,
		Sleep: func(time.Duration) {}, // tests do not wait in real seconds
	}
}

var screen = Node{ID: 76, Name: "xdg-desktop-portal-hyprland", Class: "Video/Source"}
var webcam = Node{ID: 46, Name: "v4l2_input.pci-0000_00_14.0-usb-0_7_1.0",
	Class: "Video/Source", Role: "Camera"}

func TestVerifyAcceptsTheGrantedNode(t *testing.T) {
	if err := guard(&fakeGraph{answers: [][]Node{{screen}}}).Verify(context.Background(), 1); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// The bug this whole package exists for: the pipeline links to the webcam and
// everything else looks healthy.
func TestVerifyRefusesACamera(t *testing.T) {
	err := guard(&fakeGraph{answers: [][]Node{{webcam}}}).Verify(context.Background(), 1)
	if !errors.Is(err, ErrWrongSource) {
		t.Fatalf("got %v, want ErrWrongSource", err)
	}
	// The message has to name what was about to be broadcast. "Capture
	// failed" would send the reader to their encoder settings.
	for _, want := range []string{"role=Camera", "Nothing has been sent"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q does not mention %q", err, want)
		}
	}
}

// A pipeline linked to the screen AND a camera must fail. Passing on the
// strength of the correct half would broadcast the other half.
func TestVerifyRefusesAMixOfTheScreenAndACamera(t *testing.T) {
	err := guard(&fakeGraph{answers: [][]Node{{screen, webcam}}}).Verify(context.Background(), 1)
	if !errors.Is(err, ErrWrongSource) {
		t.Fatalf("got %v, want ErrWrongSource", err)
	}
}

// Encoders take a moment to negotiate. Checking once and failing would reject
// every healthy cast.
func TestVerifyWaitsForTheLinkToAppear(t *testing.T) {
	g := &fakeGraph{answers: [][]Node{nil, nil, {screen}}}
	if err := guard(g).Verify(context.Background(), 1); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if g.looks < 3 {
		t.Errorf("gave up after %d looks", g.looks)
	}
}

// A pipeline that never links produces no video. Reporting success would give
// the user a green status and a black television.
func TestVerifyFailsWhenNothingEverLinks(t *testing.T) {
	err := guard(&fakeGraph{}).Verify(context.Background(), 1)
	if !errors.Is(err, ErrNoSource) {
		t.Fatalf("got %v, want ErrNoSource", err)
	}
}

// If castr cannot read the graph it cannot tell what it is capturing, and an
// unreadable graph is not permission to continue.
func TestVerifyFailsWhenTheGraphCannotBeRead(t *testing.T) {
	err := guard(&fakeGraph{err: errors.New("pw-dump: not found")}).Verify(context.Background(), 1)
	if !errors.Is(err, ErrNoSource) {
		t.Fatalf("got %v, want ErrNoSource", err)
	}
}

func TestVerifyStopsWhenTheContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := guard(&fakeGraph{}).Verify(ctx, 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

// Verify answers "did this start correctly". Recheck answers "is it still what
// it was" -- and those need different answers to an empty graph.
func TestRecheckRefusesASourceThatChangedToACamera(t *testing.T) {
	err := guard(&fakeGraph{answers: [][]Node{{webcam}}}).Recheck(1)
	if !errors.Is(err, ErrWrongSource) {
		t.Fatalf("got %v, want ErrWrongSource", err)
	}
	if !strings.Contains(err.Error(), "changed") {
		t.Errorf("message %q does not say the source changed", err)
	}
}

func TestRecheckAcceptsTheGrantedNode(t *testing.T) {
	if err := guard(&fakeGraph{answers: [][]Node{{screen}}}).Recheck(1); err != nil {
		t.Fatalf("Recheck: %v", err)
	}
}

// A momentarily missing link is not evidence of a substitution, and tearing a
// cast down for one would make the guard the commonest cause of failure. A
// capture that has really stopped is caught by the pipeline exiting.
func TestRecheckToleratesAMomentarilyEmptyGraph(t *testing.T) {
	if err := guard(&fakeGraph{}).Recheck(1); err != nil {
		t.Fatalf("Recheck failed on an empty graph: %v", err)
	}
}

func TestRecheckToleratesAnUnreadableGraph(t *testing.T) {
	g := guard(&fakeGraph{err: errors.New("pw-dump vanished")})
	if err := g.Recheck(1); err != nil {
		t.Fatalf("Recheck failed on an unreadable graph: %v", err)
	}
}
