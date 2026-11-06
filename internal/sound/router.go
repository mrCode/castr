package sound

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

type cast struct {
	n          int
	sinkModule int
	loopModule int // castr_mix -> this cast's sink; 0 when the receiver is silent
	choice     Choice
}

// Router owns castr's sound routing for every cast. Its methods are safe to
// call from several goroutines.
type Router struct {
	mu       sync.Mutex
	run      Runner
	stateDir string

	casts      map[string]*cast
	lastN      int // never reused within a process
	mixModule  int
	laptopLoop int    // castr_mix -> the laptop; 0 when the laptop is silent
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
	}
	if err := moveAppsOnto(r.run, mixSink, -1); err != nil {
		return "", r.failAttach(first, err)
	}

	r.lastN++
	c := &cast{n: r.lastN, choice: TV}
	name := castSinkName(c.n)
	id, err := loadNullSink(r.run, name, castDescription)
	if err != nil {
		return "", r.failAttach(first, err)
	}
	c.sinkModule = id
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

	id, err := loadNullSink(r.run, mixSink, mixDescription)
	if err != nil {
		return err
	}
	r.mixModule = id
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

// dropCast unloads one cast's loopback and sink and forgets it.
func (r *Router) dropCast(castID string) error {
	c, ok := r.casts[castID]
	if !ok {
		return nil
	}
	delete(r.casts, castID)
	var errs []error
	if c.loopModule != 0 {
		errs = append(errs, unloadModule(r.run, c.loopModule))
	}
	errs = append(errs, unloadModule(r.run, c.sinkModule))
	return errors.Join(errs...)
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
// or Both.
func (r *Router) reconcile() error {
	var errs []error
	laptop := false
	for _, c := range r.casts {
		laptop = laptop || c.choice.toLaptop()
		switch want := c.choice.toReceiver(); {
		case want && c.loopModule == 0:
			id, err := loadLoopback(r.run, mixSink+".monitor", castSinkName(c.n))
			if err != nil {
				errs = append(errs, err)
				continue
			}
			c.loopModule = id
		case !want && c.loopModule != 0:
			if err := unloadModule(r.run, c.loopModule); err != nil {
				errs = append(errs, err)
				continue
			}
			c.loopModule = 0
		}
	}
	switch {
	case laptop && r.laptopLoop == 0 && r.laptop != "":
		id, err := loadLoopback(r.run, mixSink+".monitor", r.laptop)
		if err != nil {
			errs = append(errs, err)
		} else {
			r.laptopLoop = id
		}
	case !laptop && r.laptopLoop != 0:
		if err := unloadModule(r.run, r.laptopLoop); err != nil {
			errs = append(errs, err)
		} else {
			r.laptopLoop = 0
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
	id, ok := sinkID(sinks, r.sweep)
	if !ok {
		r.sweep = ""
		return nil
	}
	if err := moveAppsOnto(r.run, mixSink, id); err != nil {
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
// rebuilt there before the old one goes.
func (r *Router) adoptLaptop(name string) error {
	if err := saveState(r.stateDir, savedState{PreviousDefault: r.previous, LaptopSink: name}); err != nil {
		return err
	}
	r.laptop = name
	old := r.laptopLoop
	r.laptopLoop = 0
	err := r.reconcile()
	if old != 0 {
		// The old output usually vanished and took its loopback with it, so a
		// failure here is expected; Restore sweeps any leftover by name.
		_ = unloadModule(r.run, old)
	}
	return err
}

// Restore undoes all routing and puts the laptop back as it was before the
// first cast.
func (r *Router) Restore() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.restoreLocked()
}

func (r *Router) restoreLocked() error {
	var known []module
	for _, c := range r.casts {
		if c.loopModule != 0 {
			known = append(known, module{id: c.loopModule, name: loopbackModule})
		}
		known = append(known, module{id: c.sinkModule, name: nullSinkModule})
	}
	if r.laptopLoop != 0 {
		known = append(known, module{id: r.laptopLoop, name: loopbackModule})
	}
	if r.mixModule != 0 {
		known = append(known, module{id: r.mixModule, name: nullSinkModule})
	}
	err := restore(r.run, r.stateDir, r.laptop, r.previous, known)

	r.casts = map[string]*cast{}
	r.mixModule, r.laptopLoop = 0, 0
	r.previous, r.laptop, r.sweep = "", "", ""
	return err
}

// restore undoes castr's routing, from the modules pactl lists or, when it
// cannot list them, the ones the caller knows it loaded. Every step runs even
// if an earlier one fails:
//  1. unload the loopbacks, so nothing feeds castr's outputs;
//  2. set the default to laptop, the output the user was last using, or to
//     previous if laptop is gone -- only ever to an output that still exists;
//     with neither left, the session manager's own choice stands;
//  3. move the streams on castr_mix to the new default;
//  4. unload castr's outputs;
//  5. delete the state file.
func restore(run Runner, stateDir, laptop, previous string, known []module) error {
	var errs []error
	mods, listErr := listModules(run)
	if listErr != nil {
		errs = append(errs, listErr)
		mods = known
	}
	var loops, sinks []int
	for _, m := range mods {
		if listErr == nil && !isCastrModule(m.args) {
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
	if mixID, ok := sinkID(present, mixSink); ok && target != "" {
		errs = append(errs, moveAppsOnto(run, target, mixID))
	}

	for _, id := range sinks {
		errs = append(errs, unloadModule(run, id))
	}
	errs = append(errs, clearState(stateDir))
	return errors.Join(errs...)
}
