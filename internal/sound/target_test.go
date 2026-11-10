package sound

import (
	"slices"
	"strconv"
	"testing"
)

// pactl move-sink-input makes WirePlumber store the output as the app's
// target (state-stream.lua), and a new stream of that app then plays there
// whatever the default: after a cast, Firefox kept playing on the speakers
// with the headphones the default. So castr never moves an app's stream. It
// clears the stream's target -- target.object and target.node, both set to
// -1 -- and the stream follows the default, with nothing stored.

// targetClears returns the keys castr cleared for a stream, in order.
func targetClears(f *fakePulse, calls [][]string, idx int) []string {
	var keys []string
	node := strconv.Itoa(nodeOf(idx))
	for _, c := range calls {
		if len(c) == 8 && c[0] == "pw-metadata" && c[3] == "--" && c[4] == node && c[6] == "-1" && c[7] == "Spa:Id" {
			keys = append(keys, c[5])
		}
	}
	return keys
}

func clearedBoth(t *testing.T, f *fakePulse, calls [][]string, idx int) {
	t.Helper()
	got := targetClears(f, calls, idx)
	if !slices.Contains(got, "target.object") || !slices.Contains(got, "target.node") {
		t.Errorf("stream %d: cleared %v, want target.object and target.node", idx, got)
	}
}

func untouched(t *testing.T, f *fakePulse, calls [][]string, idx int) {
	t.Helper()
	if got := targetClears(f, calls, idx); len(got) != 0 {
		t.Errorf("stream %d, on another output, had %v cleared", idx, got)
	}
	for _, c := range calls {
		if len(c) > 2 && c[1] == "move-sink-input" && c[2] == strconv.Itoa(idx) {
			t.Errorf("stream %d, on another output, was moved: %v", idx, c)
		}
	}
}

// noModuleStreamReleased: a module's stream -- castr's loopbacks, the
// user's own loopback -- keeps its sink; castr never clears its target.
func noModuleStreamReleased(t *testing.T, f *fakePulse) {
	t.Helper()
	for _, c := range f.calls {
		if len(c) != 8 || c[0] != "pw-metadata" || c[3] != "--" {
			continue
		}
		node, _ := strconv.Atoi(c[4])
		for _, i := range f.inputs {
			if nodeOf(i.index) == node && i.owner != 0 {
				t.Errorf("ran %v on stream %d, which a module owns", c, i.index)
			}
		}
	}
}

func noAppMoved(t *testing.T, f *fakePulse) {
	t.Helper()
	noModuleStreamReleased(t, f)
	for _, c := range f.calls {
		if len(c) > 2 && c[1] == "move-sink-input" {
			if idx, _ := strconv.Atoi(c[2]); !isCastrModule(f.streamOwners[idx]) {
				t.Errorf("ran %v: moving an app's stream makes WirePlumber pin its app there", c)
			}
		}
	}
}

func TestTakeOverLetsTheDefaultsAppsFollowIntoCastrMix(t *testing.T) {
	f := newFakePulse()
	f.addSink("headset", 0)
	firefox := f.addApp(realSink)
	f.inputs[len(f.inputs)-1].target = realSink // a target WirePlumber stored
	call := f.addApp("headset")
	var onDefault []int
	for _, i := range f.inputs {
		if i.owner == 0 && i.sink == f.sinkID(realSink) {
			onDefault = append(onDefault, i.index)
		}
	}
	r := NewRouter(f.run, t.TempDir())
	mustAttach(t, r, "airplay:A")

	for _, idx := range onDefault {
		clearedBoth(t, f, f.calls, idx)
		if got := appOn(t, f, idx); got != "castr_mix" {
			t.Errorf("stream %d is on %s, want castr_mix", idx, got)
		}
	}
	if _, ok := f.pinned()[firefox]; ok {
		t.Error("Firefox's stored target is still there")
	}
	untouched(t, f, f.calls, call)
	if got := appOn(t, f, call); got != "headset" {
		t.Errorf("the call is on %s, want headset", got)
	}
	f.userLoopOn(t, realSink)
	noAppMoved(t, f)

	// Cleared once castr_mix is the default, so they follow it there.
	def := -1
	for i, c := range f.calls {
		if len(c) > 2 && c[1] == "set-default-sink" && c[2] == "castr_mix" {
			def = i
			break
		}
	}
	for i, c := range f.calls {
		if len(c) == 8 && c[0] == "pw-metadata" && c[3] == "--" && i < def {
			t.Errorf("ran %v before castr_mix was the default", c)
		}
	}
}

func TestFollowLetsAppsOnTheLaptopOrACastOutputFollowTheDefault(t *testing.T) {
	f := newFakePulse()
	r := NewRouter(f.run, t.TempDir())
	mustAttach(t, r, "airplay:A")

	// The user picks "castr cast" for one app in pavucontrol, and for the
	// default; and a stream of an app WirePlumber pinned starts on the laptop.
	pickedCast := f.addApp("castr_cast_1")
	f.def = "castr_cast_1"
	pinned := f.addApp(realSink)
	n := len(f.calls)
	if err := r.Follow(); err != nil {
		t.Fatal(err)
	}
	if err := r.Follow(); err != nil { // the sweep of castr_cast_1 runs once the default is back
		t.Fatal(err)
	}
	for _, idx := range []int{pickedCast, pinned} {
		clearedBoth(t, f, f.calls[n:], idx)
		if got := appOn(t, f, idx); got != "castr_mix" {
			t.Errorf("stream %d is on %s, want castr_mix", idx, got)
		}
	}
	noAppMoved(t, f)
	if p := f.pinned(); len(p) != 0 {
		t.Errorf("app streams left pinned: %v", p)
	}
}

func TestRestoreLetsTheAppsFollowTheDefaultBack(t *testing.T) {
	for _, known := range []bool{true, false} {
		t.Run(map[bool]string{true: "with the history", false: "without it"}[known], func(t *testing.T) {
			f := newFakePulse()
			f.addSink("headset", 0)
			call := f.addApp("headset")
			dir := t.TempDir()
			r := NewRouter(f.run, dir)
			if known {
				r.HistoryFile = f.withStateFile(t)
			}
			mustAttach(t, r, "airplay:A")
			stuck := f.addApp("castr_mix")
			f.inputs[len(f.inputs)-1].target = "castr_mix" // pinned by an older castr
			var onMix []int
			for _, i := range f.inputs {
				if i.owner == 0 && i.sink == f.sinkID("castr_mix") {
					onMix = append(onMix, i.index)
				}
			}
			if len(onMix) != 3 {
				t.Fatalf("%d apps on castr_mix, want 3", len(onMix))
			}

			n := len(f.calls)
			if err := r.Detach("airplay:A"); err != nil {
				t.Fatal(err)
			}
			for _, idx := range onMix {
				clearedBoth(t, f, f.calls[n:], idx)
				if got := appOn(t, f, idx); got != realSink {
					t.Errorf("stream %d is on %s, want %s", idx, got, realSink)
				}
			}
			if _, ok := f.pinned()[stuck]; ok {
				t.Error("the stream pinned to castr_mix is still pinned")
			}
			untouched(t, f, f.calls[n:], call)
			if p := f.pinned(); len(p) != 1 || p[call] != "headset" {
				t.Errorf("pinned app streams %v, want only the call on the headset", p)
			}
			noAppMoved(t, f)
			stateGone(t, dir)
		})
	}
}
