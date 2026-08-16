package daemon

import (
	"sort"
	"sync"
	"time"

	"github.com/mrCode/castr/internal/discovery"
)

// BrowseFunc performs one round of mDNS discovery. Injected: no test browses
// the real network.
type BrowseFunc func() ([]discovery.Device, error)

// BrowseInterval is how often the registry re-browses while the daemon runs.
// avahi answers from a cache that has been warm since boot, so this is about
// noticing receivers that appear or vanish, not about the first answer.
const BrowseInterval = 20 * time.Second

// Registry holds what the daemon knows about receivers on the network.
//
// The distinction that matters is between "we know of no devices" and "we have
// not asked yet". A daemon that has only just started is in the second state,
// and answering `list` from it returns an empty list for a network full of
// receivers -- which is what the cast menu showed, and why `start <id>` said
// "device not found" for an Apple TV sitting right there.
type Registry struct {
	browse BrowseFunc
	now    func() time.Time

	mu        sync.Mutex
	devices   map[string]discovery.Device
	manual    map[string]discovery.Device
	answered  bool
	startedAt time.Time
}

// NewRegistry returns a registry that has not browsed yet.
func NewRegistry(browse BrowseFunc, now func() time.Time) *Registry {
	if now == nil {
		now = time.Now
	}
	return &Registry{
		browse:    browse,
		now:       now,
		devices:   map[string]discovery.Device{},
		manual:    map[string]discovery.Device{},
		startedAt: now(),
	}
}

// Refresh runs one browse and records that discovery has answered.
//
// A browse that FAILS does not count as an answer: treating an avahi error as
// "the network holds no receivers" is how a transient failure turned into a
// confident empty menu.
func (r *Registry) Refresh() error {
	found, err := r.browse()
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.devices = make(map[string]discovery.Device, len(found))
	for _, d := range found {
		r.devices[d.ID] = d
	}
	r.answered = true
	return nil
}

// Answered reports whether a browse has completed at least once.
//
// Keyed on having ASKED, not on knowing something. Waiting for "any device at
// all" instead means a network with genuinely no receivers waits out the whole
// grace period on every single command.
func (r *Registry) Answered() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.answered
}

// Add records a manually configured receiver, which survives every browse.
func (r *Registry) Add(d discovery.Device) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.manual[d.ID] = d
}

// Forget drops a manually configured receiver. Discovered ones come back on
// the next browse, so forgetting those would be a lie.
func (r *Registry) Forget(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.manual[id]
	delete(r.manual, id)
	return ok
}

// Devices returns everything known, discovered and manual, sorted by name so
// the menu does not reshuffle itself between invocations.
func (r *Registry) Devices() []discovery.Device {
	r.mu.Lock()
	defer r.mu.Unlock()

	merged := make(map[string]discovery.Device, len(r.devices)+len(r.manual))
	for id, d := range r.devices {
		merged[id] = d
	}
	// Manual entries win: the user typed that address, and a stale discovered
	// record for the same receiver should not override it.
	for id, d := range r.manual {
		merged[id] = d
	}

	out := make([]discovery.Device, 0, len(merged))
	for _, d := range merged {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Find returns the device with this id, if it is known.
func (r *Registry) Find(id string) (discovery.Device, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if d, ok := r.manual[id]; ok {
		return d, true
	}
	d, ok := r.devices[id]
	return d, ok
}

// Await gives a just-started browser its chance to answer, measured from when
// the registry was created rather than from when the command arrived.
//
// Shared by list and start. It lived only in list at first, so the keybind
// worked -- the menu lists, then starts against a by-then-warm daemon -- while
// `castr start <id>` against a cold daemon failed with "device not found" for
// a receiver that was plainly there.
func (r *Registry) Await(grace time.Duration, poll time.Duration) {
	r.mu.Lock()
	deadline := r.startedAt.Add(grace)
	r.mu.Unlock()

	for !r.Answered() && r.now().Before(deadline) {
		time.Sleep(poll)
	}
}
