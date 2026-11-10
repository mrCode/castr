package sound

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Runner executes a command and returns stdout, the same shape as
// hypr.Runner. Injected so no test ever changes the real machine's sound.
type Runner func(name string, args ...string) (string, error)

// Names castr creates. A castr module is any whose args mention one of them;
// that is how leftovers are found even with no state file, and why nothing
// else on the system is ever unloaded.
const (
	mixSink    = "castr_mix"
	castPrefix = "castr_cast_"

	mixDescription  = "castr"
	castDescription = "castr cast"

	loopbackModule = "module-loopback"
	nullSinkModule = "module-null-sink"
)

func castSinkName(n int) string { return castPrefix + strconv.Itoa(n) }

func isCastrName(name string) bool {
	return name == mixSink || strings.HasPrefix(name, castPrefix)
}

func isCastrModule(args string) bool {
	return strings.Contains(args, mixSink) || strings.Contains(args, castPrefix)
}

type sink struct {
	id   int
	name string
}

type module struct {
	id         int
	name, args string
}

// modRef is a module castr loaded, as castr loaded it. PipeWire hands the
// most recently freed module id to the next module loaded (pipewire/map.h),
// so a remembered id may by now name some other module -- a loopback that
// unloaded itself when its output vanished, say, replaced by castr's own new
// loopback. A remembered module is therefore unloaded only while pactl still
// lists it with the id, the name and the identifying args castr gave it.
type modRef struct {
	id   int
	name string
	sig  []string // args that identify it, e.g. source=... and sink=...
}

func (m modRef) loaded() bool { return m.id != 0 }

// in reports whether mods still lists m as castr loaded it.
func (m modRef) in(mods []module) bool {
	if m.id == 0 {
		return false
	}
	for _, l := range mods {
		if l.id != m.id || l.name != m.name {
			continue
		}
		fields := strings.Fields(l.args)
		for _, want := range m.sig {
			if !slices.Contains(fields, want) {
				return false
			}
		}
		return true
	}
	return false
}

// unloadRef unloads m if mods still lists it as castr's. A module that has
// gone already needs no unloading, and an id that now names another module
// must not be touched.
func unloadRef(run Runner, m modRef, mods []module) error {
	if !m.in(mods) {
		return nil
	}
	return unloadModule(run, m.id)
}

type sinkInput struct {
	index, sink int
	owner       int // -1 for a stream no module owns: an app
	mute        bool
}

// sourceOutput is a stream recording from a source.
type sourceOutput struct {
	index, source int
	owner         int    // -1 for a stream no module owns
	target        string // its target.object; "" for none
}

func pactl(run Runner, args ...string) (string, error) {
	out, err := run("pactl", args...)
	if err != nil {
		return "", fmt.Errorf("pactl %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

func defaultSink(run Runner) (string, error) {
	out, err := pactl(run, "get-default-sink")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func listSinks(run Runner) ([]sink, error) { return listShort(run, "sinks") }

// listSources lists the sources, sinks' monitors among them, as sinks: by id
// and name.
func listSources(run Runner) ([]sink, error) { return listShort(run, "sources") }

func listShort(run Runner, what string) ([]sink, error) {
	out, err := pactl(run, "list", "short", what)
	if err != nil {
		return nil, err
	}
	var sinks []sink
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 2 {
			continue
		}
		id, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		sinks = append(sinks, sink{id: id, name: f[1]})
	}
	return sinks, nil
}

func sinkID(sinks []sink, name string) (int, bool) {
	for _, s := range sinks {
		if s.name == name {
			return s.id, true
		}
	}
	return 0, false
}

// listModules parses `id\tname\targs` lines. PipeWire's own modules print
// multi-line args; a line that does not start with a numeric id continues
// the previous module's args.
func listModules(run Runner) ([]module, error) {
	out, err := pactl(run, "list", "short", "modules")
	if err != nil {
		return nil, err
	}
	var mods []module
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(line, "\t", 3)
		if id, err := strconv.Atoi(f[0]); err == nil && len(f) >= 2 {
			m := module{id: id, name: f[1]}
			if len(f) == 3 {
				m.args = strings.TrimRight(f[2], "\t")
			}
			mods = append(mods, m)
			continue
		}
		if len(mods) > 0 {
			mods[len(mods)-1].args += "\n" + line
		}
	}
	return mods, nil
}

// castrModuleIDs is the set of modules castr loaded.
func castrModuleIDs(mods []module) map[int]bool {
	ids := map[int]bool{}
	for _, m := range mods {
		if isCastrModule(m.args) {
			ids[m.id] = true
		}
	}
	return ids
}

func listSinkInputs(run Runner) ([]sinkInput, error) {
	out, err := pactl(run, "--format=json", "list", "sink-inputs")
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Index int     `json:"index"`
		Sink  int     `json:"sink"`
		Owner *string `json:"owner_module"`
		Mute  bool    `json:"mute"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parsing pactl's sink-inputs: %w", err)
	}
	inputs := make([]sinkInput, 0, len(raw))
	for _, r := range raw {
		inputs = append(inputs, sinkInput{index: r.Index, sink: r.Sink, owner: ownerID(r.Owner), mute: r.Mute})
	}
	return inputs, nil
}

// ownerID reads owner_module: a string id, or null for a stream no module
// owns, -1.
func ownerID(owner *string) int {
	if owner != nil {
		if id, err := strconv.Atoi(*owner); err == nil {
			return id
		}
	}
	return -1
}

// streamOf finds the playback stream of the module with this id: a
// loopback's.
func streamOf(inputs []sinkInput, module int) (sinkInput, bool) {
	for _, in := range inputs {
		if in.owner == module {
			return in, true
		}
	}
	return sinkInput{}, false
}

func listSourceOutputs(run Runner) ([]sourceOutput, error) {
	out, err := pactl(run, "--format=json", "list", "source-outputs")
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Index  int               `json:"index"`
		Source int               `json:"source"`
		Owner  *string           `json:"owner_module"`
		Props  map[string]string `json:"properties"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parsing pactl's source-outputs: %w", err)
	}
	outputs := make([]sourceOutput, 0, len(raw))
	for _, r := range raw {
		outputs = append(outputs, sourceOutput{index: r.Index, source: r.Source, owner: ownerID(r.Owner), target: r.Props["target.object"]})
	}
	return outputs, nil
}

// loadNullSink creates a virtual output. The description is quoted twice
// because pipewire-pulse splits module args, and then the sink properties, on
// spaces: unquoted, "castr cast" arrives as "castr".
func loadNullSink(run Runner, name, description string) (modRef, error) {
	id, err := loadModule(run, nullSinkModule, "sink_name="+name,
		fmt.Sprintf(`sink_properties="device.description='%s'"`, description))
	return modRef{id: id, name: nullSinkModule, sig: []string{"sink_name=" + name}}, err
}

// loadLoopback copies source into sink. Neither end may be moved: when a
// loopback's output vanishes, PipeWire would otherwise relink it to the
// default -- castr_mix -- and feed castr_mix back into itself. Pinned, it
// unloads itself instead, and Follow finds the laptop a new output.
//
// name ("laptop" or "cast_<n>") names its two streams. WirePlumber remembers
// a stream's volume under its media.name, and a loopback's default one,
// loopback-<pid>-<n>, is new on every load: so every loopback to the laptop
// shares one media.name, and every loopback to a receiver another, and
// WirePlumber's state stays at four entries however often the sound is
// switched. WirePlumber is also told not to restore or remember their
// volume and mute (state.restore-props): castr sets both right after the
// load, and a mute it remembered from an earlier cast, restored a moment
// later, would silence this one. The quoting is loadNullSink's.
func loadLoopback(run Runner, source, sink, name string) (modRef, error) {
	media := "castr_loop_laptop"
	if name != "laptop" {
		media = "castr_loop_cast"
	}
	props := func(dir string) string {
		return fmt.Sprintf(`"media.name=%s node.name=castr_loop_%s_%s state.restore-props=false"`, media, name, dir)
	}
	id, err := loadModule(run, loopbackModule, "source="+source, "sink="+sink, "latency_msec=30",
		"sink_dont_move=true", "source_dont_move=true",
		"sink_input_properties="+props("out"), "source_output_properties="+props("in"))
	return modRef{id: id, name: loopbackModule, sig: []string{"source=" + source, "sink=" + sink}}, err
}

func loadModule(run Runner, args ...string) (int, error) {
	out, err := pactl(run, append([]string{"load-module"}, args...)...)
	if err != nil {
		return 0, err
	}
	id, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("pactl load-module %s printed %q, not a module id", args[0], out)
	}
	return id, nil
}

func unloadModule(run Runner, id int) error {
	_, err := pactl(run, "unload-module", strconv.Itoa(id))
	return err
}

// fullAndUnmuted undoes whatever WirePlumber remembers for this sink name: a
// castr sink once left at 0% would otherwise come back at 0%, and its monitor
// would carry silence.
func fullAndUnmuted(run Runner, name string) error {
	if _, err := pactl(run, "set-sink-volume", name, "100%"); err != nil {
		return err
	}
	_, err := pactl(run, "set-sink-mute", name, "0")
	return err
}

// setStreamMute mutes or unmutes one stream: only ever one of castr's own
// loopbacks', never a real output, which may be what a receiver captures.
func setStreamMute(run Runner, index int, mute bool) error {
	v := "0"
	if mute {
		v = "1"
	}
	_, err := pactl(run, "set-sink-input-mute", strconv.Itoa(index), v)
	return err
}

func setStreamFullVolume(run Runner, index int) error {
	_, err := pactl(run, "set-sink-input-volume", strconv.Itoa(index), "100%")
	return err
}

func moveSourceOutput(run Runner, index int, source string) error {
	_, err := pactl(run, "move-source-output", strconv.Itoa(index), source)
	return err
}

func setDefaultSink(run Runner, name string) error {
	_, err := pactl(run, "set-default-sink", name)
	return err
}

func moveSinkInput(run Runner, index int, sink string) error {
	_, err := pactl(run, "move-sink-input", strconv.Itoa(index), sink)
	return err
}

// moveAppsOnto moves the app streams on from onto target. A stream a module
// owns is never an app's and stays put: castr's own loopbacks would feed
// castr_mix back into itself, and another module's -- a combine-sink's, the
// user's own loopback -- could feed back through castr, or into its own sink
// when moved back. Streams on any other output stay where the user put
// them. A stream that ends between the listing and the move is not an error
// worth failing over.
func moveAppsOnto(run Runner, target, from string) error {
	sinks, err := listSinks(run)
	if err != nil {
		return err
	}
	targetID, ok := sinkID(sinks, target)
	if !ok {
		return fmt.Errorf("output %s is gone", target)
	}
	fromID, ok := sinkID(sinks, from)
	if !ok || fromID == targetID {
		return nil // nothing is on an output that has gone
	}
	inputs, err := listSinkInputs(run)
	if err != nil {
		return err
	}
	for _, in := range inputs {
		if in.owner >= 0 || in.sink != fromID {
			continue
		}
		_ = moveSinkInput(run, in.index, target)
	}
	return nil
}

// WirePlumber keeps the output the user chose as default.configured.audio.sink
// in the "default" metadata, and plays through it whenever it exists: it is
// how Bluetooth headphones take over when they connect. pactl
// set-default-sink overwrites it, so castr saves it before taking over and
// puts it back exactly afterwards.
const configuredKey = "default.configured.audio.sink"

// configuredDefault is a saved default.configured.audio.sink.
type configuredDefault struct {
	// Saved is false in a state file from before castr saved it, or when
	// the key held a castr leftover and the stack behind it is unknown;
	// restore then leaves the key alone.
	Saved bool `json:"saved"`
	// Name is "" when the key was absent: restore deletes it.
	Name string `json:"name,omitempty"`
}

func pwMetadata(run Runner, args ...string) (string, error) {
	args = append([]string{"-n", "default"}, args...)
	out, err := run("pw-metadata", args...)
	if err != nil {
		return "", fmt.Errorf("pw-metadata %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// readConfiguredDefault reads the key with `pw-metadata -n default 0 <key>`,
// which prints
//
//	Found "default" metadata 40
//	update: id:0 key:'default.configured.audio.sink' value:'{"name":"<sink>"}' type:'Spa:String:JSON'
//
// and only the first line when the key is absent. With no "default"
// metadata it prints nothing and still exits 0; that is an error here, not
// an absent key, or restore would delete the user's choice.
func readConfiguredDefault(run Runner) (configuredDefault, error) {
	out, err := pwMetadata(run, "0", configuredKey)
	if err != nil {
		return configuredDefault{}, err
	}
	if !strings.Contains(out, `Found "default" metadata`) {
		return configuredDefault{}, errors.New("WirePlumber's default metadata is not there")
	}
	prefix := "update: id:0 key:'" + configuredKey + "' value:'"
	for _, line := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), prefix)
		if !ok {
			continue
		}
		end := strings.LastIndex(rest, "' type:'")
		if end < 0 {
			return configuredDefault{}, fmt.Errorf("pw-metadata printed %q", line)
		}
		var v struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(rest[:end]), &v); err != nil {
			return configuredDefault{}, fmt.Errorf("parsing %s %q: %w", configuredKey, rest[:end], err)
		}
		return configuredDefault{Saved: true, Name: v.Name}, nil
	}
	return configuredDefault{Saved: true}, nil
}

// writeConfiguredDefault puts a saved key back as it was: the same name, or
// no key at all.
func writeConfiguredDefault(run Runner, c configuredDefault) error {
	if !c.Saved {
		return nil
	}
	if c.Name == "" {
		_, err := pwMetadata(run, "-d", "0", configuredKey)
		return err
	}
	value, err := json.Marshal(struct {
		Name string `json:"name"`
	}{c.Name})
	if err != nil {
		return err
	}
	_, err = pwMetadata(run, "0", configuredKey, string(value), "Spa:String:JSON")
	return err
}
