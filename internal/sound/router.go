package sound

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

type cast struct {
	n          int
	sinkModule modRef
	loopModule modRef // castr_mix -> this cast's sink; none when the receiver is silent
	choice     Choice
}

// Router owns castr's sound routing for every cast. Its methods are safe to
// call from several goroutines.
type Router struct {
	mu       sync.Mutex
	run      Runner
	stateDir string

	// StartGuard, when set, starts the process that restores the sound if
	// the daemon dies. It runs once each time the state file is first
	// written, i.e. once per routing lifetime.
	StartGuard func() error

	casts      map[string]*cast
	lastN      int // never reused within a process
	mixModule  modRef
	laptopLoop modRef // castr_mix -> the laptop; none when the laptop is silent
	previous   string // the default before the first cast
	laptop     string // the output the laptop plays through now; "" for none
	sweep      string // an output whose app streams still need moving into castr_mix
}

func NewRouter(run Runner, stateDir string) *Router {
	return &Router{run: run, stateDir: stateDir, casts: map[string]*cast{}}
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
	if err := saveState(r.stateDir, savedState{PreviousDefault: def, LaptopSink: def}); err != nil {
		return err
	}
	r.previous, r.laptop = def, def
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
	return errors.Join(unloadRef(r.run, c.loopModule, mods), unloadRef(r.run, c.sinkModule, mods))
}

// Set changes where one cast's sound plays. It only loads or unloads
// loopbacks: the cast's own sink, which its receiver captures, and the
// default output are never touched, so the picture never restarts.
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

// reconcile makes the loopbacks match the choices: a receiver gets sound when
// its cast is on TV or Both, and the laptop plays when any cast is on Laptop
// or Both. A loopback that has vanished -- its output went, and it unloaded
// itself -- is forgotten first, so it is loaded again where it is wanted and
// its old id, which PipeWire may have handed on, is never unloaded. The
// laptop loopback is loaded only while the laptop's output exists; until
// then Follow finds it another.
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
		if !c.loopModule.in(mods) {
			c.loopModule = modRef{}
		}
	}
	if !r.laptopLoop.in(mods) {
		r.laptopLoop = modRef{}
	}

	var errs []error
	laptop := false
	for _, c := range r.casts {
		laptop = laptop || c.choice.toLaptop()
		switch want := c.choice.toReceiver(); {
		case want && !c.loopModule.loaded():
			ref, err := loadLoopback(r.run, mixSink+".monitor", castSinkName(c.n))
			if err != nil {
				errs = append(errs, err)
				continue
			}
			c.loopModule = ref
		case !want && c.loopModule.loaded():
			if err := unloadModule(r.run, c.loopModule.id); err != nil {
				errs = append(errs, err)
				continue
			}
			c.loopModule = modRef{}
		}
	}
	_, laptopPresent := sinkID(sinks, r.laptop)
	switch {
	case laptop && !r.laptopLoop.loaded() && r.laptop != "" && laptopPresent:
		ref, err := loadLoopback(r.run, mixSink+".monitor", r.laptop)
		if err != nil {
			errs = append(errs, err)
		} else {
			r.laptopLoop = ref
		}
	case !laptop && r.laptopLoop.loaded():
		if err := unloadModule(r.run, r.laptopLoop.id); err != nil {
			errs = append(errs, err)
		} else {
			r.laptopLoop = modRef{}
		}
	}
	return errors.Join(errs...)
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
//   - When the laptop's output vanished (the default stays castr_mix, so
//     nothing else would say), the laptop moves to the previous default, or
//     else to any real output, or else to none.
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
	return r.reconcile() // retries a loopback an earlier failure left out
}

func (r *Router) follow(sinks []sink) error {
	def, err := defaultSink(r.run)
	if err != nil {
		return err
	}
	_, defPresent := sinkID(sinks, def)
	switch {
	case isCastrName(def) && def != mixSink:
		// One of castr's cast outputs is not a laptop output.
		if err := setDefaultSink(r.run, mixSink); err != nil {
			return err
		}
	case def != mixSink && def != "" && defPresent:
		if err := r.adoptLaptop(def); err != nil {
			return err
		}
		if err := setDefaultSink(r.run, mixSink); err != nil {
			return err
		}
		r.sweep = def
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
// restore after a crash goes back to it. The laptop loopback, if wanted, is
// rebuilt there before the old one goes. The old one is unloaded only if it
// is still listed as castr loaded it: when its output vanished it unloaded
// itself, and the new loopback has most likely just been given its id.
func (r *Router) adoptLaptop(name string) error {
	if err := saveState(r.stateDir, savedState{PreviousDefault: r.previous, LaptopSink: name}); err != nil {
		return err
	}
	r.laptop = name
	old := r.laptopLoop
	r.laptopLoop = modRef{}
	err := r.reconcile()
	if !old.loaded() {
		return err
	}
	mods, listErr := listModules(r.run)
	if listErr != nil {
		return errors.Join(err, listErr)
	}
	return errors.Join(err, unloadRef(r.run, old, mods))
}

// Restore undoes all routing and puts the laptop back as it was before the
// first cast.
func (r *Router) Restore() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.restoreLocked()
}

func (r *Router) restoreLocked() error {
	err := restore(r.run, r.stateDir, r.laptop, r.previous)

	r.casts = map[string]*cast{}
	r.mixModule, r.laptopLoop = modRef{}, modRef{}
	r.previous, r.laptop, r.sweep = "", "", ""
	return err
}

// restore undoes castr's routing. It finds castr's modules by listing them,
// never by remembered ids, which PipeWire may have handed to other modules;
// if pactl cannot list them, none is unloaded, and the next Recover sweeps
// them. Every step runs even if an earlier one fails:
//  1. unload the loopbacks, so nothing feeds castr's outputs;
//  2. set the default to laptop, the output the user was last using, or to
//     previous if laptop is gone -- only ever to an output that still exists;
//     with neither left, the session manager's own choice stands;
//  3. move the app streams on castr_mix to the new default;
//  4. unload castr's outputs;
//  5. delete the state file.
func restore(run Runner, stateDir, laptop, previous string) error {
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
	target := ""
	for _, want := range []string{laptop, previous} {
		if _, ok := sinkID(present, want); !ok || want == "" || isCastrName(want) {
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
	// With no real default to move to, unloading castr_mix below hands its
	// streams to the session manager's fallback.
	if _, ok := sinkID(present, mixSink); ok && target != "" {
		errs = append(errs, moveAppsOnto(run, target, mixSink))
	}

	for _, id := range sinks {
		errs = append(errs, unloadModule(run, id))
	}
	errs = append(errs, clearState(stateDir))
	return errors.Join(errs...)
}
