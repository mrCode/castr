package sound

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Each re-link of PipeWire's graph -- a loopback loaded or unloaded -- added
// buffering to the path into a cast's output that was never drained: the
// TV's sound fell further behind with every switch until it turned robotic,
// then silent. So each loopback is loaded once and a switch only mutes or
// unmutes their streams.

// loopLoads returns the module-loopback loads among calls, by sink.
func loopLoads(calls [][]string) []string {
	var sinks []string
	for _, c := range calls {
		if len(c) > 2 && c[1] == "load-module" && c[2] == "module-loopback" {
			sinks = append(sinks, moduleArg(strings.Join(c[3:], " "), "sink"))
		}
	}
	return sinks
}

// wantPlaying checks which outputs castr_mix's sound reaches.
func wantPlaying(t *testing.T, f *fakePulse, want map[string]bool) {
	t.Helper()
	for sink, on := range want {
		if got := f.plays(t, sink); got != on {
			t.Errorf("%s plays %v, want %v", sink, got, on)
		}
	}
}

func castLoopIDs(f *fakePulse) map[string]int {
	ids := map[string]int{}
	for _, m := range f.castrModules() {
		if sink := moduleArg(m.args, "sink"); m.name == "module-loopback" && strings.HasPrefix(sink, "castr_cast_") {
			ids[sink] = m.id
		}
	}
	return ids
}

func TestSwitchingOnlyMutesTheLoopbackStreams(t *testing.T) {
	f := newFakePulse()
	r := NewRouter(f.run, t.TempDir())
	mustAttach(t, r, "airplay:A")
	mustAttach(t, r, "airplay:B")
	n := len(f.calls)

	for _, step := range []struct {
		a, b   Choice
		laptop bool
	}{
		{Laptop, TV, true},
		{TV, TV, false},
		{Both, TV, true},
		{Laptop, Laptop, true},
		{TV, Both, true},
		{TV, TV, false},
		{Both, Both, true},
	} {
		mustSet(t, r, "airplay:A", step.a)
		mustSet(t, r, "airplay:B", step.b)
		wantPlaying(t, f, map[string]bool{
			"castr_cast_1": step.a.toReceiver(),
			"castr_cast_2": step.b.toReceiver(),
			realSink:       step.laptop,
		})
	}

	changes := f.changingCallsSince(n)
	if len(changes) == 0 {
		t.Fatal("switching made no changes")
	}
	for _, c := range changes {
		if c[1] != "set-sink-input-mute" {
			t.Errorf("switching ran %v; it may only mute and unmute", c)
			continue
		}
		idx, _ := strconv.Atoi(c[2])
		if owner := f.streamOwners[idx]; !strings.HasPrefix(owner, "source=castr_mix.monitor ") {
			t.Errorf("switching muted stream %d, which is not one of castr's loopbacks", idx)
		}
	}
	f.checkLoopbacksNeverMove(t)
}

func TestAttachLoadsEachLoopbackOnce(t *testing.T) {
	// The first cast loads its own loopback and the laptop's, muted; each
	// later cast loads only its own.
	f := newFakePulse()
	r := NewRouter(f.run, t.TempDir())
	mustAttach(t, r, "airplay:A")
	if got := loopLoads(f.calls); !slices.Equal(got, []string{realSink, "castr_cast_1"}) {
		t.Errorf("the first attach loaded loopbacks into %v, want %s, then castr_cast_1", got, realSink)
	}
	wantPlaying(t, f, map[string]bool{"castr_cast_1": true, realSink: false})

	n := len(f.calls)
	mustAttach(t, r, "airplay:B")
	if got := loopLoads(f.calls[n:]); !slices.Equal(got, []string{"castr_cast_2"}) {
		t.Errorf("the second attach loaded loopbacks into %v, want only castr_cast_2", got)
	}
	wantPlaying(t, f, map[string]bool{"castr_cast_1": true, "castr_cast_2": true, realSink: false})

	n = len(f.calls)
	if err := r.Detach("airplay:A"); err != nil {
		t.Fatal(err)
	}
	if got := loopLoads(f.calls[n:]); len(got) != 0 {
		t.Errorf("a detach loaded loopbacks into %v", got)
	}
	if _, ok := castLoopIDs(f)["castr_cast_1"]; ok {
		t.Error("A's loopback is still loaded after its detach")
	}
	wantPlaying(t, f, map[string]bool{"castr_cast_2": true, realSink: false})
}

func TestEveryLoadedLoopbackStartsAtFullVolumeWithTheRightMute(t *testing.T) {
	// WirePlumber remembers a stream's volume and mute by its media.name,
	// which is the same for every load: a loopback once muted, or turned
	// down in pavucontrol, would come back that way and silence a cast.
	check := func(t *testing.T, f *fakePulse, sink string, mute bool) {
		t.Helper()
		s := f.loopStream(t, sink)
		if s.volume != "100%" || s.mute != mute {
			t.Errorf("the loopback into %s is at %s, muted %v; want 100%%, muted %v", sink, s.volume, s.mute, mute)
		}
	}
	for _, remembered := range []bool{true, false} {
		t.Run("remembered muted "+strconv.FormatBool(remembered), func(t *testing.T) {
			f := newFakePulse()
			f.rememberedMute, f.rememberedVolume = remembered, "30%"
			r := NewRouter(f.run, t.TempDir())
			mustAttach(t, r, "airplay:A")
			check(t, f, "castr_cast_1", false)
			check(t, f, realSink, true)

			mustAttach(t, r, "airplay:B")
			check(t, f, "castr_cast_2", false)

			// A new laptop output, with the laptop wanted and not.
			mustSet(t, r, "airplay:A", Laptop)
			f.addSink("bluez_x", 0)
			f.def = "bluez_x"
			if err := r.Follow(); err != nil {
				t.Fatal(err)
			}
			check(t, f, "bluez_x", false)
			check(t, f, "castr_cast_1", true)

			mustSet(t, r, "airplay:A", TV)
			f.removeSink("bluez_x")
			if err := r.Follow(); err != nil {
				t.Fatal(err)
			}
			check(t, f, realSink, true)

			// Set explicitly, whatever the stream started with.
			for idx := range f.streamOwners {
				var vol, mute bool
				for _, c := range f.calls {
					if len(c) > 2 && c[2] == strconv.Itoa(idx) {
						vol = vol || c[1] == "set-sink-input-volume"
						mute = mute || c[1] == "set-sink-input-mute"
					}
				}
				if !vol || !mute {
					t.Errorf("loopback stream %d: volume set %v, mute set %v; want both", idx, vol, mute)
				}
			}
			if len(f.streamOwners) != 5 {
				t.Errorf("%d loopback streams, want 5", len(f.streamOwners))
			}
		})
	}
}

func TestLoopbackStreamsAreNotRestoredByWirePlumber(t *testing.T) {
	// castr sets each loopback's volume and mute right after loading it;
	// WirePlumber restoring a remembered mute a moment later would undo it.
	f := newFakePulse()
	r := NewRouter(f.run, t.TempDir())
	mustAttach(t, r, "airplay:A")
	n := 0
	for _, c := range f.calls {
		if len(c) < 3 || c[1] != "load-module" || c[2] != "module-loopback" {
			continue
		}
		n++
		args := strings.Join(c[3:], " ")
		for _, side := range []string{"sink_input_properties", "source_output_properties"} {
			props, _ := quotedArg(args, side)
			if moduleArg(props, "state.restore-props") != "false" {
				t.Errorf("loopback %q: %s lets WirePlumber restore its volume and mute", args, side)
			}
		}
	}
	if n == 0 {
		t.Fatal("no loopback loaded")
	}
}

func TestALaptopOutputChangeRelinksOnlyTheLaptopLoopback(t *testing.T) {
	f := newFakePulse()
	r := NewRouter(f.run, t.TempDir())
	mustAttach(t, r, "airplay:A")
	mustAttach(t, r, "airplay:B")
	mustSet(t, r, "airplay:A", Both)
	before := castLoopIDs(f)

	onlyTheLaptopLoop := func(t *testing.T, n int, to string) {
		t.Helper()
		for _, c := range f.changingCallsSince(n) {
			switch c[1] {
			case "load-module":
				if sink := moduleArg(strings.Join(c[3:], " "), "sink"); c[2] == "module-loopback" && sink != to {
					t.Errorf("ran %v; only the laptop loopback into %s may be loaded", c, to)
				}
			case "unload-module":
				for sink, id := range before {
					if c[2] == strconv.Itoa(id) {
						t.Errorf("ran %v, unloading the loopback into %s", c, sink)
					}
				}
			case "set-sink-input-mute", "set-sink-input-volume":
				idx, _ := strconv.Atoi(c[2])
				if moduleArg(f.streamOwners[idx], "sink") != to {
					t.Errorf("ran %v on a stream that is not the new laptop loopback's", c)
				}
			}
		}
		if got := castLoopIDs(f); !maps2Equal(got, before) {
			t.Errorf("cast loopbacks %v, want them untouched: %v", got, before)
		}
	}

	t.Run("the user picks another output", func(t *testing.T) {
		n := len(f.calls)
		f.addSink("bluez_x", 0)
		f.def = "bluez_x"
		if err := r.Follow(); err != nil {
			t.Fatal(err)
		}
		onlyTheLaptopLoop(t, n, "bluez_x")
		wantPlaying(t, f, map[string]bool{"bluez_x": true, "castr_cast_1": true, "castr_cast_2": true})
		if f.hasLoopback("castr_mix.monitor", realSink) {
			t.Error("the old laptop loopback is still there")
		}
	})

	t.Run("that output vanishes", func(t *testing.T) {
		mustSet(t, r, "airplay:A", TV)
		n := len(f.calls)
		f.removeSink("bluez_x")
		if err := r.Follow(); err != nil {
			t.Fatal(err)
		}
		onlyTheLaptopLoop(t, n, realSink)
		wantPlaying(t, f, map[string]bool{realSink: false, "castr_cast_1": true, "castr_cast_2": true})
	})
}

func maps2Equal(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestFollowCorrectsALoopbackStreamMutedFromOutside(t *testing.T) {
	// A mute in pavucontrol, or one WirePlumber restored late, must not
	// silence a cast, nor let the laptop play when no cast wants it.
	f := newFakePulse()
	r := NewRouter(f.run, t.TempDir())
	mustAttach(t, r, "airplay:A")
	mustAttach(t, r, "airplay:B")
	mustSet(t, r, "airplay:B", Laptop)
	wantPlaying(t, f, map[string]bool{"castr_cast_1": true, "castr_cast_2": false, realSink: true})

	f.setStreamMute(f.loopStream(t, "castr_cast_1").index, true)
	f.setStreamMute(f.loopStream(t, "castr_cast_2").index, false)
	f.setStreamMute(f.loopStream(t, realSink).index, true)
	n := len(f.calls)
	if err := r.Follow(); err != nil {
		t.Fatal(err)
	}
	wantPlaying(t, f, map[string]bool{"castr_cast_1": true, "castr_cast_2": false, realSink: true})
	if got := loopLoads(f.calls[n:]); len(got) != 0 {
		t.Errorf("Follow loaded loopbacks into %v to correct a mute", got)
	}

	mustSet(t, r, "airplay:B", TV)
	f.setStreamMute(f.loopStream(t, realSink).index, false)
	if err := r.Follow(); err != nil {
		t.Fatal(err)
	}
	wantPlaying(t, f, map[string]bool{"castr_cast_1": true, "castr_cast_2": true, realSink: false})

	n = len(f.calls)
	if err := r.Follow(); err != nil {
		t.Fatal(err)
	}
	if c := f.changingCallsSince(n); len(c) != 0 {
		t.Errorf("a Follow with nothing to correct changed things: %v", c)
	}
}

func TestFollowMovesACastsCaptureBackOntoItsOutput(t *testing.T) {
	// Once the helper's capture was found on castr_mix.monitor: the TV got
	// every laptop sound whatever the choice.
	f := newFakePulse()
	r := NewRouter(f.run, t.TempDir())
	mustAttach(t, r, "airplay:A")
	mustAttach(t, r, "chromecast:B")
	helper := f.addCapture("castr_cast_1.monitor", "castr_cast_1")
	chromecast := f.addCapture("castr_cast_2.monitor", "castr_cast_2.monitor")
	recorder := f.addCapture("castr_mix.monitor", "")          // the user records the mix
	other := f.addCapture("mic", "castr_cast_9")               // no such cast
	owned := f.addCapture("castr_mix.monitor", "castr_cast_1") // a module's
	f.outputs[len(f.outputs)-1].owner = 1

	n := len(f.calls)
	if err := r.Follow(); err != nil {
		t.Fatal(err)
	}
	if c := f.changingCallsSince(n); len(c) != 0 {
		t.Errorf("Follow moved captures that were where they belong: %v", c)
	}

	f.outputs[0].source = f.sinkID("castr_mix")
	f.outputs[1].source = micSource
	if err := r.Follow(); err != nil {
		t.Fatal(err)
	}
	for idx, want := range map[int]string{
		helper:     "castr_cast_1.monitor",
		chromecast: "castr_cast_2.monitor",
		recorder:   "castr_mix.monitor",
		other:      "mic",
		owned:      "castr_mix.monitor",
	} {
		if got := f.captureSource(idx); got != want {
			t.Errorf("capture %d is on %s, want %s", idx, got, want)
		}
	}

	n = len(f.calls)
	if err := r.Follow(); err != nil {
		t.Fatal(err)
	}
	if c := f.changingCallsSince(n); len(c) != 0 {
		t.Errorf("a second Follow changed things: %v", c)
	}
}

func TestTheLaptopLoopbackIsMutedBeforeAnySoundReachesCastrMix(t *testing.T) {
	// The laptop loopback starts unmuted. Loaded and muted before castr_mix
	// is made the default and the apps are moved onto it, it has nothing to
	// play on the laptop while it is unmuted, however late pactl lists it.
	f := newFakePulse()
	r := NewRouter(f.run, t.TempDir())
	mustAttach(t, r, "airplay:A")

	first := func(match func(c []string) bool) int {
		for i, c := range f.calls {
			if match(c) {
				return i
			}
		}
		return -1
	}
	laptopStream := func(c []string) bool {
		idx, _ := strconv.Atoi(c[2])
		return moduleArg(f.streamOwners[idx], "sink") == realSink
	}
	mute := first(func(c []string) bool {
		return len(c) > 3 && c[1] == "set-sink-input-mute" && c[3] == "1" && laptopStream(c)
	})
	volume := first(func(c []string) bool { return len(c) > 2 && c[1] == "set-sink-input-volume" && laptopStream(c) })
	move := first(func(c []string) bool { return len(c) > 3 && c[1] == "move-sink-input" && c[3] == "castr_mix" })
	def := first(func(c []string) bool { return len(c) > 2 && c[1] == "set-default-sink" && c[2] == "castr_mix" })
	if mute < 0 || volume < 0 || move < 0 || def < 0 {
		t.Fatalf("calls missing: laptop mute %d, laptop volume %d, move onto castr_mix %d, default castr_mix %d", mute, volume, move, def)
	}
	if mute > move || volume > move {
		t.Errorf("an app was moved onto castr_mix (call %d) before the laptop loopback was muted (%d) and set to 100%% (%d)", move, mute, volume)
	}
	if mute > def || volume > def {
		t.Errorf("castr_mix was made the default (call %d) before the laptop loopback was muted (%d) and set to 100%% (%d)", def, mute, volume)
	}
	if !strings.Contains(strings.Join(f.calls[first(func(c []string) bool {
		return len(c) > 2 && c[1] == "load-module" && c[2] == "module-loopback"
	})], " "), "state.restore-props=false") {
		t.Error("the laptop loopback lets WirePlumber restore its mute")
	}
}
