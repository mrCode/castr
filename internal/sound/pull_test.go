package sound

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// WirePlumber links a new stream of an app it remembers a target for -- the
// output castr's own restore moved it back to, say -- to that output, not
// to the default: a YouTube video opened mid-cast played on the laptop's
// headphones instead of the TV.

func appOn(t *testing.T, f *fakePulse, idx int) string {
	t.Helper()
	for _, i := range f.inputs {
		if i.index == idx {
			return f.sinkName(i.sink)
		}
	}
	t.Fatalf("no stream %d", idx)
	return ""
}

func TestFollowPullsAppsThatStartMidCastIntoCastrMix(t *testing.T) {
	f := newFakePulse()
	f.addSink("headset", 0)
	r := NewRouter(f.run, t.TempDir())
	var logged []string
	r.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	mustAttach(t, r, "airplay:A")
	logged = nil

	video := f.addApp(realSink) // the laptop's output
	call := f.addApp("headset") // put there by the user
	if err := r.Follow(); err != nil {
		t.Fatal(err)
	}
	if got := appOn(t, f, video); got != "castr_mix" {
		t.Errorf("the app that started on the laptop output is on %s, want castr_mix", got)
	}
	clearedBoth(t, f, f.calls, video)
	untouched(t, f, f.calls, call)
	noAppMoved(t, f)
	if got := appOn(t, f, call); got != "headset" {
		t.Errorf("the app the user put on the headset was moved to %s", got)
	}
	f.userLoopOn(t, realSink)
	if s := f.loopStream(t, realSink); f.sinkName(s.sink) != realSink {
		t.Error("castr's laptop loopback was moved")
	}
	f.checkNoFeedback(t)
	want := strconv.Itoa(video)
	if len(logged) != 1 || !strings.Contains(logged[0], want) || !strings.Contains(logged[0], "app-"+want) {
		t.Errorf("logged %q, want one line naming stream %s and its app", logged, want)
	}

	n := len(f.calls)
	logged = nil
	if err := r.Follow(); err != nil {
		t.Fatal(err)
	}
	if c := f.changingCallsSince(n); len(c) != 0 || len(logged) != 0 {
		t.Errorf("a second Follow changed %v and logged %q", c, logged)
	}

	// The laptop moves to new headphones: they are its output now, and the
	// speakers are just another output.
	f.addSink("bluez_x", 0)
	f.def = "bluez_x"
	if err := r.Follow(); err != nil {
		t.Fatal(err)
	}
	onHeadphones := f.addApp("bluez_x")
	onSpeakers := f.addApp(realSink)
	if err := r.Follow(); err != nil {
		t.Fatal(err)
	}
	if got := appOn(t, f, onHeadphones); got != "castr_mix" {
		t.Errorf("the app that started on the new laptop output is on %s, want castr_mix", got)
	}
	if got := appOn(t, f, onSpeakers); got != realSink {
		t.Errorf("an app on an output the laptop left was moved to %s", got)
	}
	f.checkNoFeedback(t)
}

func TestFollowPullsNothingWithNoCast(t *testing.T) {
	f := newFakePulse()
	r := NewRouter(f.run, t.TempDir())
	if err := r.Follow(); err != nil {
		t.Fatal(err)
	}
	mustAttach(t, r, "airplay:A")
	if err := r.Detach("airplay:A"); err != nil {
		t.Fatal(err)
	}
	f.addApp(realSink)
	n := len(f.calls)
	if err := r.Follow(); err != nil {
		t.Fatal(err)
	}
	if c := f.changingCallsSince(n); len(c) != 0 {
		t.Errorf("Follow with no cast changed %v", c)
	}
	f.allAppsOn(t, realSink)
}
