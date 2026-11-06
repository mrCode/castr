package sound

import (
	"encoding/json"
	"fmt"
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

type sinkInput struct {
	index, sink int
	owner       int // -1 for a stream no module owns: an app
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

func listSinks(run Runner) ([]sink, error) {
	out, err := pactl(run, "list", "short", "sinks")
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
	// owner_module is a string id, or null for a stream no module owns.
	var raw []struct {
		Index int     `json:"index"`
		Sink  int     `json:"sink"`
		Owner *string `json:"owner_module"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parsing pactl's sink-inputs: %w", err)
	}
	inputs := make([]sinkInput, 0, len(raw))
	for _, r := range raw {
		in := sinkInput{index: r.Index, sink: r.Sink, owner: -1}
		if r.Owner != nil {
			if id, err := strconv.Atoi(*r.Owner); err == nil {
				in.owner = id
			}
		}
		inputs = append(inputs, in)
	}
	return inputs, nil
}

// loadNullSink creates a virtual output. The description is quoted twice
// because pipewire-pulse splits module args, and then the sink properties, on
// spaces: unquoted, "castr cast" arrives as "castr".
func loadNullSink(run Runner, name, description string) (int, error) {
	return loadModule(run, nullSinkModule, "sink_name="+name,
		fmt.Sprintf(`sink_properties="device.description='%s'"`, description))
}

// loadLoopback copies source into sink. Neither end may be moved: when a
// loopback's output vanishes, PipeWire would otherwise relink it to the
// default -- castr_mix -- and feed castr_mix back into itself. Pinned, it
// unloads itself instead, and Follow finds the laptop a new output.
func loadLoopback(run Runner, source, sink string) (int, error) {
	return loadModule(run, loopbackModule, "source="+source, "sink="+sink, "latency_msec=30",
		"sink_dont_move=true", "source_dont_move=true")
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

func setDefaultSink(run Runner, name string) error {
	_, err := pactl(run, "set-default-sink", name)
	return err
}

func moveSinkInput(run Runner, index int, sink string) error {
	_, err := pactl(run, "move-sink-input", strconv.Itoa(index), sink)
	return err
}

// moveAppsOnto moves app streams onto target. Streams castr's own modules own
// -- the loopbacks -- stay put: moving a loopback into castr_mix would feed
// castr_mix back into itself. from limits the move to streams on one sink;
// -1 means every sink. A stream that ends between the listing and the move
// is not an error worth failing over.
func moveAppsOnto(run Runner, target string, from int) error {
	sinks, err := listSinks(run)
	if err != nil {
		return err
	}
	targetID, ok := sinkID(sinks, target)
	if !ok {
		return fmt.Errorf("output %s is gone", target)
	}
	mods, err := listModules(run)
	if err != nil {
		return err
	}
	ours := castrModuleIDs(mods)
	inputs, err := listSinkInputs(run)
	if err != nil {
		return err
	}
	for _, in := range inputs {
		if ours[in.owner] || in.sink == targetID || (from >= 0 && in.sink != from) {
			continue
		}
		_ = moveSinkInput(run, in.index, target)
	}
	return nil
}
