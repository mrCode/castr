package daemon

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mrCode/castr/internal/discovery"
)

func dev(id, name string) discovery.Device {
	return discovery.Device{ID: id, Name: name, Address: "10.0.0.5", Port: 7000,
		Protocol: discovery.ProtocolAirPlay}
}

// fakeBrowser models a browser that answers slowly, the way mDNS really does.
type fakeBrowser struct {
	mu      sync.Mutex
	found   []discovery.Device
	err     error
	calls   int
	release chan struct{}
}

func (b *fakeBrowser) browse() ([]discovery.Device, error) {
	b.mu.Lock()
	release := b.release
	b.mu.Unlock()
	if release != nil {
		<-release
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	return append([]discovery.Device{}, b.found...), b.err
}

func TestAFreshRegistryHasNotAnsweredYet(t *testing.T) {
	// The whole point: "we know of nothing" and "we have not asked" are
	// different states, and only the second one is worth waiting on.
	r := NewRegistry((&fakeBrowser{}).browse, nil)

	if r.Answered() {
		t.Error("a registry that has never browsed claims discovery answered")
	}
	if len(r.Devices()) != 0 {
		t.Error("a registry that has never browsed knows devices")
	}
}

func TestAnEmptyNetworkStillCountsAsAnswered(t *testing.T) {
	// Otherwise a network with genuinely no receivers waits out the entire
	// grace period on every single command.
	r := NewRegistry((&fakeBrowser{}).browse, nil)

	if err := r.Refresh(); err != nil {
		t.Fatal(err)
	}

	if !r.Answered() {
		t.Error("an empty but successful browse did not count as an answer")
	}
}

func TestAFailedBrowseIsNotAnAnswer(t *testing.T) {
	// Treating an avahi error as "the network holds no receivers" turns a
	// transient failure into a confident empty menu.
	b := &fakeBrowser{err: errors.New("avahi-daemon is not running")}
	r := NewRegistry(b.browse, nil)

	if err := r.Refresh(); err == nil {
		t.Fatal("Refresh hid the browse error")
	}

	if r.Answered() {
		t.Error("a failed browse counted as discovery having answered")
	}
}

func TestAwaitReturnsAsSoonAsDiscoveryAnswers(t *testing.T) {
	b := &fakeBrowser{found: []discovery.Device{dev("a", "TV")}, release: make(chan struct{})}
	r := NewRegistry(b.browse, time.Now)
	go func() { _ = r.Refresh() }()

	go func() {
		time.Sleep(100 * time.Millisecond)
		close(b.release)
	}()

	start := time.Now()
	r.Await(5*time.Second, 5*time.Millisecond)

	if !r.Answered() {
		t.Fatal("Await returned before discovery answered")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("waited %v after the answer arrived; it should return promptly", elapsed)
	}
}

func TestAwaitGivesUpAfterTheGraceRatherThanBlockingForever(t *testing.T) {
	b := &fakeBrowser{release: make(chan struct{})} // never released
	r := NewRegistry(b.browse, time.Now)
	go func() { _ = r.Refresh() }()

	if !awaitReturns(r, 150*time.Millisecond, 2*time.Second) {
		t.Error("Await blocked forever with no answer coming")
	}
	if r.Answered() {
		t.Error("the test browser answered; it was supposed to hang")
	}
}

// awaitReturns reports whether Await finished within limit, rather than letting
// a stuck Await hang the whole package until the test binary times out.
func awaitReturns(r *Registry, grace, limit time.Duration) bool {
	done := make(chan struct{})
	go func() {
		r.Await(grace, 5*time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(limit):
		return false
	}
}

func TestTheGraceIsMeasuredFromDaemonStartNotFromTheCommand(t *testing.T) {
	// A daemon that has been up for ten minutes must answer `list` instantly.
	// Measuring the grace from when the command arrived made every command
	// wait, forever, on a daemon whose discovery had failed once.
	b := &fakeBrowser{release: make(chan struct{})} // never answers
	started := time.Now()
	// Frozen at start, then jumped forward: the registry was created ten
	// minutes ago and the command arrives now.
	var clockMu sync.Mutex
	elapsed := time.Duration(0)
	r := NewRegistry(b.browse, func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return started.Add(elapsed)
	})
	clockMu.Lock()
	elapsed = 10 * time.Minute
	clockMu.Unlock()
	go func() { _ = r.Refresh() }()

	if !awaitReturns(r, 12*time.Second, time.Second) {
		t.Error("a long-running daemon waited on a grace that expired minutes ago")
	}
}

func TestManualDevicesSurviveABrowseThatDoesNotSeeThem(t *testing.T) {
	// The user added it precisely BECAUSE mDNS does not find it -- a receiver
	// on another subnet, which is a configuration that works.
	b := &fakeBrowser{found: []discovery.Device{dev("a", "Discovered")}}
	r := NewRegistry(b.browse, nil)
	r.Add(dev("m", "Meeting Room"))

	if err := r.Refresh(); err != nil {
		t.Fatal(err)
	}

	if _, ok := r.Find("m"); !ok {
		t.Error("a browse erased the manually added receiver")
	}
	if len(r.Devices()) != 2 {
		t.Errorf("devices = %d, want the discovered one and the manual one", len(r.Devices()))
	}
}

func TestAManualEntryOverridesAStaleDiscoveredOne(t *testing.T) {
	b := &fakeBrowser{found: []discovery.Device{
		{ID: "a", Name: "TV", Address: "10.0.0.9", Port: 7000, Protocol: discovery.ProtocolAirPlay},
	}}
	r := NewRegistry(b.browse, nil)
	if err := r.Refresh(); err != nil {
		t.Fatal(err)
	}
	r.Add(discovery.Device{ID: "a", Name: "TV", Address: "172.26.1.4", Port: 7000,
		Protocol: discovery.ProtocolAirPlay})

	got, ok := r.Find("a")

	if !ok || got.Address != "172.26.1.4" {
		t.Errorf("address = %q, want the address the user typed", got.Address)
	}
}

func TestForgettingOnlyAppliesToManualEntries(t *testing.T) {
	// A discovered receiver comes back on the next browse, so reporting it
	// forgotten would be a lie.
	b := &fakeBrowser{found: []discovery.Device{dev("a", "TV")}}
	r := NewRegistry(b.browse, nil)
	if err := r.Refresh(); err != nil {
		t.Fatal(err)
	}
	r.Add(dev("m", "Manual"))

	if !r.Forget("m") {
		t.Error("forgetting a manual entry reported nothing to forget")
	}
	if r.Forget("a") {
		t.Error("claimed to forget a discovered receiver, which will return")
	}
}

func TestARefreshReplacesRatherThanAccumulates(t *testing.T) {
	// Receivers that go away must disappear from the menu. Merging every
	// browse into the last one left dead entries there until the daemon exited.
	b := &fakeBrowser{found: []discovery.Device{dev("a", "One"), dev("b", "Two")}}
	r := NewRegistry(b.browse, nil)
	if err := r.Refresh(); err != nil {
		t.Fatal(err)
	}

	b.mu.Lock()
	b.found = []discovery.Device{dev("a", "One")}
	b.mu.Unlock()
	if err := r.Refresh(); err != nil {
		t.Fatal(err)
	}

	if _, ok := r.Find("b"); ok {
		t.Error("a receiver that vanished from the network is still listed")
	}
}

func TestDevicesAreOrderedStably(t *testing.T) {
	// Go map iteration is randomised, so an unsorted list reshuffles the cast
	// menu between invocations and the user picks the wrong receiver.
	b := &fakeBrowser{found: []discovery.Device{dev("c", "Zulu"), dev("a", "Alpha"), dev("b", "Mike")}}
	r := NewRegistry(b.browse, nil)
	if err := r.Refresh(); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 20; i++ {
		got := r.Devices()
		if got[0].Name != "Alpha" || got[1].Name != "Mike" || got[2].Name != "Zulu" {
			t.Fatalf("order = %v, want it sorted by name every time", got)
		}
	}
}
