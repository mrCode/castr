package client

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrCode/castr/internal/daemon"
)

// fakeDaemon is a real unix socket speaking the real protocol, so framing bugs
// are visible rather than mocked away.
type fakeDaemon struct {
	mu       sync.Mutex
	socket   string
	listener net.Listener
	requests []daemon.Request
	reply    func(daemon.Request) daemon.Response
}

func startFakeDaemon(t *testing.T, socket string) *fakeDaemon {
	t.Helper()
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeDaemon{socket: socket, listener: l,
		reply: func(daemon.Request) daemon.Response { return daemon.OK(nil) }}

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go d.serve(conn)
		}
	}()
	t.Cleanup(func() { l.Close() })
	return d
}

func (d *fakeDaemon) serve(conn net.Conn) {
	defer conn.Close()
	raw, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return
	}
	var req daemon.Request
	if err := json.Unmarshal(raw, &req); err != nil {
		return
	}

	d.mu.Lock()
	d.requests = append(d.requests, req)
	reply := d.reply
	d.mu.Unlock()

	out, _ := json.Marshal(reply(req))
	conn.Write(append(out, '\n'))
}

func (d *fakeDaemon) got() []daemon.Request {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]daemon.Request(nil), d.requests...)
}

func (d *fakeDaemon) answerWith(f func(daemon.Request) daemon.Response) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reply = f
}

// emptyList is a well-formed answer for a network with no receivers, which is
// a different thing from an answer with no device list at all.
func emptyList(daemon.Request) daemon.Response {
	return daemon.OK(map[string]any{"devices": []daemon.DeviceJSON{}})
}

func socketPath(t *testing.T) string {
	t.Helper()
	// Short path: a unix socket address is capped at ~100 bytes, and the
	// default temp dir plus a long test name overflows it.
	return filepath.Join(t.TempDir(), "s")
}

func TestARequestReachesTheDaemonAndTheReplyComesBack(t *testing.T) {
	socket := socketPath(t)
	d := startFakeDaemon(t, socket)
	d.answerWith(func(daemon.Request) daemon.Response {
		return daemon.OK(map[string]any{"devices": []daemon.DeviceJSON{
			{ID: "airplay:aa", Name: "Meeting Room", Protocol: "airplay"}}})
	})
	c := New(socket, nil)

	devices, err := c.Devices()

	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 || devices[0].Name != "Meeting Room" {
		t.Errorf("devices = %+v", devices)
	}
	if got := d.got(); len(got) != 1 || got[0].Cmd != daemon.CmdList {
		t.Errorf("daemon saw %+v, want one list", got)
	}
}

func TestTheDaemonsOwnMessageIsWhatTheUserSees(t *testing.T) {
	// Wrapping it in "request failed:" pushes the useful half off the end of
	// a notification.
	socket := socketPath(t)
	d := startFakeDaemon(t, socket)
	d.answerWith(func(daemon.Request) daemon.Response {
		return daemon.Err("device not found: airplay:ghost")
	})
	c := New(socket, nil)

	err := c.Start("airplay:ghost", "mirror")

	if err == nil {
		t.Fatal("want an error")
	}
	if err.Error() != "device not found: airplay:ghost" {
		t.Errorf("err = %q, want the daemon's message verbatim", err)
	}
}

func TestStartCarriesTheModeItWasGiven(t *testing.T) {
	socket := socketPath(t)
	d := startFakeDaemon(t, socket)
	c := New(socket, nil)

	if err := c.Start("airplay:aa", "extend"); err != nil {
		t.Fatal(err)
	}

	got := d.got()
	if len(got) != 1 || got[0].Mode != "extend" || got[0].DeviceID != "airplay:aa" {
		t.Errorf("daemon saw %+v", got)
	}
}

func TestNoDaemonAndNoWayToStartOneIsReportedClearly(t *testing.T) {
	c := New(socketPath(t), nil)

	_, err := c.Devices()

	if !errors.Is(err, ErrNoDaemon) {
		t.Errorf("err = %v, want ErrNoDaemon", err)
	}
}

func TestAClientStartsADaemonWhenNoneIsListening(t *testing.T) {
	// The daemon exits when idle, so most commands arrive with nothing there.
	socket := socketPath(t)
	spawned := make(chan struct{})
	var once sync.Once

	c := New(socket, func() error {
		once.Do(func() {
			startFakeDaemon(t, socket).answerWith(emptyList)
			close(spawned)
		})
		return nil
	})

	if _, err := c.Devices(); err != nil {
		t.Fatalf("the client did not start a daemon: %v", err)
	}

	select {
	case <-spawned:
	default:
		t.Error("no daemon was spawned")
	}
}

func TestTheClientWaitsForASpawnedDaemonToBind(t *testing.T) {
	// Dialling immediately after spawning always fails: the daemon takes its
	// lock, sweeps, and only then binds.
	socket := socketPath(t)
	c := New(socket, func() error {
		go func() {
			time.Sleep(120 * time.Millisecond)
			startFakeDaemon(t, socket).answerWith(emptyList)
		}()
		return nil
	})

	if _, err := c.Devices(); err != nil {
		t.Errorf("the client gave up before the daemon bound: %v", err)
	}
}

func TestTheClientGivesUpOnADaemonThatNeverArrives(t *testing.T) {
	// Otherwise a broken install hangs the cast keybind forever.
	socket := socketPath(t)
	var elapsed time.Duration
	base := time.Now()
	c := New(socket, func() error { return nil })
	c.Now = func() time.Time { return base.Add(elapsed) }
	c.Sleep = func(d time.Duration) { elapsed += d }

	_, err := c.Devices()

	if !errors.Is(err, ErrNoDaemon) {
		t.Fatalf("err = %v, want ErrNoDaemon", err)
	}
	if elapsed < SpawnWait {
		t.Errorf("gave up after %v, want it to wait the full %v", elapsed, SpawnWait)
	}
	// Upper bound too: without it, a loop that never checks its deadline
	// passes this test on a fake clock and hangs on a real one.
	if elapsed > 2*SpawnWait {
		t.Errorf("waited %v, far past the %v it should give up at", elapsed, SpawnWait)
	}
}

func TestASpawnThatFailsSaysWhy(t *testing.T) {
	c := New(socketPath(t), func() error { return errors.New("castrd: no such file") })

	_, err := c.Devices()

	if !strings.Contains(err.Error(), "no such file") {
		t.Errorf("err = %q, want the spawn failure named", err)
	}
}

func TestAStatusPollNeverSpawnsADaemon(t *testing.T) {
	// The bar polls this several times a minute. Spawning from a poll would
	// keep a daemon alive forever and defeat the idle timeout entirely.
	socket := socketPath(t)

	sessions := SessionsQuietly(socket)

	if sessions != nil {
		t.Errorf("sessions = %v, want nothing", sessions)
	}
	if _, err := net.Dial("unix", socket); err == nil {
		t.Error("a status poll started a daemon")
	}
}

func TestAStatusPollReportsLiveCastsWhenADaemonIsThere(t *testing.T) {
	socket := socketPath(t)
	d := startFakeDaemon(t, socket)
	d.answerWith(func(daemon.Request) daemon.Response {
		return daemon.OK(map[string]any{"sessions": []daemon.SessionJSON{
			{DeviceID: "a", Name: "TV", Mode: "mirror", State: "streaming"}}})
	})

	sessions := SessionsQuietly(socket)

	if len(sessions) != 1 || sessions[0].Name != "TV" {
		t.Errorf("sessions = %+v", sessions)
	}
}

func TestADaemonThatHangsUpWithoutReplyingIsReported(t *testing.T) {
	// Silence is indistinguishable from success if you do not check.
	socket := socketPath(t)
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		conn, err := l.Accept()
		if err == nil {
			conn.Close() // read nothing, say nothing
		}
	}()

	c := New(socket, nil)
	c.Timeout = 2 * time.Second

	if _, err := c.Devices(); err == nil {
		t.Error("a daemon that said nothing was treated as success")
	}
}

func TestTheRequestTimeoutOutlastsARealHandshake(t *testing.T) {
	// Capture began 23s after session-ready on real hardware, and the user may
	// still have to answer a screen-share prompt. A client that gave up at 30s
	// reported failure for a cast that then worked.
	if Timeout < time.Minute {
		t.Errorf("Timeout = %v, too short for a real AirPlay start", Timeout)
	}
}

func TestAddSendsTheWholeDevice(t *testing.T) {
	socket := socketPath(t)
	d := startFakeDaemon(t, socket)
	c := New(socket, nil)

	if err := c.Add(daemon.DeviceJSON{ID: "meeting", Name: "Meeting Room",
		Address: "10.10.10.231", Port: 7000, Protocol: "airplay"}); err != nil {
		t.Fatal(err)
	}

	got := d.got()
	if len(got) != 1 || got[0].Device == nil {
		t.Fatalf("daemon saw %+v", got)
	}
	if got[0].Device.Address != "10.10.10.231" {
		t.Errorf("address = %q, want the one the user typed", got[0].Device.Address)
	}
}

func TestAnOkWithNoDeviceListIsNotAnEmptyNetwork(t *testing.T) {
	// Reading it as "no receivers" is how an empty cast menu gets shown for a
	// room full of them -- the single most reported symptom this project had.
	socket := socketPath(t)
	d := startFakeDaemon(t, socket)
	d.answerWith(func(daemon.Request) daemon.Response { return daemon.OK(nil) })
	c := New(socket, nil)

	devices, err := c.Devices()

	if err == nil {
		t.Fatalf("devices = %v with no error; a missing list must not read as an empty one", devices)
	}
	// The wrapped decoder error also mentions "device list", so checking for
	// that alone proves nothing. What matters is that the user is not shown
	// raw JSON-decoder wording they cannot act on.
	if strings.Contains(err.Error(), "unexpected end of JSON input") {
		t.Errorf("err = %q, want a message a user can act on", err)
	}
}

func TestAnEmptyDeviceListIsReportedAsEmptyRatherThanAsAFailure(t *testing.T) {
	// A network with genuinely no receivers is a normal answer.
	socket := socketPath(t)
	startFakeDaemon(t, socket).answerWith(emptyList)
	c := New(socket, nil)

	devices, err := c.Devices()

	if err != nil {
		t.Fatalf("an empty network was reported as an error: %v", err)
	}
	if len(devices) != 0 {
		t.Errorf("devices = %v", devices)
	}
}
