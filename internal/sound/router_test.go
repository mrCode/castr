package sound

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	mute        bool
	volume      string
	// target is the output an app stream is pinned to: by a move, or by a
	// target WirePlumber stored for its app. "" for a stream that follows
	// the default, as WirePlumber relinks it (linking.follow-default-target).
	target string
	// cleared holds which of target.object and target.node have been set
	// to -1; both clear the target.
	cleared map[string]bool
}

// nodeOf is a stream's object.id, its node id in PipeWire: not its pactl
// index.
func nodeOf(index int) int { return index + 10000 }

// fakeOutput is a source-output: a stream recording from a source, such as
// a receiver's capture of its cast's monitor.
type fakeOutput struct {
	index, source int
	owner         int    // 0 for a client's stream
	target        string // its target.object property; "" for none
}

// micSource is the id of the one source that is not a sink's monitor. A
// monitor's id is its sink's, as pipewire-pulse lists them.
const micSource = 7

type fakePulse struct {
	sinks   []fakeSink
	modules []fakeModule
	inputs  []fakeInput
	def     string
	nextID  int
	// Module ids come from a pool that hands back the most recently freed
	// id first, as PipeWire's pw_map does (pipewire/map.h): an id castr
	// remembers may by now name a different module.
	nextMod  int
	freeMods []int
	calls    [][]string
	outputs  []fakeOutput
	// streamOwners is the args of the module that owned each sink-input
	// ever created, by index: a test can tell castr's own streams from
	// others' after they are gone.
	streamOwners map[int]string
	// rememberedMute and rememberedVolume are what WirePlumber would restore
	// onto a new loopback stream it remembers by media.name.
	rememberedMute   bool
	rememberedVolume string

	defaultErr error
	defaultOut *string // overrides get-default-sink's output when set
	failCmd    string  // a pactl command, args joined by spaces, that fails

	// WirePlumber's stack of the outputs the user chose, most recent first
	// (state-default-nodes.lua): history[0] is default.configured.audio.sink,
	// and the key is absent exactly when the stack is empty. Writing the key
	// -- with pw-metadata, or set-default-sink, as pipewire-pulse does --
	// moves that output to the top; deleting it erases the whole stack. The
	// default is then the first output in the stack that exists.
	// noMetadata makes pw-metadata find no "default" metadata at all, and
	// metaFail makes its calls fail.
	history    []string
	noMetadata bool
	metaFail   bool
	// lagDefault makes get-default-sink answer with the default from before
	// the last write of the key, as when WirePlumber has not handled it yet.
	lagDefault bool
	lagged     string
	// failWrite makes the n-th write of the key with pw-metadata fail.
	failWrite, writes int
	// stateFile, when set, is WirePlumber's default-nodes state file, kept
	// in step with history.
	stateFile string
}

func newFakePulse() *fakePulse {
	f := &fakePulse{nextID: 100, nextMod: 0x20000000, def: realSink, streamOwners: map[int]string{}, rememberedVolume: "100%"}
	f.setConfigured(realSink)
	f.addSink(realSink, 0)
	// Modules that are not castr's. Neither may ever be unloaded. The first
	// has PipeWire's multi-line args, as `pactl list short modules` prints.
	// The second is the user's own loopback, playing into the real sink; its
	// stream is not an app's, and castr must never move it.
	userLoop := f.modID()
	f.modules = append(f.modules,
		fakeModule{id: 1, name: "libpipewire-module-rt", args: "{\n            nice.level    = -11\n            rt.prio       = 88\n        }"},
		fakeModule{id: userLoop, name: "module-loopback", args: "source=mic.monitor sink=" + realSink + " latency_msec=60"},
	)
	f.inputs = append(f.inputs, fakeInput{index: f.id(), sink: f.sinkID(realSink), owner: userLoop})
	f.addApp(realSink)
	f.addApp(realSink)
	return f
}

func (f *fakePulse) id() int { f.nextID++; return f.nextID }

func (f *fakePulse) modID() int {
	if n := len(f.freeMods); n > 0 {
		id := f.freeMods[n-1]
		f.freeMods = f.freeMods[:n-1]
		return id
	}
	f.nextMod++
	return f.nextMod
}

func (f *fakePulse) moduleByID(id int) (fakeModule, bool) {
	for _, m := range f.modules {
		if m.id == id {
			return m, true
		}
	}
	return fakeModule{}, false
}

func (f *fakePulse) addSink(name string, module int) {
	f.sinks = append(f.sinks, fakeSink{id: f.id(), name: name, module: module})
}

// addApp starts an app stream on sink. On the default it follows the
// default; anywhere else it is pinned there, as by the user or by a target
// WirePlumber stored for its app.
func (f *fakePulse) addApp(sink string) int {
	idx := f.id()
	in := fakeInput{index: idx, sink: f.sinkID(sink)}
	if sink != f.def {
		in.target = sink
	}
	f.inputs = append(f.inputs, in)
	return idx
}

// relink puts each app stream where WirePlumber links it: on its target
// while that exists, else on the default.
func (f *fakePulse) relink() {
	def := f.sinkID(f.def)
	for i := range f.inputs {
		in := &f.inputs[i]
		if in.owner != 0 {
			continue
		}
		if id := f.sinkID(in.target); in.target != "" && id >= 0 {
			in.sink = id
		} else if def >= 0 {
			in.sink = def
		}
	}
}

// pinned returns the target of each app stream that has one, by index.
func (f *fakePulse) pinned() map[int]string {
	out := map[int]string{}
	for _, i := range f.inputs {
		if i.owner == 0 && i.target != "" {
			out[i.index] = i.target
		}
	}
	return out
}

func (f *fakePulse) sinkID(name string) int {
	for _, s := range f.sinks {
		if s.name == name {
			return s.id
		}
	}
	return -1
}

// sourceID is a source's id by name: a sink's monitor, or the mic.
func (f *fakePulse) sourceID(name string) int {
	if name == "mic" {
		return micSource
	}
	if sink, ok := strings.CutSuffix(name, ".monitor"); ok {
		return f.sinkID(sink)
	}
	return -1
}

// addCapture adds a client's stream recording from source, with target as
// its target.object.
func (f *fakePulse) addCapture(source, target string) int {
	idx := f.id()
	f.outputs = append(f.outputs, fakeOutput{index: idx, source: f.sourceID(source), target: target})
	return idx
}

func (f *fakePulse) captureSource(idx int) string {
	for _, o := range f.outputs {
		if o.index == idx {
			if o.source == micSource {
				return "mic"
			}
			return f.sinkName(o.source) + ".monitor"
		}
	}
	return ""
}

func (f *fakePulse) sinkName(id int) string {
	for _, s := range f.sinks {
		if s.id == id {
			return s.name
		}
	}
	return ""
}

// removeSink makes a sink vanish the way a Bluetooth disconnect does: a
// vanished default is chosen again, and the streams fall back to the default. A loopback into it that was loaded with
// sink_dont_move=true unloads itself instead, as PulseAudio's does; one
// without is relinked to the default like any stream.
func (f *fakePulse) removeSink(name string) {
	id := f.sinkID(name)
	var mods []fakeModule
	for _, m := range f.modules {
		if m.name == "module-loopback" && moduleArg(m.args, "sink") == name && moduleArg(m.args, "sink_dont_move") == "true" {
			f.dropInputsOf(m.id)
			f.freeMods = append(f.freeMods, m.id)
			continue
		}
		mods = append(mods, m)
	}
	f.modules = mods
	var kept []fakeSink
	for _, s := range f.sinks {
		if s.id != id {
			kept = append(kept, s)
		}
	}
	f.sinks = kept
	if f.def == name {
		f.reselect()
	}
	for i := range f.inputs {
		if f.inputs[i].sink == id {
			f.inputs[i].sink = f.sinkID(f.def)
		}
	}
}

func moduleArg(args, key string) string {
	for _, a := range strings.Fields(args) {
		if k, v, _ := strings.Cut(a, "="); k == key {
			return v
		}
	}
	return ""
}

func (f *fakePulse) dropInputsOf(module int) {
	var inputs []fakeInput
	for _, i := range f.inputs {
		if i.owner != module {
			inputs = append(inputs, i)
		}
	}
	f.inputs = inputs
}

// setConfigured writes default.configured.audio.sink: name moves to the top
// of the stack. The default is left as it is, so a test can set up any
// starting point.
func (f *fakePulse) setConfigured(name string) {
	f.history = append([]string{name}, slices.DeleteFunc(slices.Clone(f.history), func(s string) bool { return s == name })...)
	f.saveStateFile()
}

// configured is default.configured.audio.sink, nil when the key is absent.
func (f *fakePulse) configured() *string {
	if len(f.history) == 0 {
		return nil
	}
	return &f.history[0]
}

// writeConfigured is what WirePlumber does when the key is written: the
// output moves to the top of the stack, and the default is chosen again.
func (f *fakePulse) writeConfigured(name string) {
	f.lagged = f.def
	f.setConfigured(name)
	f.reselect()
}

// deleteConfigured is what WirePlumber does when the key is deleted: the
// whole stack is erased.
func (f *fakePulse) deleteConfigured() {
	f.history = nil
	f.saveStateFile()
	f.reselect()
}

// reselect picks the default as WirePlumber does: the first output in the
// stack that exists, or else, by priority, a hardware output; failing both
// the default stays.
func (f *fakePulse) reselect() {
	for _, name := range f.history {
		if f.sinkID(name) >= 0 {
			f.def = name
			return
		}
	}
	for _, s := range f.sinks {
		if s.module == 0 {
			f.def = s.name
			return
		}
	}
}

// saveStateFile writes the stack as WirePlumber's state file does, keys in
// no particular order, with a key of another type alongside.
func (f *fakePulse) saveStateFile() {
	if f.stateFile == "" {
		return
	}
	lines := []string{"default.configured.audio.source=alsa_input.pci-0000_00_1f.3.analog-stereo"}
	for i, name := range f.history {
		key := "default.configured.audio.sink"
		if i > 0 {
			key += "." + strconv.Itoa(i-1)
		}
		lines = append(lines, key+"="+name)
	}
	slices.Reverse(lines)
	raw := "[default-nodes]\n" + strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(f.stateFile, []byte(raw), 0o600); err != nil {
		panic(err)
	}
}

// withStateFile gives the fake a WirePlumber state file and returns its path.
func (f *fakePulse) withStateFile(t *testing.T) string {
	t.Helper()
	f.stateFile = filepath.Join(t.TempDir(), "default-nodes")
	f.saveStateFile()
	return f.stateFile
}

// metadata models pw-metadata on the "default" metadata, as measured on
// PipeWire 1.6.9: it prints a "Found" line, then one update line per key it
// shows; a key that is absent prints nothing more, and with no "default"
// metadata it prints nothing at all and still exits 0. Writing the key makes
// WirePlumber follow it when that output exists.
func (f *fakePulse) metadata(args []string) (string, error) {
	if f.metaFail {
		return "", errors.New("pw-metadata: signal: killed")
	}
	if f.noMetadata {
		return "", nil
	}
	const found = "Found \"default\" metadata 40\n"
	const key = "default.configured.audio.sink"
	switch strings.Join(args, " ") {
	case "-n default 0 " + key:
		c := f.configured()
		if c == nil {
			return found, nil
		}
		return found + fmt.Sprintf("update: id:0 key:'%s' value:'{\"name\":\"%s\"}' type:'Spa:String:JSON'\n", key, *c), nil
	case "-n default -d 0 " + key:
		f.deleteConfigured()
		return found, nil
	}
	if len(args) == 7 && strings.Join(args[:3], " ") == "-n default --" && args[5] == "-1" && args[6] == "Spa:Id" &&
		(args[4] == "target.object" || args[4] == "target.node") {
		// Setting both keys to -1 deletes the stored target: the stream
		// follows the default. Only one leaves it, as measured.
		node, _ := strconv.Atoi(args[3])
		for i := range f.inputs {
			in := &f.inputs[i]
			if nodeOf(in.index) != node {
				continue
			}
			if in.cleared == nil {
				in.cleared = map[string]bool{}
			}
			in.cleared[args[4]] = true
			if in.cleared["target.object"] && in.cleared["target.node"] {
				in.target, in.cleared = "", nil
			}
		}
		return found, nil
	}
	if len(args) == 6 && strings.Join(args[:4], " ") == "-n default 0 "+key && args[5] == "Spa:String:JSON" {
		var v struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(args[4]), &v); err != nil || v.Name == "" {
			return "", fmt.Errorf("bad value %q", args[4])
		}
		if f.writes++; f.writes == f.failWrite {
			return "", errors.New("pw-metadata: signal: killed")
		}
		f.writeConfigured(v.Name)
		return found, nil
	}
	return "", fmt.Errorf("unexpected pw-metadata %q", args)
}

func (f *fakePulse) run(name string, args ...string) (string, error) {
	f.relink()
	defer f.relink()
	f.calls = append(f.calls, append([]string{name}, args...))
	if name == "pw-metadata" {
		return f.metadata(args)
	}
	if name != "pactl" {
		return "", fmt.Errorf("unexpected command %s", name)
	}
	cmd := strings.Join(args, " ")
	if f.failCmd != "" && cmd == f.failCmd {
		return "", errors.New("Connection failure: Connection refused")
	}
	switch {
	case cmd == "get-default-sink":
		if f.defaultErr != nil {
			return "", f.defaultErr
		}
		if f.defaultOut != nil {
			return *f.defaultOut, nil
		}
		if f.lagDefault && f.lagged != "" {
			return f.lagged + "\n", nil
		}
		return f.def + "\n", nil
	case cmd == "list short sinks":
		var b strings.Builder
		for _, s := range f.sinks {
			fmt.Fprintf(&b, "%d\t%s\tPipeWire\tfloat32le 2ch 48000Hz\tSUSPENDED\n", s.id, s.name)
		}
		return b.String(), nil
	case cmd == "list short sources":
		var b strings.Builder
		fmt.Fprintf(&b, "%d\tmic\tPipeWire\ts32le 2ch 48000Hz\tSUSPENDED\n", micSource)
		for _, s := range f.sinks {
			fmt.Fprintf(&b, "%d\t%s.monitor\tPipeWire\tfloat32le 2ch 48000Hz\tSUSPENDED\n", s.id, s.name)
		}
		return b.String(), nil
	case cmd == "--format=json list source-outputs":
		type out struct {
			Index  int               `json:"index"`
			Source int               `json:"source"`
			Owner  *string           `json:"owner_module"`
			Props  map[string]string `json:"properties"`
		}
		list := []out{}
		for _, o := range f.outputs {
			var owner *string
			if o.owner != 0 {
				s := strconv.Itoa(o.owner)
				owner = &s
			}
			// As measured during a real cast: the helper's pulsesrc.
			props := map[string]string{"application.name": "gst-launch-1.0", "media.name": "pulsesrc"}
			if o.target != "" {
				props["target.object"] = o.target
			}
			list = append(list, out{Index: o.index, Source: o.source, Owner: owner, Props: props})
		}
		raw, _ := json.Marshal(list)
		return string(raw), nil
	case cmd == "list short modules":
		var b strings.Builder
		for _, m := range f.modules {
			fmt.Fprintf(&b, "%d\t%s\t%s\t\n", m.id, m.name, m.args)
		}
		return b.String(), nil
	case cmd == "--format=json list sink-inputs":
		type in struct {
			Index int               `json:"index"`
			Sink  int               `json:"sink"`
			Owner *string           `json:"owner_module"`
			Mute  bool              `json:"mute"`
			Props map[string]string `json:"properties"`
		}
		out := []in{}
		for _, i := range f.inputs {
			var owner *string
			if i.owner != 0 {
				s := strconv.Itoa(i.owner)
				owner = &s
			}
			out = append(out, in{Index: i.index, Sink: i.sink, Owner: owner, Mute: i.mute,
				Props: map[string]string{"application.name": "app-" + strconv.Itoa(i.index), "object.id": strconv.Itoa(nodeOf(i.index))}})
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
	case "set-sink-input-mute", "set-sink-input-volume":
		idx, _ := strconv.Atoi(args[1])
		for i := range f.inputs {
			if f.inputs[i].index != idx {
				continue
			}
			if args[0] == "set-sink-input-volume" {
				f.inputs[i].volume = args[2]
				return "", nil
			}
			switch args[2] {
			case "1":
				f.inputs[i].mute = true
			case "0":
				f.inputs[i].mute = false
			default:
				return "", fmt.Errorf("bad mute %q", args[2])
			}
			return "", nil
		}
		return "", errors.New("No such entity")
	case "move-source-output":
		idx, _ := strconv.Atoi(args[1])
		src := f.sourceID(args[2])
		if src < 0 {
			return "", errors.New("No such entity")
		}
		for i := range f.outputs {
			if f.outputs[i].index == idx {
				f.outputs[i].source = src
				return "", nil
			}
		}
		return "", errors.New("No such entity")
	case "set-default-sink":
		if f.sinkID(args[1]) < 0 {
			return "", errors.New("No such entity")
		}
		f.writeConfigured(args[1])
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
				if f.inputs[i].owner == 0 {
					f.inputs[i].target = args[2] // WirePlumber stores it for the app
				}
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
	id := f.modID()
	switch args[0] {
	case "module-null-sink":
		if kv["sink_name"] == "" || f.sinkID(kv["sink_name"]) >= 0 {
			f.freeMods = append(f.freeMods, id)
			return "", errors.New("bad sink_name")
		}
		f.addSink(kv["sink_name"], id)
	case "module-loopback":
		src := strings.TrimSuffix(kv["source"], ".monitor")
		if f.sinkID(src) < 0 || f.sinkID(kv["sink"]) < 0 || kv["latency_msec"] != "30" {
			f.freeMods = append(f.freeMods, id)
			return "", errors.New("bad loopback")
		}
		idx := f.id()
		f.inputs = append(f.inputs, fakeInput{index: idx, sink: f.sinkID(kv["sink"]), owner: id,
			mute: f.rememberedMute, volume: f.rememberedVolume})
		f.streamOwners[idx] = strings.Join(args[1:], " ")
	default:
		f.freeMods = append(f.freeMods, id)
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
	f.freeMods = append(f.freeMods, id)
	f.dropInputsOf(id)
	for _, s := range f.sinks {
		if s.module == id {
			f.removeSink(s.name) // the session manager picks another default
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

// loopStream returns the stream of castr's loopback from castr_mix into
// sink, failing the test when there is none.
func (f *fakePulse) loopStream(t *testing.T, sink string) fakeInput {
	t.Helper()
	want := "source=castr_mix.monitor sink=" + sink + " "
	for _, m := range f.modules {
		if m.name != "module-loopback" || !strings.HasPrefix(m.args, want) {
			continue
		}
		for _, i := range f.inputs {
			if i.owner == m.id {
				return i
			}
		}
		t.Fatalf("the loopback into %s has no stream", sink)
	}
	t.Fatalf("no loopback castr_mix.monitor -> %s", sink)
	return fakeInput{}
}

// plays reports whether castr_mix's sound reaches sink: its loopback is
// there and its stream is not muted.
func (f *fakePulse) plays(t *testing.T, sink string) bool {
	t.Helper()
	return !f.loopStream(t, sink).mute
}

// setStreamMute mutes or unmutes a stream from outside castr, as the user
// can in pavucontrol.
func (f *fakePulse) setStreamMute(idx int, mute bool) {
	for i := range f.inputs {
		if f.inputs[i].index == idx {
			f.inputs[i].mute = mute
		}
	}
}

// checkLoopbacksNeverMove: a castr loopback PipeWire relinks after its
// output vanishes would land on the default, castr_mix, and feed castr_mix
// back into itself.
func (f *fakePulse) checkLoopbacksNeverMove(t *testing.T) {
	t.Helper()
	n := 0
	for _, m := range f.castrModules() {
		if m.name != "module-loopback" {
			continue
		}
		n++
		if moduleArg(m.args, "sink_dont_move") != "true" || moduleArg(m.args, "source_dont_move") != "true" {
			t.Errorf("loopback %q may be moved by PipeWire", m.args)
		}
	}
	if n == 0 {
		t.Error("no castr loopback to check")
	}
}

// checkNoFeedback: no castr loopback stream may play into castr_mix.
func (f *fakePulse) checkNoFeedback(t *testing.T) {
	t.Helper()
	for _, i := range f.inputs {
		if m, ok := f.moduleByID(i.owner); ok && isCastrModule(m.args) && i.sink == f.sinkID("castr_mix") {
			t.Errorf("castr's own loopback stream %d plays into castr_mix", i.index)
		}
	}
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

// userLoopOn checks the user's own loopback stream still plays where it did.
func (f *fakePulse) userLoopOn(t *testing.T, sink string) {
	t.Helper()
	for _, i := range f.inputs {
		if m, ok := f.moduleByID(i.owner); ok && moduleArg(m.args, "source") == "mic.monitor" {
			if got := f.sinkName(i.sink); got != sink {
				t.Errorf("the user's own loopback stream was moved to %s, want it on %s", got, sink)
			}
			return
		}
	}
	t.Error("the user's own loopback stream is gone")
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
	if c[0] == "pw-metadata" {
		return len(c) > 5 || slices.Contains(c, "-d") // a write or a delete
	}
	switch c[1] {
	case "load-module", "unload-module", "set-sink-volume", "set-sink-mute", "set-default-sink", "move-sink-input",
		"set-sink-input-mute", "set-sink-input-volume", "move-source-output":
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

func readState(t *testing.T, dir string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, StateFile))
	if err != nil {
		t.Fatalf("reading the state file: %v", err)
	}
	var s map[string]any
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
	if !f.plays(t, "castr_cast_1") {
		t.Error("castr_cast_1 gets no sound")
	}
	if f.plays(t, realSink) {
		t.Error("the laptop loopback is not muted, so the laptop is not silent")
	}
	f.checkLoopbacksNeverMove(t)
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

func TestOnlyTheDefaultOutputsAppsMoveIntoCastr(t *testing.T) {
	// A call app the user put on a headset while music plays on the
	// speakers stays on the headset, and a stream another module owns -- a
	// combine-sink's, a user's own loopback -- is never moved: moved into
	// castr_mix it could feed back, and moved "back" it could feed its own
	// sink. At the end each stream is where it was.
	f := newFakePulse()
	f.addSink("headset", 0)
	call := f.addApp("headset")
	dir := t.TempDir()
	r := NewRouter(f.run, dir)

	mustAttach(t, r, "airplay:A")
	mustAttach(t, r, "airplay:B")
	mustSet(t, r, "airplay:A", Both)

	for _, i := range f.inputs {
		switch {
		case i.index == call:
			if s := f.sinkName(i.sink); s != "headset" {
				t.Errorf("the call on the headset was moved to %s", s)
			}
		case i.owner == 0:
			if s := f.sinkName(i.sink); s != "castr_mix" {
				t.Errorf("an app on the default output is on %s, want castr_mix", s)
			}
		}
	}
	f.userLoopOn(t, realSink)
	f.checkNoFeedback(t)

	if err := r.Detach("airplay:A"); err != nil {
		t.Fatal(err)
	}
	if err := r.Detach("airplay:B"); err != nil {
		t.Fatal(err)
	}
	for _, i := range f.inputs {
		want := realSink
		if i.index == call {
			want = "headset"
		}
		if i.owner == 0 && f.sinkName(i.sink) != want {
			t.Errorf("after the cast app stream %d is on %s, want %s", i.index, f.sinkName(i.sink), want)
		}
	}
	f.userLoopOn(t, realSink)
	stateGone(t, dir)
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
	if !f.plays(t, realSink) {
		t.Error("A=laptop: the laptop is silent")
	}
	if f.plays(t, "castr_cast_1") {
		t.Error("A=laptop: A's receiver still gets sound")
	}
	if !f.plays(t, "castr_cast_2") {
		t.Error("A=laptop: B's receiver lost its sound")
	}

	mustSet(t, r, "airplay:A", TV)
	if f.plays(t, realSink) {
		t.Error("both tv: the laptop still plays")
	}

	mustSet(t, r, "airplay:A", Both)
	if !f.plays(t, "castr_cast_1") || !f.plays(t, realSink) {
		t.Error("A=both: want A's receiver and the laptop to play")
	}
	f.checkLoopbacksNeverMove(t)
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
		case c[1] == "set-sink-input-mute":
		case c[1] == "unload-module" && castSinks[c[2]]:
			t.Errorf("switching unloaded a cast sink: %v", c)
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
	// not exist, and with neither saved output left it leaves WirePlumber's
	// choice in place.
	t.Run("neither saved output is left", func(t *testing.T) {
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
			if len(c) > 1 && c[1] == "set-default-sink" && c[2] != "castr_mix" {
				t.Errorf("ran %v; with both saved outputs gone the session manager's choice stands", c)
			}
		}
		if m := f.castrModules(); len(m) != 0 {
			t.Errorf("castr modules left: %v", m)
		}
		stateGone(t, dir)
	})

	t.Run("the laptop output is gone, the previous default is not", func(t *testing.T) {
		f := newFakePulse()
		dir := t.TempDir()
		r := NewRouter(f.run, dir)
		mustAttach(t, r, "airplay:A")
		f.addSink("bluez_x", 0)
		f.def = "bluez_x"
		if err := r.Follow(); err != nil {
			t.Fatal(err)
		}
		f.removeSink("bluez_x")

		if err := r.Detach("airplay:A"); err != nil {
			t.Fatal(err)
		}

		for _, c := range f.calls {
			if len(c) > 2 && c[1] == "set-default-sink" && c[2] == "bluez_x" && f.sinkID("bluez_x") < 0 {
				t.Errorf("ran %v against a sink that is gone", c)
			}
		}
		if f.def != realSink {
			t.Errorf("default is %s, want the previous default %s", f.def, realSink)
		}
		f.allAppsOn(t, realSink)
		if m := f.castrModules(); len(m) != 0 {
			t.Errorf("castr modules left: %v", m)
		}
		stateGone(t, dir)
	})
}

func TestRestoreKeepsTheOutputTheUserSwitchedTo(t *testing.T) {
	// Headphones plugged in mid-cast are what the user is using now; putting
	// the old speakers back would undo their choice.
	switchMidCast := func(t *testing.T) (*fakePulse, *Router, string) {
		f := newFakePulse()
		dir := t.TempDir()
		r := NewRouter(f.run, dir)
		mustAttach(t, r, "airplay:A")
		f.addSink("bluez_x", 0)
		f.def = "bluez_x"
		f.addApp("bluez_x")
		if err := r.Follow(); err != nil {
			t.Fatal(err)
		}
		return f, r, dir
	}

	t.Run("the last detach", func(t *testing.T) {
		f, r, dir := switchMidCast(t)
		if err := r.Detach("airplay:A"); err != nil {
			t.Fatal(err)
		}
		if f.def != "bluez_x" {
			t.Errorf("default is %s, want bluez_x", f.def)
		}
		f.allAppsOn(t, "bluez_x")
		stateGone(t, dir)
	})

	t.Run("recovery after a crash", func(t *testing.T) {
		f, _, dir := switchMidCast(t)
		if err := Recover(f.run, dir); err != nil {
			t.Fatal(err)
		}
		if f.def != "bluez_x" {
			t.Errorf("default is %s, want bluez_x", f.def)
		}
		f.allAppsOn(t, "bluez_x")
		if m := f.castrModules(); len(m) != 0 {
			t.Errorf("castr modules left: %v", m)
		}
		stateGone(t, dir)
	})
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
			t.Errorf("module-owned stream %d was moved onto castr_mix", i.index)
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

func TestFollowSurvivesATransientPactlFailure(t *testing.T) {
	// Only a confirmed-missing castr_mix ends the routing; anything else is
	// retried on the next tick.
	for _, cmd := range []string{"list short sinks", "get-default-sink", "--format=json list sink-inputs",
		"list short sources", "--format=json list source-outputs"} {
		t.Run(cmd, func(t *testing.T) {
			f := newFakePulse()
			dir := t.TempDir()
			r := NewRouter(f.run, dir)
			mustAttach(t, r, "airplay:A")
			mustSet(t, r, "airplay:A", Both)
			// A changed default makes Follow reach every query it makes.
			f.addSink("bluez_x", 0)
			f.def = "bluez_x"
			f.addApp("bluez_x")
			f.failCmd = cmd

			err := r.Follow()
			if err == nil && cmd != "--format=json list sink-inputs" {
				t.Fatal("Follow hid the failure")
			}
			if err != nil && strings.Contains(err.Error(), "lost") {
				t.Errorf("Follow = %v; a transient failure is not a lost routing", err)
			}
			if c, ok := r.Choice("airplay:A"); !ok || c != Both {
				t.Errorf("Choice = %q, %v; want both, still routed", c, ok)
			}
			if _, err := os.Stat(filepath.Join(dir, StateFile)); err != nil {
				t.Errorf("state file: %v; the routing was torn down", err)
			}
			if !f.hasLoopback("castr_mix.monitor", "castr_cast_1") {
				t.Error("A's receiver lost its sound")
			}

			f.failCmd = ""
			if err := r.Follow(); err != nil {
				t.Fatalf("the retry: %v", err)
			}
			if f.def != "castr_mix" {
				t.Errorf("after the retry the default is %s, want castr_mix", f.def)
			}
			if !f.hasLoopback("castr_mix.monitor", "bluez_x") {
				t.Error("after the retry the laptop does not play on bluez_x")
			}
			f.allAppsOn(t, "castr_mix")
			f.checkNoFeedback(t)
		})
	}
}

func TestFollowFindsANewOutputWhenTheLaptopOutputDisappears(t *testing.T) {
	// The default stays castr_mix when the laptop's output vanishes, so
	// nothing else would tell castr: the laptop would stay silent.
	onBluetooth := func(t *testing.T, extra ...string) (*fakePulse, *Router, string) {
		f := newFakePulse()
		for _, s := range extra {
			f.addSink(s, 0)
		}
		dir := t.TempDir()
		r := NewRouter(f.run, dir)
		mustAttach(t, r, "airplay:A")
		mustSet(t, r, "airplay:A", Laptop)
		f.addSink("bluez_x", 0)
		f.def = "bluez_x"
		if err := r.Follow(); err != nil {
			t.Fatal(err)
		}
		f.removeSink("bluez_x")
		return f, r, dir
	}

	t.Run("back to the previous default", func(t *testing.T) {
		f, r, dir := onBluetooth(t)
		if err := r.Follow(); err != nil {
			t.Fatal(err)
		}
		if !f.hasLoopback("castr_mix.monitor", realSink) {
			t.Error("the laptop does not play on the previous default")
		}
		if s := readState(t, dir); s["laptop_sink"] != realSink {
			t.Errorf("laptop_sink %q, want %s", s["laptop_sink"], realSink)
		}
		if f.def != "castr_mix" {
			t.Errorf("default is %s, want castr_mix", f.def)
		}
		f.checkNoFeedback(t)
		f.checkLoopbacksNeverMove(t)

		n := len(f.calls)
		if err := r.Follow(); err != nil {
			t.Fatal(err)
		}
		if c := f.changingCallsSince(n); len(c) != 0 {
			t.Errorf("a second Follow changed things: %v", c)
		}
	})

	t.Run("to another real output", func(t *testing.T) {
		f, r, dir := onBluetooth(t, "speaker2")
		f.removeSink(realSink)
		if err := r.Follow(); err != nil {
			t.Fatal(err)
		}
		if !f.hasLoopback("castr_mix.monitor", "speaker2") {
			t.Error("the laptop does not play on speaker2")
		}
		if s := readState(t, dir); s["laptop_sink"] != "speaker2" {
			t.Errorf("laptop_sink %q, want speaker2", s["laptop_sink"])
		}
		f.checkNoFeedback(t)
	})

	t.Run("no real output left", func(t *testing.T) {
		f, r, dir := onBluetooth(t)
		f.removeSink(realSink)
		if err := r.Follow(); err != nil {
			t.Fatal(err)
		}
		for _, m := range f.castrModules() {
			if m.name == "module-loopback" && moduleArg(m.args, "sink") != "castr_cast_1" && moduleArg(m.args, "sink") != "" {
				t.Errorf("a laptop loopback exists with no output: %q", m.args)
			}
		}
		if s := readState(t, dir); s["laptop_sink"] != "" {
			t.Errorf("laptop_sink %q, want none", s["laptop_sink"])
		}
		f.checkNoFeedback(t)

		n := len(f.calls)
		if err := r.Follow(); err != nil {
			t.Fatal(err)
		}
		if c := f.changingCallsSince(n); len(c) != 0 {
			t.Errorf("a second Follow changed things: %v", c)
		}
	})
}

func TestAVanishedLoopbacksIdIsNeverUnloadedWhenReused(t *testing.T) {
	// The laptop's Bluetooth output disconnects: its pinned loopback unloads
	// itself, and PipeWire hands its id to the next module loaded. castr must
	// never unload that id on the strength of remembering it.
	onBluetooth := func(t *testing.T) (*fakePulse, *Router, int) {
		f := newFakePulse()
		r := NewRouter(f.run, t.TempDir())
		mustAttach(t, r, "airplay:A")
		mustSet(t, r, "airplay:A", Laptop)
		f.addSink("bluez_x", 0)
		f.def = "bluez_x"
		if err := r.Follow(); err != nil {
			t.Fatal(err)
		}
		old := -1
		for _, m := range f.castrModules() {
			if m.name == "module-loopback" && moduleArg(m.args, "sink") == "bluez_x" {
				old = m.id
			}
		}
		if old < 0 {
			t.Fatal("no laptop loopback to bluez_x")
		}
		f.removeSink("bluez_x")
		return f, r, old
	}

	t.Run("Follow's new laptop loopback", func(t *testing.T) {
		f, r, old := onBluetooth(t)
		n := len(f.calls)
		if err := r.Follow(); err != nil {
			t.Fatal(err)
		}
		for _, c := range f.calls[n:] {
			if len(c) > 2 && c[1] == "unload-module" && c[2] == strconv.Itoa(old) {
				t.Errorf("ran %v: that id had gone, and was handed to the new loopback", c)
			}
		}
		m, ok := f.moduleByID(old)
		if !ok || moduleArg(m.args, "sink") != realSink {
			t.Fatalf("module %d is %+v, %v; want the new laptop loopback, reusing the freed id", old, m, ok)
		}
		if err := r.Follow(); err != nil {
			t.Fatal(err)
		}
		if !f.hasLoopback("castr_mix.monitor", realSink) {
			t.Error("the laptop is silent: its new loopback was unloaded")
		}
	})

	t.Run("a switch before Follow notices", func(t *testing.T) {
		f, r, old := onBluetooth(t)
		mustAttach(t, r, "airplay:B") // its sink takes the freed id
		if m, ok := f.moduleByID(old); !ok || moduleArg(m.args, "sink_name") != "castr_cast_2" {
			t.Fatalf("module %d is %+v, %v; want B's sink, reusing the freed id", old, m, ok)
		}
		mustSet(t, r, "airplay:A", TV)
		if f.sinkID("castr_cast_2") < 0 {
			t.Error("B's sink was unloaded in place of A's vanished laptop loopback")
		}
		mustSet(t, r, "airplay:A", Laptop)
		if err := r.Follow(); err != nil {
			t.Fatal(err)
		}
		if !f.hasLoopback("castr_mix.monitor", realSink) {
			t.Error("the laptop does not play after switching back to laptop")
		}
		if f.sinkID("castr_cast_2") < 0 || !f.hasLoopback("castr_mix.monitor", "castr_cast_2") {
			t.Error("B lost its sink or its sound")
		}
	})
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

func TestTheRouterStartsTheGuardOnceOnFirstAttach(t *testing.T) {
	f := newFakePulse()
	r := NewRouter(f.run, t.TempDir())
	starts := 0
	r.StartGuard = func() error { starts++; return nil }

	mustAttach(t, r, "a")
	mustAttach(t, r, "b")
	if starts != 1 {
		t.Fatalf("guard started %d times across two attaches, want 1", starts)
	}
	if err := r.Restore(); err != nil {
		t.Fatal(err)
	}
	mustAttach(t, r, "c")
	if starts != 2 {
		t.Fatalf("guard started %d times after a restore and a new attach, want 2", starts)
	}
}

func TestTheStateFileNamesTheDaemonThatOwnsTheRouting(t *testing.T) {
	// A dead daemon's guard must not tear down a new daemon's routing.
	f := newFakePulse()
	dir := t.TempDir()
	r := NewRouter(f.run, dir)
	mustAttach(t, r, "airplay:A")
	if s := readState(t, dir); s["daemon_pid"] != float64(os.Getpid()) {
		t.Errorf("daemon_pid %v, want %d", s["daemon_pid"], os.Getpid())
	}
}

func TestAFailedRestoreKeepsTheStateFileUntilARetrySucceeds(t *testing.T) {
	// Deleting the state file after a restore that failed leaves the laptop
	// on castr_mix -- silent, with every cast on TV -- and nothing that
	// knows to retry.
	f := newFakePulse()
	dir := t.TempDir()
	r := NewRouter(f.run, dir)
	mustAttach(t, r, "airplay:A")
	f.failCmd = "list short modules"

	if err := r.Detach("airplay:A"); err == nil {
		t.Fatal("Detach hid the failure")
	}
	if s := readState(t, dir); s["previous_default"] != realSink {
		t.Fatalf("state %v; want it kept, naming %s", s, realSink)
	}

	if err := r.RetryRestore(); err == nil {
		t.Fatal("RetryRestore hid the failure")
	}
	readState(t, dir)

	f.failCmd = ""
	if err := r.RetryRestore(); err != nil {
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

	n := len(f.calls)
	if err := r.RetryRestore(); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != n {
		t.Errorf("with nothing pending RetryRestore ran %v", f.calls[n:])
	}
}

func TestRetryRestoreLeavesALiveRoutingAlone(t *testing.T) {
	f := newFakePulse()
	dir := t.TempDir()
	r := NewRouter(f.run, dir)
	mustAttach(t, r, "airplay:A")
	n := len(f.calls)
	if err := r.RetryRestore(); err != nil {
		t.Fatal(err)
	}
	if c := f.changingCallsSince(n); len(c) != 0 {
		t.Errorf("RetryRestore changed a live routing: %v", c)
	}
	if c, ok := r.Choice("airplay:A"); !ok || c != TV {
		t.Errorf("Choice = %q, %v; want tv", c, ok)
	}
}

func TestANewCastFinishesAPendingRestoreFirst(t *testing.T) {
	// The default the failed restore left is castr_mix: taking it as the
	// one to go back to would lose the real one.
	f := newFakePulse()
	dir := t.TempDir()
	r := NewRouter(f.run, dir)
	mustAttach(t, r, "airplay:A")
	f.failCmd = "list short modules"
	_ = r.Detach("airplay:A")
	f.failCmd = ""

	mustAttach(t, r, "airplay:B")
	if s := readState(t, dir); s["previous_default"] != realSink {
		t.Errorf("state %v, want previous_default %s", s, realSink)
	}
	if err := r.Detach("airplay:B"); err != nil {
		t.Fatal(err)
	}
	if f.def != realSink {
		t.Errorf("default is %s, want %s", f.def, realSink)
	}
	if m := f.castrModules(); len(m) != 0 {
		t.Errorf("castr modules left: %v", m)
	}
	stateGone(t, dir)
}

func TestAFailedRecoverKeepsTheStateFile(t *testing.T) {
	f := newFakePulse()
	dir := t.TempDir()
	mustAttach(t, NewRouter(f.run, dir), "airplay:A")
	f.failCmd = "list short modules"
	if err := Recover(f.run, dir); err == nil {
		t.Fatal("Recover hid the failure")
	}
	readState(t, dir)
	f.failCmd = ""
	if err := Recover(f.run, dir); err != nil {
		t.Fatal(err)
	}
	stateGone(t, dir)
}

func TestACorruptStateFileIsReported(t *testing.T) {
	f := newFakePulse()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, StateFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Recover(f.run, dir)
	if err == nil || !strings.Contains(err.Error(), StateFile) {
		t.Fatalf("Recover = %v, want the parse error", err)
	}
	stateGone(t, dir) // nothing in it to retry with; the restore itself worked
}

func TestFollowTakesBackAppsFromACastOutputMadeTheDefault(t *testing.T) {
	// The output pickers list "castr cast" during a cast. Picked, it is not
	// a laptop output: the default goes back to castr_mix, and so do the app
	// streams that followed the default there.
	f := newFakePulse()
	r := NewRouter(f.run, t.TempDir())
	mustAttach(t, r, "airplay:A")
	f.def = "castr_cast_1"
	f.addApp("castr_cast_1")

	if err := r.Follow(); err != nil {
		t.Fatal(err)
	}
	if f.def != "castr_mix" {
		t.Errorf("default is %s, want castr_mix", f.def)
	}
	f.allAppsOn(t, "castr_mix")
	if !f.hasLoopback("castr_mix.monitor", "castr_cast_1") {
		t.Error("A's receiver lost its sound")
	}
	f.checkNoFeedback(t)
}

func configuredIs(t *testing.T, f *fakePulse, want string) {
	t.Helper()
	switch c := f.configured(); {
	case want == "" && c != nil:
		t.Errorf("WirePlumber's configured default is %s, want the key absent", *c)
	case want != "" && c == nil:
		t.Errorf("WirePlumber's configured default is absent, want %s", want)
	case want != "" && *c != want:
		t.Errorf("WirePlumber's configured default is %s, want %s", *c, want)
	}
}

func TestRestorePutsBackWirePlumbersConfiguredDefault(t *testing.T) {
	// On this machine WirePlumber's configured default is Bluetooth
	// headphones that are not connected, and it plays through the speakers
	// meanwhile. set-default-sink at restore would make the speakers the
	// configured default, and the headphones would no longer take over when
	// they connect.
	const headphones = "bluez_output.4C_CF_7C_EF_03_D0.1"
	const write = "pw-metadata -n default 0 default.configured.audio.sink {\"name\":\"" + headphones + "\"} Spa:String:JSON"
	for _, how := range []string{"the last detach", "recovery after a crash"} {
		t.Run(how, func(t *testing.T) {
			f := newFakePulse()
			f.setConfigured(headphones)
			dir := t.TempDir()
			r := NewRouter(f.run, dir)
			mustAttach(t, r, "airplay:A")
			if c, ok := readState(t, dir)["configured_default"].(map[string]any); !ok || c["saved"] != true || c["name"] != headphones {
				t.Errorf("state saved configured_default %v, want %s", readState(t, dir)["configured_default"], headphones)
			}

			n := len(f.calls)
			if how == "the last detach" {
				if err := r.Detach("airplay:A"); err != nil {
					t.Fatal(err)
				}
			} else if err := Recover(f.run, dir); err != nil {
				t.Fatal(err)
			}

			configuredIs(t, f, headphones)
			if f.def != realSink {
				t.Errorf("default is %s, want %s", f.def, realSink)
			}
			wrote := false
			for _, c := range f.calls[n:] {
				wrote = wrote || strings.Join(c, " ") == write
			}
			if !wrote {
				t.Errorf("never ran %s", write)
			}
			f.allAppsOn(t, realSink)
			stateGone(t, dir)
		})
	}
}

func TestRestoreDeletesAConfiguredDefaultThatWasAbsent(t *testing.T) {
	f := newFakePulse()
	f.history = nil
	dir := t.TempDir()
	r := NewRouter(f.run, dir)
	mustAttach(t, r, "airplay:A")
	configuredIs(t, f, "castr_mix")

	if err := r.Detach("airplay:A"); err != nil {
		t.Fatal(err)
	}
	configuredIs(t, f, "")
	if f.def != realSink {
		t.Errorf("default is %s, want %s", f.def, realSink)
	}
	stateGone(t, dir)
}

func TestTheConfiguredDefaultKeepsTheOutputTheUserPickedMidCast(t *testing.T) {
	f := newFakePulse()
	r := NewRouter(f.run, t.TempDir())
	mustAttach(t, r, "airplay:A")
	f.addSink("bluez_x", 0)
	f.def = "bluez_x"
	f.setConfigured("bluez_x") // what picking it does
	if err := r.Follow(); err != nil {
		t.Fatal(err)
	}
	if err := r.Detach("airplay:A"); err != nil {
		t.Fatal(err)
	}
	configuredIs(t, f, "bluez_x")
	if f.def != "bluez_x" {
		t.Errorf("default is %s, want bluez_x", f.def)
	}
}

func TestAFailedConfiguredDefaultWriteIsRetried(t *testing.T) {
	const headphones = "bluez_hp"
	f := newFakePulse()
	f.setConfigured(headphones)
	dir := t.TempDir()
	r := NewRouter(f.run, dir)
	mustAttach(t, r, "airplay:A")
	f.metaFail = true
	if err := r.Detach("airplay:A"); err == nil {
		t.Fatal("Detach hid the failure")
	}
	readState(t, dir)
	f.metaFail = false
	if err := r.RetryRestore(); err != nil {
		t.Fatal(err)
	}
	configuredIs(t, f, headphones)
	stateGone(t, dir)
}

func TestRoutingNeedsWirePlumbersMetadata(t *testing.T) {
	// Without it castr could not put the configured default back -- and an
	// absent "Found" line is not an absent key: deleting the key at restore
	// would lose the user's choice.
	for name, breakIt := range map[string]func(*fakePulse){
		"pw-metadata fails":   func(f *fakePulse) { f.metaFail = true },
		"no default metadata": func(f *fakePulse) { f.noMetadata = true },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakePulse()
			breakIt(f)
			dir := t.TempDir()
			r := NewRouter(f.run, dir)
			if _, err := r.Attach("airplay:A"); err == nil {
				t.Fatal("Attach succeeded without WirePlumber's metadata")
			}
			if c := f.changingCallsSince(0); len(c) != 0 {
				t.Errorf("changed things: %v", c)
			}
			configuredIs(t, f, realSink)
			stateGone(t, dir)
		})
	}
}

func TestFollowAdoptsTheConfiguredDefaultWhenItAppears(t *testing.T) {
	// Bluetooth headphones connect mid-cast. WirePlumber would switch to
	// them, but castr_mix is its configured default now, so castr does it:
	// they become the laptop.
	const headphones = "bluez_hp"
	f := newFakePulse()
	f.setConfigured(headphones)
	dir := t.TempDir()
	r := NewRouter(f.run, dir)
	mustAttach(t, r, "airplay:A")
	mustSet(t, r, "airplay:A", Laptop)

	f.addSink(headphones, 0)
	if err := r.Follow(); err != nil {
		t.Fatal(err)
	}
	if !f.hasLoopback("castr_mix.monitor", headphones) {
		t.Error("the laptop does not play on the headphones")
	}
	if f.hasLoopback("castr_mix.monitor", realSink) {
		t.Error("the laptop still plays on the speakers")
	}
	if s := readState(t, dir); s["laptop_sink"] != headphones {
		t.Errorf("laptop_sink %v, want %s", s["laptop_sink"], headphones)
	}
	if f.def != "castr_mix" {
		t.Errorf("default is %s, want castr_mix", f.def)
	}
	f.checkNoFeedback(t)

	n := len(f.calls)
	if err := r.Follow(); err != nil {
		t.Fatal(err)
	}
	if c := f.changingCallsSince(n); len(c) != 0 {
		t.Errorf("a second Follow changed things: %v", c)
	}

	if err := r.Detach("airplay:A"); err != nil {
		t.Fatal(err)
	}
	if f.def != headphones {
		t.Errorf("default is %s, want %s", f.def, headphones)
	}
	configuredIs(t, f, headphones)
}

func TestFollowLeavesAConfiguredDefaultThatWasThereAllAlong(t *testing.T) {
	// Present at the start but not what WirePlumber played through: it has
	// not "appeared", and adopting it would move the sound unasked.
	f := newFakePulse()
	f.addSink("bluez_hp", 0)
	f.setConfigured("bluez_hp")
	r := NewRouter(f.run, t.TempDir())
	mustAttach(t, r, "airplay:A")
	mustSet(t, r, "airplay:A", Laptop)
	if err := r.Follow(); err != nil {
		t.Fatal(err)
	}
	if !f.hasLoopback("castr_mix.monitor", realSink) || f.hasLoopback("castr_mix.monitor", "bluez_hp") {
		t.Error("the laptop moved off the output it was playing through")
	}
}

func TestFollowLetsTheConfiguredDefaultComeBack(t *testing.T) {
	// The headphones the laptop played through disconnect and reconnect.
	f := newFakePulse()
	f.addSink("bluez_hp", 0)
	f.def = "bluez_hp"
	f.setConfigured("bluez_hp")
	r := NewRouter(f.run, t.TempDir())
	mustAttach(t, r, "airplay:A")
	mustSet(t, r, "airplay:A", Laptop)

	f.removeSink("bluez_hp")
	if err := r.Follow(); err != nil {
		t.Fatal(err)
	}
	if !f.hasLoopback("castr_mix.monitor", realSink) {
		t.Fatal("the laptop did not fall back to the speakers")
	}
	f.addSink("bluez_hp", 0)
	if err := r.Follow(); err != nil {
		t.Fatal(err)
	}
	if !f.hasLoopback("castr_mix.monitor", "bluez_hp") || f.hasLoopback("castr_mix.monitor", realSink) {
		t.Error("the laptop did not go back to the headphones")
	}
}

func TestACastrConfiguredDefaultLeftBehindIsNotPutBack(t *testing.T) {
	// An older castr left castr_mix as WirePlumber's configured default:
	// that was never the user's choice. Without the history file castr
	// cannot see what lies behind it, and deleting the key would erase it
	// all: the key is left alone.
	f := newFakePulse()
	f.setConfigured("bluez_hp")
	f.setConfigured("castr_mix")
	r := NewRouter(f.run, t.TempDir())
	mustAttach(t, r, "airplay:A")
	n := len(f.calls)
	if err := r.Detach("airplay:A"); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls[n:] {
		if c[0] == "pw-metadata" && isChanging(c) && slices.Contains(c, "default.configured.audio.sink") {
			t.Errorf("restore ran %v, want the key left alone", c)
		}
	}
	if !slices.Contains(f.history, "bluez_hp") || !slices.Contains(f.history, realSink) {
		t.Errorf("output history %v lost what was behind castr_mix", f.history)
	}
	if f.def != realSink {
		t.Errorf("default is %s, want %s", f.def, realSink)
	}
}

func TestNothingEverChangesARealOutputsVolume(t *testing.T) {
	// During a cast the volume keys act on castr_mix, the whole cast's
	// loudness. The real outputs' own volume and mute are never touched, so
	// they are exactly as they were after the cast.
	f := newFakePulse()
	f.setConfigured("bluez_hp")
	r := NewRouter(f.run, t.TempDir())
	mustAttach(t, r, "airplay:A")
	mustAttach(t, r, "chromecast:B")
	for _, c := range []Choice{Laptop, Both, TV, Both} {
		mustSet(t, r, "airplay:A", c)
	}
	f.addSink("bluez_hp", 0) // headphones connect
	if err := r.Follow(); err != nil {
		t.Fatal(err)
	}
	f.addSink("bluez_x", 0) // the user picks another output
	f.def = "bluez_x"
	if err := r.Follow(); err != nil {
		t.Fatal(err)
	}
	f.removeSink("bluez_x")
	if err := r.Follow(); err != nil {
		t.Fatal(err)
	}
	if err := r.Detach("airplay:A"); err != nil {
		t.Fatal(err)
	}
	if err := r.Detach("chromecast:B"); err != nil {
		t.Fatal(err)
	}

	for _, c := range f.calls {
		if len(c) > 2 && (c[1] == "set-sink-volume" || c[1] == "set-sink-mute") && !isCastrName(c[2]) {
			t.Errorf("touched a real output's volume: %v", c)
		}
		// castr's own loopback streams are castr's to mute; no other stream is.
		if len(c) > 2 && (c[1] == "set-sink-input-volume" || c[1] == "set-sink-input-mute") {
			if idx, _ := strconv.Atoi(c[2]); !isCastrModule(f.streamOwners[idx]) {
				t.Errorf("touched a stream's volume: %v", c)
			}
		}
	}
}

func TestLoopbacksHaveStableNames(t *testing.T) {
	// WirePlumber remembers each stream's volume under its media.name. A
	// loopback's default one, loopback-<pid>-<n>, is new on every load, so
	// every switch to Laptop or Both would add entries without bound.
	f := newFakePulse()
	r := NewRouter(f.run, t.TempDir())
	mustAttach(t, r, "airplay:A")
	mustAttach(t, r, "airplay:B")
	for _, c := range []Choice{Laptop, Both, TV, Laptop, Both} {
		mustSet(t, r, "airplay:A", c)
		mustSet(t, r, "airplay:B", c)
	}

	names := map[string]bool{}
	loads := 0
	for _, c := range f.calls {
		if len(c) < 3 || c[1] != "load-module" || c[2] != "module-loopback" {
			continue
		}
		loads++
		args := strings.Join(c[3:], " ")
		for _, side := range []string{"sink_input_properties", "source_output_properties"} {
			props, ok := quotedArg(args, side)
			if !ok {
				t.Errorf("loopback %q has no %s", args, side)
				continue
			}
			names[moduleArg(props, "media.name")] = true
			if !strings.HasPrefix(moduleArg(props, "node.name"), "castr_loop_") {
				t.Errorf("loopback %q: %s has no castr node.name", args, side)
			}
		}
	}
	if loads < 3 {
		t.Fatalf("only %d loopback loads", loads)
	}
	if len(names) != 2 || !names["castr_loop_laptop"] || !names["castr_loop_cast"] {
		t.Errorf("loopback media.names %v, want only castr_loop_laptop and castr_loop_cast", names)
	}
	f.checkLoopbacksNeverMove(t)
}

// quotedArg returns the value of key="..." in module args.
func quotedArg(args, key string) (string, bool) {
	_, rest, ok := strings.Cut(args, key+`="`)
	if !ok {
		return "", false
	}
	value, _, ok := strings.Cut(rest, `"`)
	return value, ok
}
