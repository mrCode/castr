package sound

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The fake models just enough of pipewire-pulse for the router: sinks,
// modules, sink-inputs with their owning module, and the default sink. It
// applies every pactl command the router uses and records each call, so no
// test ever changes the real machine's sound.

const realSink = "alsa_output.pci-0000_00_1f.3.analog-stereo"

type fakeSink struct {
	id     int
	name   string
	module int // 0 for hardware
}

type fakeModule struct {
	id         int
	name, args string
}

type fakeInput struct {
	index, sink int
	owner       int // 0 for an app stream
}

type fakePulse struct {
	sinks   []fakeSink
	modules []fakeModule
	inputs  []fakeInput
	def     string
	nextID  int
	calls   [][]string

	defaultErr error
	defaultOut *string // overrides get-default-sink's output when set
}

func newFakePulse() *fakePulse {
	f := &fakePulse{nextID: 100, def: realSink}
	f.addSink(realSink, 0)
	// Modules that are not castr's. Neither may ever be unloaded. The first
	// has PipeWire's multi-line args, as `pactl list short modules` prints.
	f.modules = append(f.modules,
		fakeModule{id: 1, name: "libpipewire-module-rt", args: "{\n            nice.level    = -11\n            rt.prio       = 88\n        }"},
		fakeModule{id: f.id(), name: "module-loopback", args: "source=mic.monitor sink=" + realSink + " latency_msec=60"},
	)
	f.addApp(realSink)
	f.addApp(realSink)
	return f
}

func (f *fakePulse) id() int { f.nextID++; return f.nextID }

func (f *fakePulse) addSink(name string, module int) {
	f.sinks = append(f.sinks, fakeSink{id: f.id(), name: name, module: module})
}

func (f *fakePulse) addApp(sink string) int {
	idx := f.id()
	f.inputs = append(f.inputs, fakeInput{index: idx, sink: f.sinkID(sink)})
	return idx
}

func (f *fakePulse) sinkID(name string) int {
	for _, s := range f.sinks {
		if s.name == name {
			return s.id
		}
	}
	return -1
}

func (f *fakePulse) sinkName(id int) string {
	for _, s := range f.sinks {
		if s.id == id {
			return s.name
		}
	}
	return ""
}

// removeSink makes a sink vanish the way a Bluetooth disconnect does: its
// streams fall back to the default.
func (f *fakePulse) removeSink(name string) {
	id := f.sinkID(name)
	var kept []fakeSink
	for _, s := range f.sinks {
		if s.id != id {
			kept = append(kept, s)
		}
	}
	f.sinks = kept
	for i := range f.inputs {
		if f.inputs[i].sink == id {
			f.inputs[i].sink = f.sinkID(f.def)
		}
	}
}

func (f *fakePulse) run(name string, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if name != "pactl" {
		return "", fmt.Errorf("unexpected command %s", name)
	}
	cmd := strings.Join(args, " ")
	switch {
	case cmd == "get-default-sink":
		if f.defaultErr != nil {
			return "", f.defaultErr
		}
		if f.defaultOut != nil {
			return *f.defaultOut, nil
		}
		return f.def + "\n", nil
	case cmd == "list short sinks":
		var b strings.Builder
		for _, s := range f.sinks {
			fmt.Fprintf(&b, "%d\t%s\tPipeWire\tfloat32le 2ch 48000Hz\tSUSPENDED\n", s.id, s.name)
		}
		return b.String(), nil
	case cmd == "list short modules":
		var b strings.Builder
		for _, m := range f.modules {
			fmt.Fprintf(&b, "%d\t%s\t%s\t\n", m.id, m.name, m.args)
		}
		return b.String(), nil
	case cmd == "--format=json list sink-inputs":
		type in struct {
			Index int     `json:"index"`
			Sink  int     `json:"sink"`
			Owner *string `json:"owner_module"`
		}
		out := []in{}
		for _, i := range f.inputs {
			var owner *string
			if i.owner != 0 {
				s := strconv.Itoa(i.owner)
				owner = &s
			}
			out = append(out, in{Index: i.index, Sink: i.sink, Owner: owner})
		}
		raw, _ := json.Marshal(out)
		return string(raw), nil
	}
	if len(args) == 0 {
		return "", errors.New("no command")
	}
	switch args[0] {
	case "load-module":
		return f.loadModule(args[1:])
	case "unload-module":
		id, _ := strconv.Atoi(args[1])
		return "", f.unload(id)
	case "set-sink-volume", "set-sink-mute":
		if f.sinkID(args[1]) < 0 {
			return "", errors.New("No such entity")
		}
		return "", nil
	case "set-default-sink":
		if f.sinkID(args[1]) < 0 {
			return "", errors.New("No such entity")
		}
		f.def = args[1]
		return "", nil
	case "move-sink-input":
		idx, _ := strconv.Atoi(args[1])
		sink := f.sinkID(args[2])
		if sink < 0 {
			return "", errors.New("No such entity")
		}
		for i := range f.inputs {
			if f.inputs[i].index == idx {
				f.inputs[i].sink = sink
				return "", nil
			}
		}
		return "", errors.New("No such entity")
	}
	return "", fmt.Errorf("unexpected pactl %s", cmd)
}

func (f *fakePulse) loadModule(args []string) (string, error) {
	kv := map[string]string{}
	for _, a := range args[1:] {
		k, v, _ := strings.Cut(a, "=")
		kv[k] = v
	}
	id := f.id()
	switch args[0] {
	case "module-null-sink":
		if kv["sink_name"] == "" || f.sinkID(kv["sink_name"]) >= 0 {
			return "", errors.New("bad sink_name")
		}
		f.addSink(kv["sink_name"], id)
	case "module-loopback":
		src := strings.TrimSuffix(kv["source"], ".monitor")
		if f.sinkID(src) < 0 || f.sinkID(kv["sink"]) < 0 || kv["latency_msec"] != "30" {
			return "", errors.New("bad loopback")
		}
		f.inputs = append(f.inputs, fakeInput{index: f.id(), sink: f.sinkID(kv["sink"]), owner: id})
	default:
		return "", errors.New("unknown module")
	}
	f.modules = append(f.modules, fakeModule{id: id, name: args[0], args: strings.Join(args[1:], " ")})
	return strconv.Itoa(id) + "\n", nil
}

func (f *fakePulse) unload(id int) error {
	found := false
	var kept []fakeModule
	for _, m := range f.modules {
		if m.id == id {
			found = true
			continue
		}
		kept = append(kept, m)
	}
	if !found {
		return errors.New("No such entity")
	}
	f.modules = kept
	var inputs []fakeInput
	for _, i := range f.inputs {
		if i.owner != id {
			inputs = append(inputs, i)
		}
	}
	f.inputs = inputs
	for _, s := range f.sinks {
		if s.module == id {
			if f.def == s.name { // the session manager falls back to another sink
				for _, o := range f.sinks {
					if o.module == 0 {
						f.def = o.name
						break
					}
				}
			}
			f.removeSink(s.name)
		}
	}
	return nil
}

// --- queries the tests make of the fake ---

func (f *fakePulse) hasLoopback(source, sink string) bool {
	want := "source=" + source + " sink=" + sink + " "
	for _, m := range f.modules {
		if m.name == "module-loopback" && strings.HasPrefix(m.args, want) {
			return true
		}
	}
	return false
}

func (f *fakePulse) castrModules() []fakeModule {
	var out []fakeModule
	for _, m := range f.modules {
		if strings.Contains(m.args, "castr_mix") || strings.Contains(m.args, "castr_cast_") {
			out = append(out, m)
		}
	}
	return out
}

// appSinks returns the sink name each app stream is on.
func (f *fakePulse) appSinks() []string {
	var out []string
	for _, i := range f.inputs {
		if i.owner == 0 {
			out = append(out, f.sinkName(i.sink))
		}
	}
	return out
}

func (f *fakePulse) allAppsOn(t *testing.T, sink string) {
	t.Helper()
	for _, s := range f.appSinks() {
		if s != sink {
			t.Errorf("app streams are on %v, want all on %s", f.appSinks(), sink)
			return
		}
	}
}

func isChanging(c []string) bool {
	if len(c) < 2 {
		return false
	}
	switch c[1] {
	case "load-module", "unload-module", "set-sink-volume", "set-sink-mute", "set-default-sink", "move-sink-input":
		return true
	}
	return false
}

func (f *fakePulse) changingCallsSince(n int) [][]string {
	var out [][]string
	for _, c := range f.calls[n:] {
		if isChanging(c) {
			out = append(out, c)
		}
	}
	return out
}

func readState(t *testing.T, dir string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, StateFile))
	if err != nil {
		t.Fatalf("reading the state file: %v", err)
	}
	var s map[string]string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("state file %q: %v", raw, err)
	}
	return s
}

func stateGone(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, StateFile)); !os.IsNotExist(err) {
		t.Errorf("state file still there (%v)", err)
	}
}

func mustAttach(t *testing.T, r *Router, id string) string {
	t.Helper()
	src, err := r.Attach(id)
	if err != nil {
		t.Fatalf("Attach(%s): %v", id, err)
	}
	return src
}

func mustSet(t *testing.T, r *Router, id string, c Choice) {
	t.Helper()
	if err := r.Set(id, c); err != nil {
		t.Fatalf("Set(%s, %s): %v", id, c, err)
	}
}

// --- the tests ---

func TestAttachRoutesTheLaptopIntoCastrAndTheCastStartsOnTV(t *testing.T) {
	f := newFakePulse()
	dir := t.TempDir()
	r := NewRouter(f.run, dir)

	src := mustAttach(t, r, "airplay:A")

	if f.def != "castr_mix" {
		t.Errorf("default is %s, want castr_mix", f.def)
	}
	f.allAppsOn(t, "castr_mix")
	if !f.hasLoopback("castr_mix.monitor", "castr_cast_1") {
		t.Error("no loopback castr_mix.monitor -> castr_cast_1")
	}
	if f.hasLoopback("castr_mix.monitor", realSink) {
		t.Error("a loopback to the real sink exists, so the laptop is not silent")
	}
	if src != "castr_cast_1.monitor" {
		t.Errorf("source %q, want castr_cast_1.monitor", src)
	}
	if c, ok := r.Choice("airplay:A"); !ok || c != TV {
		t.Errorf("Choice = %q, %v; want tv", c, ok)
	}
	if s := readState(t, dir); s["previous_default"] != realSink || s["laptop_sink"] != realSink {
		t.Errorf("state %v, want both sinks %s", s, realSink)
	}
}

func TestEveryCastrSinkIsSetToFullVolumeAndUnmuted(t *testing.T) {
	// WirePlumber remembers volume and mute by sink name: a castr sink once
	// left at 0% comes back at 0%, and its monitor carries silence.
	f := newFakePulse()
	r := NewRouter(f.run, t.TempDir())
	mustAttach(t, r, "airplay:A")
	mustAttach(t, r, "chromecast:B")

	for _, name := range []string{"castr_mix", "castr_cast_1", "castr_cast_2"} {
		var vol, mute bool
		for _, c := range f.calls {
			j := strings.Join(c, " ")
			vol = vol || j == "pactl set-sink-volume "+name+" 100%"
			mute = mute || j == "pactl set-sink-mute "+name+" 0"
		}
		if !vol || !mute {
			t.Errorf("%s: volume set %v, unmuted %v; want both", name, vol, mute)
		}
	}
	for _, c := range f.calls {
		if len(c) > 2 && (c[1] == "set-sink-volume" || c[1] == "set-sink-mute") && !strings.HasPrefix(c[2], "castr_") {
			t.Errorf("touched a real sink: %v", c)
		}
	}
}

func TestTheChoicesCombineAcrossCasts(t *testing.T) {
	f := newFakePulse()
	r := NewRouter(f.run, t.TempDir())
	mustAttach(t, r, "airplay:A")
	mustAttach(t, r, "airplay:B")

	mustSet(t, r, "airplay:A", Laptop)
	mustSet(t, r, "airplay:B", TV)
	if !f.hasLoopback("castr_mix.monitor", realSink) {
		t.Error("A=laptop: no laptop loopback")
	}
	if f.hasLoopback("castr_mix.monitor", "castr_cast_1") {
		t.Error("A=laptop: A's receiver still gets sound")
	}
	if !f.hasLoopback("castr_mix.monitor", "castr_cast_2") {
		t.Error("A=laptop: B's receiver lost its sound")
	}

	mustSet(t, r, "airplay:A", TV)
	if f.hasLoopback("castr_mix.monitor", realSink) {
		t.Error("both tv: the laptop still plays")
	}

	mustSet(t, r, "airplay:A", Both)
	if !f.hasLoopback("castr_mix.monitor", "castr_cast_1") || !f.hasLoopback("castr_mix.monitor", realSink) {
		t.Error("A=both: want A's receiver and the laptop to play")
	}
	if c, _ := r.Choice("airplay:A"); c != Both {
		t.Errorf("Choice(A) = %q, want both", c)
	}
	if err := r.Set("airplay:A", "speaker"); err == nil {
		t.Error("Set accepted an unknown choice")
	}
	if err := r.Set("airplay:Z", TV); err == nil {
		t.Error("Set accepted a cast that is not routed")
	}
}

func TestSwitchingNeverTouchesTheCastSinks(t *testing.T) {
	// Unloading a cast sink would cut the helper's capture and restart the
	// picture; changing the default would let the laptop play.
	f := newFakePulse()
	r := NewRouter(f.run, t.TempDir())
	mustAttach(t, r, "airplay:A")
	mustAttach(t, r, "airplay:B")
	castSinks := map[string]bool{}
	for _, s := range f.sinks {
		if strings.HasPrefix(s.name, "castr_cast_") {
			castSinks[strconv.Itoa(s.module)] = true
		}
	}
	n := len(f.calls)

	for _, c := range []Choice{Laptop, Both, TV, Laptop, TV, Both} {
		mustSet(t, r, "airplay:A", c)
		mustSet(t, r, "airplay:B", c)
	}

	changes := f.changingCallsSince(n)
	if len(changes) == 0 {
		t.Fatal("switching made no changes")
	}
	for _, c := range changes {
		switch {
		case c[1] == "load-module" && c[2] == "module-loopback":
		case c[1] == "unload-module" && !castSinks[c[2]]:
		default:
			t.Errorf("switching ran %v", c)
		}
	}
}

func TestTheLastDetachRestoresEverything(t *testing.T) {
	for _, order := range [][]string{{"airplay:A", "airplay:B"}, {"airplay:B", "airplay:A"}} {
		t.Run(strings.Join(order, ","), func(t *testing.T) {
			f := newFakePulse()
			dir := t.TempDir()
			r := NewRouter(f.run, dir)
			mustAttach(t, r, "airplay:A")
			mustAttach(t, r, "airplay:B")
			mustSet(t, r, "airplay:A", Both)

			if err := r.Detach(order[0]); err != nil {
				t.Fatal(err)
			}
			if f.def != "castr_mix" {
				t.Fatalf("after the first detach the default is %s; the other cast lost its routing", f.def)
			}
			if err := r.Detach(order[1]); err != nil {
				t.Fatal(err)
			}

			if f.def != realSink {
				t.Errorf("default is %s, want %s", f.def, realSink)
			}
			f.allAppsOn(t, realSink)
			if m := f.castrModules(); len(m) != 0 {
				t.Errorf("castr modules left: %v", m)
			}
			if len(f.modules) != 2 {
				t.Errorf("modules %v; want only the two that are not castr's", f.modules)
			}
			stateGone(t, dir)
		})
	}
}

func TestRestoreWhenTheSavedSinkIsGone(t *testing.T) {
	// Bluetooth disconnected mid-cast: castr must not set a default that does
	// not exist, and must leave WirePlumber's choice in place.
	f := newFakePulse()
	f.addSink("speaker2", 0)
	dir := t.TempDir()
	r := NewRouter(f.run, dir)
	mustAttach(t, r, "airplay:A")
	f.removeSink(realSink)

	if err := r.Detach("airplay:A"); err != nil {
		t.Fatal(err)
	}

	for _, c := range f.calls {
		if len(c) > 2 && c[1] == "set-default-sink" && c[2] == realSink {
			t.Errorf("ran %v against a sink that is gone", c)
		}
	}
	if m := f.castrModules(); len(m) != 0 {
		t.Errorf("castr modules left: %v", m)
	}
	stateGone(t, dir)
}

func TestFollowAdoptsANewDefaultOutput(t *testing.T) {
	// Headphones plugged in mid-cast: the new output is the laptop now.
	f := newFakePulse()
	dir := t.TempDir()
	r := NewRouter(f.run, dir)
	mustAttach(t, r, "airplay:A")
	mustSet(t, r, "airplay:A", Laptop)

	f.addSink("bluez_x", 0)
	f.def = "bluez_x"
	app := f.addApp("bluez_x")

	if err := r.Follow(); err != nil {
		t.Fatal(err)
	}

	if !f.hasLoopback("castr_mix.monitor", "bluez_x") {
		t.Error("the laptop loopback does not go to bluez_x")
	}
	if f.hasLoopback("castr_mix.monitor", realSink) {
		t.Error("the old laptop loopback is still there")
	}
	if f.def != "castr_mix" {
		t.Errorf("default is %s, want castr_mix", f.def)
	}
	for _, i := range f.inputs {
		if i.index == app && f.sinkName(i.sink) != "castr_mix" {
			t.Errorf("the new app stream is on %s, want castr_mix", f.sinkName(i.sink))
		}
		if i.owner != 0 && i.sink == f.sinkID("castr_mix") {
			t.Errorf("castr's own loopback stream %d was moved onto castr_mix", i.index)
		}
	}
	if s := readState(t, dir); s["laptop_sink"] != "bluez_x" || s["previous_default"] != realSink {
		t.Errorf("state %v, want laptop_sink bluez_x and previous_default %s", s, realSink)
	}

	n := len(f.calls)
	if err := r.Follow(); err != nil {
		t.Fatal(err)
	}
	if c := f.changingCallsSince(n); len(c) != 0 {
		t.Errorf("a second Follow changed things: %v", c)
	}
}

func TestFollowGivesUpWhenCastrMixDisappears(t *testing.T) {
	// pipewire-pulse restarted: every module is gone, and so is the routing.
	f := newFakePulse()
	dir := t.TempDir()
	r := NewRouter(f.run, dir)
	mustAttach(t, r, "airplay:A")
	for _, m := range f.castrModules() {
		_ = f.unload(m.id)
	}
	f.def = realSink

	err := r.Follow()
	if err == nil || !strings.Contains(err.Error(), "lost") {
		t.Fatalf("Follow = %v, want an error saying the routing was lost", err)
	}
	stateGone(t, dir)
	if _, ok := r.Choice("airplay:A"); ok {
		t.Error("the cast still reports a choice after the routing was lost")
	}
}

func TestRecoverRestoresAPendingRouting(t *testing.T) {
	t.Run("a dead daemon's routing", func(t *testing.T) {
		f := newFakePulse()
		dir := t.TempDir()
		dead := NewRouter(f.run, dir)
		mustAttach(t, dead, "airplay:A")
		mustAttach(t, dead, "airplay:B")
		mustSet(t, dead, "airplay:B", Both)

		if err := Recover(f.run, dir); err != nil {
			t.Fatal(err)
		}

		if f.def != realSink {
			t.Errorf("default is %s, want %s", f.def, realSink)
		}
		f.allAppsOn(t, realSink)
		if m := f.castrModules(); len(m) != 0 {
			t.Errorf("castr modules left: %v", m)
		}
		stateGone(t, dir)
	})

	t.Run("leftover modules without a state file", func(t *testing.T) {
		f := newFakePulse()
		dir := t.TempDir()
		mustAttach(t, NewRouter(f.run, dir), "airplay:A")
		if err := os.Remove(filepath.Join(dir, StateFile)); err != nil {
			t.Fatal(err)
		}

		if err := Recover(f.run, dir); err != nil {
			t.Fatal(err)
		}
		if m := f.castrModules(); len(m) != 0 {
			t.Errorf("castr modules left: %v", m)
		}
		if len(f.modules) != 2 {
			t.Errorf("modules %v; want only the two that are not castr's", f.modules)
		}
	})

	t.Run("nothing pending", func(t *testing.T) {
		f := newFakePulse()
		if err := Recover(f.run, t.TempDir()); err != nil {
			t.Fatal(err)
		}
		if c := f.changingCallsSince(0); len(c) != 0 {
			t.Errorf("Recover changed things with nothing pending: %v", c)
		}
	})
}

func TestAttachFailsCleanlyWithoutADefaultSink(t *testing.T) {
	empty := "\n"
	for name, breakIt := range map[string]func(*fakePulse){
		"get-default-sink fails": func(f *fakePulse) { f.defaultErr = errors.New("Connection refused") },
		"empty default":          func(f *fakePulse) { f.defaultOut = &empty },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakePulse()
			breakIt(f)
			dir := t.TempDir()
			r := NewRouter(f.run, dir)

			if _, err := r.Attach("airplay:A"); err == nil {
				t.Fatal("Attach succeeded without a default sink")
			}
			if m := f.castrModules(); len(m) != 0 {
				t.Errorf("castr modules left: %v", m)
			}
			if c := f.changingCallsSince(0); len(c) != 0 {
				t.Errorf("changed things: %v", c)
			}
			stateGone(t, dir)
			if _, ok := r.Choice("airplay:A"); ok {
				t.Error("a failed attach reports a choice")
			}
		})
	}
}

func TestEachAttachGetsAFreshCastSink(t *testing.T) {
	// n is never reused within a process, so a late unload of an old cast's
	// sink can never hit a new cast.
	f := newFakePulse()
	r := NewRouter(f.run, t.TempDir())
	mustAttach(t, r, "airplay:A")
	if err := r.Detach("airplay:A"); err != nil {
		t.Fatal(err)
	}
	if src := mustAttach(t, r, "airplay:A"); src != "castr_cast_2.monitor" {
		t.Errorf("second attach source %q, want castr_cast_2.monitor", src)
	}
	if _, err := r.Attach("airplay:A"); err == nil {
		t.Error("attaching the same cast twice succeeded")
	}
}
