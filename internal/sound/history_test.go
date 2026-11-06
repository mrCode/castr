package sound

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Outputs on this machine, as WirePlumber 0.5.18 named them.
const (
	jbl    = "bluez_output.4C_CF_7C_EF_03_D0.1"
	buds   = "bluez_output.20_91_DF_7A_FF_FE.1"
	hyperX = "alsa_output.usb-HP__Inc_HyperX_Cloud_III_S_Wireless_C1V54100KJ-00.analog-stereo"
)

// before is this machine's output history before the hardware check's cast:
// only the speakers (realSink) are there; the rest are disconnected.
var before = []string{jbl, buds, hyperX, realSink}

// historyIs checks that the stack starts with want, exactly, and that
// anything below is castr's own -- left there by castr's own writes, and
// never chosen, as castr's outputs are gone.
func historyIs(t *testing.T, f *fakePulse, want []string) {
	t.Helper()
	if len(f.history) < len(want) || !slices.Equal(f.history[:len(want)], want) {
		t.Errorf("output history %v, want %v", f.history, want)
		return
	}
	for _, name := range f.history[len(want):] {
		if !isCastrName(name) {
			t.Errorf("output history %v, want %v and then only castr's own", f.history, want)
			return
		}
	}
}

func savedHistory(t *testing.T, dir string) any {
	t.Helper()
	return readState(t, dir)["sink_history"]
}

func TestRestorePutsWirePlumbersOutputHistoryBackExactly(t *testing.T) {
	// Measured after a cast with the old restore: the fallback order behind
	// the headphones changed, [jbl, speakers, castr_mix, buds, hyperX].
	for _, how := range []string{"the last detach", "recovery after a crash"} {
		t.Run(how, func(t *testing.T) {
			f := newFakePulse()
			f.history = slices.Clone(before)
			dir := t.TempDir()
			r := NewRouter(f.run, dir)
			r.HistoryFile = f.withStateFile(t)
			mustAttach(t, r, "airplay:A")
			mustSet(t, r, "airplay:A", Both)

			got, _ := savedHistory(t, dir).([]any)
			if len(got) != len(before) {
				t.Fatalf("sink_history %v, want %v", savedHistory(t, dir), before)
			}
			for i, name := range before {
				if got[i] != name {
					t.Fatalf("sink_history %v, want %v", got, before)
				}
			}

			if how == "the last detach" {
				if err := r.Detach("airplay:A"); err != nil {
					t.Fatal(err)
				}
			} else if err := Recover(f.run, dir); err != nil {
				t.Fatal(err)
			}
			historyIs(t, f, before)
			if f.def != realSink {
				t.Errorf("default is %s, want %s", f.def, realSink)
			}
			f.allAppsOn(t, realSink)
			f.userLoopOn(t, realSink)
			if m := f.castrModules(); len(m) != 0 {
				t.Errorf("castr modules left: %v", m)
			}
			stateGone(t, dir)
		})
	}
}

func TestRestoreNeverSetsTheDefaultOverTheHistory(t *testing.T) {
	// pactl set-default-sink pushes its output onto the history: the speakers
	// would land second, above the buds and the HyperX.
	f := newFakePulse()
	f.history = slices.Clone(before)
	r := NewRouter(f.run, t.TempDir())
	r.HistoryFile = f.withStateFile(t)
	mustAttach(t, r, "airplay:A")
	n := len(f.calls)
	if err := r.Detach("airplay:A"); err != nil {
		t.Fatal(err)
	}
	var writes []string
	for _, c := range f.calls[n:] {
		if len(c) > 1 && c[1] == "set-default-sink" {
			t.Errorf("restore ran %v", c)
		}
		if c[0] == "pw-metadata" && len(c) == 7 {
			writes = append(writes, c[5])
		}
	}
	want := []string{`{"name":"` + realSink + `"}`, `{"name":"` + hyperX + `"}`, `{"name":"` + buds + `"}`, `{"name":"` + jbl + `"}`}
	if !slices.Equal(writes, want) {
		t.Errorf("restore wrote %v, want the history from the bottom up %v", writes, want)
	}
}

func TestAnOutputPickedMidCastEndsOnTopOfTheHistory(t *testing.T) {
	// As WirePlumber would have had it without castr: the pick on top, the
	// rest below in their order.
	f := newFakePulse()
	f.history = slices.Clone(before)
	dir := t.TempDir()
	r := NewRouter(f.run, dir)
	r.HistoryFile = f.withStateFile(t)
	mustAttach(t, r, "airplay:A")

	f.addSink(hyperX, 0)
	if _, err := f.run("pactl", "set-default-sink", hyperX); err != nil { // the user picks it
		t.Fatal(err)
	}
	if err := r.Follow(); err != nil {
		t.Fatal(err)
	}
	if got, _ := savedHistory(t, dir).([]any); len(got) == 0 || got[0] != hyperX {
		t.Errorf("sink_history %v, want %s on top", savedHistory(t, dir), hyperX)
	}

	if err := r.Detach("airplay:A"); err != nil {
		t.Fatal(err)
	}
	historyIs(t, f, []string{hyperX, jbl, buds, realSink})
	if f.def != hyperX {
		t.Errorf("default is %s, want %s", f.def, hyperX)
	}
	f.allAppsOn(t, hyperX)
}

func TestAnEmptyHistoryIsLeftEmpty(t *testing.T) {
	f := newFakePulse()
	f.history = nil
	dir := t.TempDir()
	r := NewRouter(f.run, dir)
	r.HistoryFile = f.withStateFile(t)
	mustAttach(t, r, "airplay:A")
	if got, ok := savedHistory(t, dir).([]any); !ok || len(got) != 0 {
		t.Errorf("sink_history %#v, want []", savedHistory(t, dir))
	}
	if err := r.Detach("airplay:A"); err != nil {
		t.Fatal(err)
	}
	if len(f.history) != 0 {
		t.Errorf("output history %v, want it empty", f.history)
	}
	if f.def != realSink {
		t.Errorf("default is %s, want %s", f.def, realSink)
	}
	f.allAppsOn(t, realSink)
	stateGone(t, dir)
}

func TestWithoutTheHistoryFileOnlyTheConfiguredDefaultIsPutBack(t *testing.T) {
	// Not knowing the history is no reason to delete the key, which would
	// erase it all.
	for name, path := range map[string]func(t *testing.T) string{
		"missing":    func(t *testing.T) string { return filepath.Join(t.TempDir(), "default-nodes") },
		"unreadable": func(t *testing.T) string { return t.TempDir() }, // a directory
		"not set":    func(t *testing.T) string { return "" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakePulse()
			f.history = slices.Clone(before)
			dir := t.TempDir()
			r := NewRouter(f.run, dir)
			r.HistoryFile = path(t)
			mustAttach(t, r, "airplay:A")
			if h, ok := readState(t, dir)["sink_history"]; ok && h != nil {
				t.Errorf("sink_history %v, want it unknown", h)
			}
			n := len(f.calls)
			if err := r.Detach("airplay:A"); err != nil {
				t.Fatal(err)
			}
			writes := 0
			for _, c := range f.calls[n:] {
				if c[0] == "pw-metadata" && slices.Contains(c, "-d") {
					t.Errorf("deleted the key: %v", c)
				}
				if c[0] == "pw-metadata" && len(c) == 7 {
					writes++
				}
			}
			if writes != 1 {
				t.Errorf("%d writes of the key, want 1", writes)
			}
			configuredIs(t, f, jbl)
			if f.def != realSink {
				t.Errorf("default is %s, want %s", f.def, realSink)
			}
			stateGone(t, dir)
		})
	}
}

func TestTheSavedHistoryFollowsWirePlumbersMetadata(t *testing.T) {
	// WirePlumber writes its state file a moment after a change, so the file
	// may lag behind the metadata, which is the truth.
	for name, tc := range map[string]struct {
		file       string
		configured *string
		want       []string
	}{
		"a pick not saved yet": {
			file:       "default.configured.audio.sink=" + realSink + "\ndefault.configured.audio.sink.0=" + jbl + "\n",
			configured: ptr(jbl),
			want:       []string{jbl, realSink},
		},
		"a new output not saved yet": {
			file:       "default.configured.audio.sink=" + realSink + "\n",
			configured: ptr(buds),
			want:       []string{buds, realSink},
		},
		"a delete not saved yet": {
			file: "default.configured.audio.sink=" + realSink + "\n",
			want: []string{},
		},
		"castr's own leftovers": {
			file: "default.configured.audio.sink=" + jbl + "\ndefault.configured.audio.sink.0=" + realSink +
				"\ndefault.configured.audio.sink.1=castr_mix\ndefault.configured.audio.sink.2=castr_cast_3\ndefault.configured.audio.sink.3=" + buds + "\n",
			configured: ptr(jbl),
			want:       []string{jbl, realSink, buds},
		},
		"castr_mix left on top": {
			file:       "default.configured.audio.sink=castr_mix\ndefault.configured.audio.sink.0=" + realSink + "\n",
			configured: ptr("castr_mix"),
			want:       []string{realSink},
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakePulse()
			f.history = nil
			if tc.configured != nil {
				f.history = []string{*tc.configured}
			}
			path := filepath.Join(t.TempDir(), "default-nodes")
			if err := os.WriteFile(path, []byte("[default-nodes]\n"+tc.file), 0o600); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			r := NewRouter(f.run, dir)
			r.HistoryFile = path
			mustAttach(t, r, "airplay:A")
			raw, _ := savedHistory(t, dir).([]any)
			got := []string{}
			for _, v := range raw {
				got = append(got, v.(string))
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("sink_history %v, want %v", got, tc.want)
			}
			// The configured default is the stack's top: what WirePlumber
			// would switch to when it appears.
			c, _ := readState(t, dir)["configured_default"].(map[string]any)
			if name, _ := c["name"].(string); len(tc.want) > 0 && name != tc.want[0] || len(tc.want) == 0 && name != "" {
				t.Errorf("configured_default %v, want the top of %v", c, tc.want)
			}
		})
	}
}

func ptr(s string) *string { return &s }

func TestReadingWirePlumbersHistory(t *testing.T) {
	for name, tc := range map[string]struct {
		file string
		want []string
	}{
		"this machine's": {
			file: "[default-nodes]\ndefault.configured.audio.sink=" + jbl +
				"\ndefault.configured.audio.sink.0=" + realSink +
				"\ndefault.configured.audio.sink.1=castr_mix\ndefault.configured.audio.sink.2=" + buds +
				"\ndefault.configured.audio.sink.3=" + hyperX + "\n",
			want: []string{jbl, realSink, "castr_mix", buds, hyperX},
		},
		"in any order, among other keys": {
			file: "[default-nodes]\ndefault.configured.audio.sink.1=c\ndefault.configured.audio.source=mic\n" +
				"default.configured.audio.sink.0=b\ndefault.configured.audio.source.0=mic2\ndefault.configured.audio.sink=a\n",
			want: []string{"a", "b", "c"},
		},
		"stopping at the first gap": {
			file: "[default-nodes]\ndefault.configured.audio.sink=a\ndefault.configured.audio.sink.1=c\n",
			want: []string{"a"},
		},
		"no top means no history": {
			file: "[default-nodes]\ndefault.configured.audio.sink.0=b\n",
			want: []string{},
		},
		"only its own section": {
			file: "[other]\ndefault.configured.audio.sink=x\n[default-nodes]\ndefault.configured.audio.sink=a\n",
			want: []string{"a"},
		},
		"GKeyFile escapes and comments": {
			file: "# comment\n[default-nodes]\n\ndefault.configured.audio.sink = \\sodd\\\\name\n",
			want: []string{` odd\name`},
		},
		"empty": {
			file: "",
			want: []string{},
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "default-nodes")
			if err := os.WriteFile(path, []byte(tc.file), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := readSinkHistory(path)
			if err != nil {
				t.Fatal(err)
			}
			if got == nil || !slices.Equal(got, tc.want) {
				t.Errorf("history %#v, want %#v", got, tc.want)
			}
		})
	}
	if _, err := readSinkHistory(filepath.Join(t.TempDir(), "none")); err == nil {
		t.Error("a missing file read as a history")
	}
}

func TestWirePlumbersHistoryFileIsInTheStateDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/state")
	if got := WirePlumberHistoryFile(); got != "/state/wireplumber/default-nodes" {
		t.Errorf("got %s", got)
	}
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/home/u")
	if got := WirePlumberHistoryFile(); got != "/home/u/.local/state/wireplumber/default-nodes" {
		t.Errorf("got %s", got)
	}
}

func TestRestoreMovesTheAppsToWhereWirePlumberWillPlay(t *testing.T) {
	// WirePlumber picks the new default a moment after the writes, so
	// pactl may still say castr_mix: the apps go to the first output in the
	// stack that exists, which is what it will pick. Here that is the buds,
	// which connected mid-cast, not the speakers the laptop played on.
	f := newFakePulse()
	f.history = slices.Clone(before)
	r := NewRouter(f.run, t.TempDir())
	r.HistoryFile = f.withStateFile(t)
	mustAttach(t, r, "airplay:A")
	f.addSink(buds, 0)
	if err := r.Follow(); err != nil {
		t.Fatal(err)
	}
	stale := "castr_mix\n"
	f.defaultOut = &stale
	n := len(f.calls)
	if err := r.Detach("airplay:A"); err != nil {
		t.Fatal(err)
	}
	moved := 0
	for _, c := range f.calls[n:] {
		if len(c) == 4 && c[1] == "move-sink-input" {
			if c[3] != buds {
				t.Errorf("restore ran %v, want the apps moved to %s", c, buds)
			}
			moved++
		}
	}
	if moved != 2 {
		t.Errorf("moved %d app streams, want 2", moved)
	}
	historyIs(t, f, before)
	if f.def != buds {
		t.Errorf("default is %s, want %s", f.def, buds)
	}
}

func TestShutdownFinishesARestoreThatFailed(t *testing.T) {
	// castrd's shutdown calls Restore. After a failed last detach the
	// router has forgotten what to put back; the state file has not.
	for _, known := range []bool{true, false} {
		t.Run(map[bool]string{true: "with the history", false: "without it"}[known], func(t *testing.T) {
			f := newFakePulse()
			f.history = slices.Clone(before)
			dir := t.TempDir()
			r := NewRouter(f.run, dir)
			if known {
				r.HistoryFile = f.withStateFile(t)
			}
			mustAttach(t, r, "airplay:A")
			f.metaFail = true
			if err := r.Detach("airplay:A"); err == nil {
				t.Fatal("Detach hid the failure")
			}
			readState(t, dir)

			f.metaFail = false
			if err := r.Restore(); err != nil {
				t.Fatal(err)
			}
			if known {
				historyIs(t, f, before)
			}
			configuredIs(t, f, jbl)
			if f.def != realSink {
				t.Errorf("default is %s, want %s", f.def, realSink)
			}
			stateGone(t, dir)
		})
	}
}

func TestShutdownWithNothingRoutedChangesNothing(t *testing.T) {
	f := newFakePulse()
	r := NewRouter(f.run, t.TempDir())
	if err := r.Restore(); err != nil {
		t.Fatal(err)
	}
	if c := f.changingCallsSince(0); len(c) != 0 {
		t.Errorf("Restore changed things: %v", c)
	}
}
