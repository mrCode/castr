package sound

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

type cast struct {
	n          int
	sinkModule modRef
	loop       loopback // castr_mix -> this cast's sink, muted while the receiver is silent
	choice     Choice
}

// loopback is one of castr's loopbacks out of castr_mix. Each is loaded once
// and a switch only mutes or unmutes its stream: every load or unload
// re-links PipeWire's graph, and each re-link added buffering to the path
// into a receiver's capture that was never drained, until the TV's sound
// turned robotic, then silent. fresh says its stream still needs its volume
// and mute set, as castr does right after loading it.
type loopback struct {
	modRef
	fresh bool
}

// Router owns castr's sound routing for every cast. Its methods are safe to
// call from several goroutines.
type Router struct {
	mu       sync.Mutex
	run      Runner
	stateDir string
	pid      int // written into the state file, for the guard

	// StartGuard, when set, starts the process that restores the sound if
	// the daemon dies. It runs once each time the state file is first
	// written, i.e. once per routing lifetime.
	StartGuard func() error
	// HistoryFile is WirePlumber's state file with its stack of chosen
	// outputs (WirePlumberHistoryFile). It is only ever read. "" or a file
	// that cannot be read leaves the stack unknown.
	HistoryFile string

	casts      map[string]*cast
	lastN      int // never reused within a process
	mixModule  modRef
	laptopLoop loopback // castr_mix -> the laptop, muted while the laptop is silent; none with no laptop output
	previous   string   // the default before the first cast
	laptop     string   // the output the laptop plays through now; "" for none
	sweep      string   // an output whose app streams still need moving into castr_mix
	// configured is WirePlumber's configured default as it would be without
	// castr; configuredSeen says whether that output was there at the last
	// look, so Follow can tell when it appears.
	configured     configuredDefault
	configuredSeen bool
	history        []string // WirePlumber's stack without castr, top first; nil when unknown
}

func NewRouter(run Runner, stateDir string) *Router {
	return &Router{run: run, stateDir: stateDir, pid: os.Getpid(), casts: map[string]*cast{}}
}

// Attach routes a new cast and returns the source its receiver captures. The
// cast starts on TV. The first attach takes over the default output.
func (r *Router) Attach(castID string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.casts[castID]; ok {
		return "", fmt.Errorf("sound for %s is already routed", castID)
	}
	first := len(r.casts) == 0
	if first {
		// A restore that failed left its state file. Finish it first: the
		// default it left may be castr_mix, which is no default to go back to.
		if err := r.retryRestore(); err != nil {
			return "", fmt.Errorf("the sound from an earlier cast is not restored yet: %w", err)
		}
		if err := r.takeOver(); err != nil {
			return "", errors.Join(err, r.restoreLocked())
		}
		// Only what plays on the default: a stream the user put on another
		// output stays there, and is not sent to the TV.
		if err := moveAppsOnto(r.run, mixSink, r.previous); err != nil {
			return "", r.failAttach(first, err)
		}
	}

	r.lastN++
	c := &cast{n: r.lastN, choice: TV}
	name := castSinkName(c.n)
	ref, err := loadNullSink(r.run, name, castDescription)
	if err != nil {
		return "", r.failAttach(first, err)
	}
	c.sinkModule = ref
	r.casts[castID] = c
	if err := fullAndUnmuted(r.run, name); err != nil {
		return "", r.failAttach(first, err, r.dropCast(castID))
	}
	if err := r.reconcile(); err != nil {
		return "", r.failAttach(first, err, r.dropCast(castID))
	}
	return name + ".monitor", nil
}

// takeOver records the current default, then makes castr_mix the default so
// every laptop sound lands there. The state file is written before anything
// changes.
func (r *Router) takeOver() error {
	def, err := defaultSink(r.run)
	if err != nil {
		return fmt.Errorf("finding the default output: %w", err)
	}
	if def == "" {
		return errors.New("there is no default output")
	}
	if isCastrName(def) {
		// Saving it would make Restore put castr's own output back as the
		// default. Recover runs at startup, so this means it failed.
		return fmt.Errorf("the default output is castr's own %s, left from an earlier cast", def)
	}
	configured, err := readConfiguredDefault(r.run)
	if err != nil {
		return fmt.Errorf("reading the configured default output: %w", err)
	}
	history := r.readHistory(configured)
	switch {
	case history != nil:
		// Its top, with castr's own outputs dropped.
		configured.Name = ""
		if len(history) > 0 {
			configured.Name = history[0]
		}
	case isCastrName(configured.Name):
		// Left by an earlier castr: no choice of the user's to put back. But
		// with the stack unknown, castr cannot see what lies behind it, and
		// deleting the key would erase it all: the key is left alone.
		configured = configuredDefault{}
	}
	sinks, err := listSinks(r.run)
	if err != nil {
		return err
	}
	r.previous, r.configured, r.history = def, configured, history
	_, r.configuredSeen = sinkID(sinks, configured.Name)
	if err := r.save(def); err != nil {
		r.previous, r.configured, r.history = "", configuredDefault{}, nil
		return err
	}
	r.laptop = def
	if r.StartGuard != nil {
		if err := r.StartGuard(); err != nil {
			return err
		}
	}

	ref, err := loadNullSink(r.run, mixSink, mixDescription)
	if err != nil {
		return err
	}
	r.mixModule = ref
	if err := fullAndUnmuted(r.run, mixSink); err != nil {
		return err
	}
	return setDefaultSink(r.run, mixSink)
}

// readHistory reads WirePlumber's stack of chosen outputs, made to agree
// with the configured default just read; nil when it cannot be read.
func (r *Router) readHistory(configured configuredDefault) []string {
	if r.HistoryFile == "" {
		return nil
	}
	history, err := readSinkHistory(r.HistoryFile)
	if err != nil {
		return nil
	}
	return historyAsConfigured(history, configured)
}

// failAttach undoes a failed attach: everything when it was the first, or
// just the new cast otherwise. The errors are joined onto err.
func (r *Router) failAttach(first bool, err error, more ...error) error {
	errs := append([]error{err}, more...)
	if first {
		errs = append(errs, r.restoreLocked())
	}
	return errors.Join(errs...)
}

// dropCast unloads one cast's loopback and sink and forgets it. If pactl
// cannot list the modules to confirm they are still castr's, they stay for
// Restore or Recover, which find castr's modules by name.
func (r *Router) dropCast(castID string) error {
	c, ok := r.casts[castID]
	if !ok {
		return nil
	}
	delete(r.casts, castID)
	mods, err := listModules(r.run)
	if err != nil {
		return err
	}
	return errors.Join(unloadRef(r.run, c.loop.modRef, mods), unloadRef(r.run, c.sinkModule, mods))
}

// Set changes where one cast's sound plays. It only mutes or unmutes the
// streams of castr's loopbacks: no loopback is loaded or unloaded, so the
// graph is never re-linked, and the cast's own sink, which its receiver
// captures, and the default output are never touched, so the picture never
// restarts.
func (r *Router) Set(castID string, choice Choice) error {
	choice, err := ParseChoice(string(choice))
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	c, ok := r.casts[castID]
	if !ok {
		return fmt.Errorf("sound for %s is not routed", castID)
	}
	old := c.choice
	c.choice = choice
	if err := r.reconcile(); err != nil {
		c.choice = old
		return err
	}
	return nil
}

// Choice reports a cast's current choice, and false if it is not routed.
func (r *Router) Choice(castID string) (Choice, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.casts[castID]
	if !ok {
		return "", false
	}
	return c.choice, true
}

// reconcile makes the loopbacks match the choices. Every cast has its
// loopback, and the laptop has one while its output exists; their streams
// are muted where the sound should not play: a receiver gets sound when its
// cast is on TV or Both, and the laptop plays when any cast is on Laptop or
// Both. A loopback that has vanished -- its output went, and it unloaded
// itself -- is forgotten first, so it is loaded again and its old id, which
// PipeWire may have handed on, is never unloaded. Until the laptop's output
// exists, Follow finds it another.
func (r *Router) reconcile() error {
	mods, err := listModules(r.run)
	if err != nil {
		return err
	}
	sinks, err := listSinks(r.run)
	if err != nil {
		return err
	}
	for _, c := range r.casts {
		if !c.loop.in(mods) {
			c.loop = loopback{}
		}
	}
	if !r.laptopLoop.in(mods) {
		r.laptopLoop = loopback{}
	}

	var errs []error
	for _, c := range r.casts {
		if c.loop.loaded() {
			continue
		}
		ref, err := loadLoopback(r.run, mixSink+".monitor", castSinkName(c.n), "cast_"+strconv.Itoa(c.n))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		c.loop = loopback{modRef: ref, fresh: true}
	}
	// A laptop loopback into an output that went has unloaded itself, and
	// adoptLaptop unloads the one into an output the laptop left.
	if _, present := sinkID(sinks, r.laptop); present && r.laptop != "" && !r.laptopLoop.loaded() {
		ref, err := loadLoopback(r.run, mixSink+".monitor", r.laptop, "laptop")
		if err != nil {
			errs = append(errs, err)
		} else {
			r.laptopLoop = loopback{modRef: ref, fresh: true}
		}
	}
	return errors.Join(append(errs, r.applyMutes())...)
}

// applyMutes mutes each loopback's stream where the sound should not play,
// and unmutes it where it should. A fresh one is also set to 100%: what
// WirePlumber may remember for it is castr's, never the user's. Any other
// stream is left alone, and a real output never muted: a receiver may be
// capturing it. A stream pactl does not list yet is set on the next call.
func (r *Router) applyMutes() error {
	inputs, err := listSinkInputs(r.run)
	if err != nil {
		return err
	}
	var errs []error
	laptop := false
	for _, c := range r.casts {
		laptop = laptop || c.choice.toLaptop()
		errs = append(errs, r.applyMute(inputs, &c.loop, !c.choice.toReceiver()))
	}
	errs = append(errs, r.applyMute(inputs, &r.laptopLoop, !laptop))
	return errors.Join(errs...)
}

func (r *Router) applyMute(inputs []sinkInput, l *loopback, mute bool) error {
	if !l.loaded() {
		return nil
	}
	in, ok := streamOf(inputs, l.id)
	if !ok {
		return nil
	}
	if l.fresh {
		if err := setStreamFullVolume(r.run, in.index); err != nil {
			return err
		}
	}
	if l.fresh || in.mute != mute {
		if err := setStreamMute(r.run, in.index, mute); err != nil {
			return err
		}
	}
	l.fresh = false
	return nil
}

// keepCaptures moves a receiver's capture back onto its cast's output when
// something moved it: once it was found on castr_mix.monitor, and the TV got
// every laptop sound whatever the choice. A cast's capture -- the helper's,
// or castr's own for a Chromecast -- is the stream that targets its output
// (target.object); a stream a module owns is never moved.
func (r *Router) keepCaptures() error {
	sources, err := listSources(r.run)
	if err != nil {
		return err
	}
	outputs, err := listSourceOutputs(r.run)
	if err != nil {
		return err
	}
	var errs []error
	for _, c := range r.casts {
		name := castSinkName(c.n)
		monitor := name + ".monitor"
		id, ok := sinkID(sources, monitor)
		if !ok {
			continue
		}
		for _, o := range outputs {
			if o.owner >= 0 || (o.target != name && o.target != monitor) || o.source == id || !listed(sources, o.source) {
				continue
			}
			errs = append(errs, moveSourceOutput(r.run, o.index, monitor))
		}
	}
	return errors.Join(errs...)
}

// listed reports whether a source with this id is listed: a stream not yet
// linked to one is left for WirePlumber to link by its target.
func listed(sources []sink, id int) bool {
	for _, s := range sources {
		if s.id == id {
			return true
		}
	}
	return false
}

// Detach ends one cast's routing. The last detach restores the laptop. A cast
// that is not routed -- for example after Follow gave up -- is a no-op.
func (r *Router) Detach(castID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.casts[castID]; !ok {
		return nil
	}
	err := r.dropCast(castID)
	if len(r.casts) == 0 {
		return errors.Join(err, r.restoreLocked())
	}
	return errors.Join(err, r.reconcile())
}

// Follow keeps the routing right while the user changes outputs. The daemon
// calls it every second while anything is attached.
//   - When the default is no longer castr_mix, the user picked a new output:
//     it becomes the laptop, the laptop loopback moves there, castr_mix is
//     made the default again, and the app streams on the new output are
//     moved into castr_mix.
//   - When WirePlumber's configured default output appears -- Bluetooth
//     headphones connect -- it becomes the laptop, as WirePlumber would have
//     switched to it had castr_mix not been its configured default.
//   - When the laptop's output vanished (the default stays castr_mix, so
//     nothing else would say), the laptop moves to the previous default, or
//     else to any real output, or else to none.
//   - Every loopback stream muted or unmuted from outside -- in pavucontrol,
//     or by a mute WirePlumber remembered -- is set back as the choices say.
//   - A cast's capture moved off its cast's output is moved back.
//
// Only a castr_mix confirmed missing ends the routing: Follow then restores
// and returns an error saying the routing was lost, and every cast has lost
// its choice. Any other failure is returned as it is, with the routing kept,
// and the next call retries.
func (r *Router) Follow() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.casts) == 0 {
		return nil
	}
	sinks, err := listSinks(r.run)
	if err != nil {
		return err
	}
	if _, ok := sinkID(sinks, mixSink); !ok {
		return errors.Join(fmt.Errorf("the sound routing was lost: %s is gone", mixSink), r.restoreLocked())
	}
	if err := r.follow(sinks); err != nil {
		return err
	}
	// reconcile retries a loopback an earlier failure left out, and puts
	// back a mute changed from outside.
	return errors.Join(r.reconcile(), r.keepCaptures())
}

func (r *Router) follow(sinks []sink) error {
	def, err := defaultSink(r.run)
	if err != nil {
		return err
	}
	_, defPresent := sinkID(sinks, def)
	switch {
	case isCastrName(def) && def != mixSink:
		// One of castr's cast outputs is not a laptop output. The app
		// streams that followed the default there come back too; the cast's
		// own loopback, a module's stream, stays.
		if err := setDefaultSink(r.run, mixSink); err != nil {
			return err
		}
		r.sweep = def
	case def != mixSink && def != "" && defPresent:
		// The user picked it, which made it WirePlumber's configured
		// default; the restore keeps that.
		r.configured = configuredDefault{Saved: true, Name: def}
		r.configuredSeen = true
		if r.history != nil {
			r.history = moveToTop(r.history, def) // as WirePlumber did
		}
		if err := r.adoptLaptop(def); err != nil {
			return err
		}
		if err := setDefaultSink(r.run, mixSink); err != nil {
			return err
		}
		r.sweep = def
	case r.configuredAppeared(sinks):
		// What WirePlumber would switch to -- Bluetooth headphones that
		// connect mid-cast -- but castr_mix is its configured default now.
		if err := r.adoptLaptop(r.configured.Name); err != nil {
			return err
		}
	default:
		if _, ok := sinkID(sinks, r.laptop); !ok || r.laptop == "" {
			if next := laptopFallback(sinks, r.previous); next != r.laptop {
				if err := r.adoptLaptop(next); err != nil {
					return err
				}
			}
		}
	}
	if r.sweep == "" {
		return nil
	}
	// Kept until it succeeds, so a failed move is retried on the next call.
	if _, ok := sinkID(sinks, r.sweep); !ok {
		r.sweep = ""
		return nil
	}
	if err := moveAppsOnto(r.run, mixSink, r.sweep); err != nil {
		return err
	}
	r.sweep = ""
	return nil
}

// configuredAppeared reports whether the configured default output has just
// appeared, and is not the laptop already. It also notes whether it is there
// now, for the next call.
func (r *Router) configuredAppeared(sinks []sink) bool {
	name := r.configured.Name
	_, present := sinkID(sinks, name)
	appeared := present && !r.configuredSeen
	r.configuredSeen = present
	return appeared && name != "" && name != r.laptop && !isCastrName(name)
}

// laptopFallback picks the laptop's output once its own is gone: the
// default from before the first cast if it is still there, or else the first
// real output, or else none.
func laptopFallback(sinks []sink, previous string) string {
	if _, ok := sinkID(sinks, previous); ok && previous != "" && !isCastrName(previous) {
		return previous
	}
	for _, s := range sinks {
		if !strings.HasPrefix(s.name, "castr_") {
			return s.name
		}
	}
	return ""
}

// adoptLaptop makes name the laptop's output, recording it first so that a
// restore after a crash goes back to it. The laptop loopback is rebuilt
// there, muted as the choices say, before the old one goes; the casts'
// loopbacks are left as they are. The old one is unloaded only if it
// is still listed as castr loaded it: when its output vanished it unloaded
// itself, and the new loopback has most likely just been given its id.
func (r *Router) adoptLaptop(name string) error {
	if err := r.save(name); err != nil {
		return err
	}
	r.laptop = name
	old := r.laptopLoop
	r.laptopLoop = loopback{}
	err := r.reconcile()
	if !old.loaded() {
		return err
	}
	mods, listErr := listModules(r.run)
	if listErr != nil {
		return errors.Join(err, listErr)
	}
	return errors.Join(err, unloadRef(r.run, old.modRef, mods))
}

// save writes the state file for the routing as it is, with laptop as the
// laptop's output.
func (r *Router) save(laptop string) error {
	return saveState(r.stateDir, r.state(laptop))
}

func (r *Router) state(laptop string) savedState {
	return savedState{
		PreviousDefault: r.previous, LaptopSink: laptop, DaemonPID: r.pid,
		Configured: r.configured, SinkHistory: r.history,
	}
}

// RetryRestore finishes a restore that failed, while no cast is routed: the
// state file it kept says what to go back to. With nothing pending it does
// nothing. The daemon calls it every second.
func (r *Router) RetryRestore() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.casts) > 0 {
		return nil
	}
	return r.retryRestore()
}

func (r *Router) retryRestore() error {
	if _, pending, _ := loadState(r.stateDir); !pending {
		return nil
	}
	return Recover(r.run, r.stateDir)
}

// Restore undoes all routing and puts the laptop back as it was before the
// first cast. With no cast routed it finishes a restore that failed, from
// its state file: the router forgot what to put back when that restore
// ended, and restoring from memory would delete the file with the
// configured default never written back. It also sweeps any castr modules
// left with no state file.
func (r *Router) Restore() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.casts) == 0 {
		return Recover(r.run, r.stateDir)
	}
	return r.restoreLocked()
}

func (r *Router) restoreLocked() error {
	err := restore(r.run, r.stateDir, r.state(r.laptop))

	r.casts = map[string]*cast{}
	r.mixModule, r.laptopLoop = modRef{}, loopback{}
	r.previous, r.laptop, r.sweep = "", "", ""
	r.configured, r.configuredSeen, r.history = configuredDefault{}, false, nil
	return err
}

// restore undoes castr's routing. It finds castr's modules by listing them,
// never by remembered ids, which PipeWire may have handed to other modules;
// if pactl cannot list them, none is unloaded, and the next Recover sweeps
// them. Every step runs even if an earlier one fails:
//  1. unload the loopbacks, so nothing feeds castr's outputs;
//  2. with WirePlumber's stack of chosen outputs known, move the app streams
//     on castr_mix to the output WirePlumber will pick from it, then rebuild
//     it exactly (writeSinkHistory), as it would be without castr;
//  3. without it, set the default to the laptop, the output the user was
//     last using, or to previous if that is gone -- only ever to an output
//     that still exists; with neither left, the session manager's own
//     choice stands -- and move the app streams on castr_mix there;
//  4. unload castr's outputs;
//  5. without the stack, put WirePlumber's configured default back exactly,
//     which step 2 overwrote: the same output, or no key at all;
//  6. delete the state file -- only if every step worked, so that a failed
//     restore is retried: by the daemon, by the guard, or by the next castrd.
func restore(run Runner, stateDir string, s savedState) error {
	var errs []error
	mods, err := listModules(run)
	errs = append(errs, err)
	var loops, sinks []int
	for _, m := range mods {
		if !isCastrModule(m.args) {
			continue
		}
		if m.name == loopbackModule {
			loops = append(loops, m.id)
		} else {
			sinks = append(sinks, m.id)
		}
	}

	for _, id := range loops {
		errs = append(errs, unloadModule(run, id))
	}

	present, err := listSinks(run)
	errs = append(errs, err)
	usable := func(name string) bool {
		_, ok := sinkID(present, name)
		return ok && name != "" && !isCastrName(name)
	}
	// With no real default to move to, unloading castr_mix below hands its
	// streams to the session manager's fallback.
	moveApps := func(target string) {
		if _, ok := sinkID(present, mixSink); ok && target != "" {
			errs = append(errs, moveAppsOnto(run, target, mixSink))
		}
	}
	stack := s.Configured.Saved && s.SinkHistory != nil
	if stack {
		// WirePlumber will pick the first output in the stack that exists:
		// its place in the stack outranks any priority. castr works that
		// out rather than ask: WirePlumber handles each write after
		// pw-metadata returns, so get-default-sink may still name an output
		// an earlier write made the default for a moment. The apps move
		// first, so they do not follow those moments around either.
		target := ""
		for _, want := range s.SinkHistory {
			if usable(want) {
				target = want
				break
			}
		}
		if target == "" {
			if def, err := defaultSink(run); err == nil && usable(def) {
				target = def
			}
		}
		for _, want := range []string{s.LaptopSink, s.PreviousDefault} {
			if target == "" && usable(want) {
				target = want
			}
		}
		moveApps(target)
		errs = append(errs, writeSinkHistory(run, s.SinkHistory))
	} else {
		target := ""
		for _, want := range []string{s.LaptopSink, s.PreviousDefault} {
			if !usable(want) {
				continue
			}
			if err := setDefaultSink(run, want); err != nil {
				errs = append(errs, err)
				continue
			}
			target = want
			break
		}
		if target == "" {
			if def, err := defaultSink(run); err == nil && def != "" && !isCastrName(def) {
				target = def
			}
		}
		moveApps(target)
	}

	for _, id := range sinks {
		errs = append(errs, unloadModule(run, id))
	}
	if !stack {
		errs = append(errs, writeConfiguredDefault(run, s.Configured))
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	return clearState(stateDir)
}
