package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrCode/castr/internal/session"
)

// ask sends one request over a real socket and returns the reply, which is the
// only way to catch framing bugs that Handle alone cannot see.
func (r *rig) ask(t *testing.T, socket string, req Request) Response {
	t.Helper()
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	line, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}

	raw, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatalf("reading the reply: %v (got %q)", err, raw)
	}
	var resp Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decoding %q: %v", raw, err)
	}
	return resp
}

func (r *rig) serve(t *testing.T) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "daemon.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.daemon.Serve(ctx, socket) }()

	// Wait for the socket to accept before returning, so no test races startup.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("unix", socket); err == nil {
			c.Close()
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	t.Cleanup(func() {
		cancel()
		r.daemon.Shutdown()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve returned %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return after shutdown")
		}
	})
	return socket
}

func TestListAnswersOverTheSocket(t *testing.T) {
	r := newRig(t, dev("a", "Meeting Room"))
	socket := r.serve(t)

	resp := r.ask(t, socket, Request{Cmd: CmdList})

	if !resp.Ok {
		t.Fatalf("list failed: %s", resp.Error)
	}
	var data struct {
		Devices []DeviceJSON `json:"devices"`
	}
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Devices) != 1 || data.Devices[0].Name != "Meeting Room" {
		t.Errorf("devices = %+v, want the one receiver", data.Devices)
	}
}

func TestAStartAndAStopRoundTrip(t *testing.T) {
	r := newRig(t, dev("a", "TV"))
	socket := r.serve(t)

	start := r.ask(t, socket, Request{Cmd: CmdStart, DeviceID: "a", Mode: session.ModeExtend})
	if !start.Ok {
		t.Fatalf("start failed: %s", start.Error)
	}

	status := r.ask(t, socket, Request{Cmd: CmdStatus})
	var data struct {
		Sessions []SessionJSON `json:"sessions"`
	}
	if err := json.Unmarshal(status.Data, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Sessions) != 1 || data.Sessions[0].State != string(session.Streaming) {
		t.Fatalf("sessions = %+v, want one streaming", data.Sessions)
	}
	if data.Sessions[0].Mode != session.ModeExtend {
		t.Errorf("mode = %q, want the mode the client asked for", data.Sessions[0].Mode)
	}

	stop := r.ask(t, socket, Request{Cmd: CmdStop, DeviceID: "a"})
	if !stop.Ok {
		t.Fatalf("stop failed: %s", stop.Error)
	}
	if r.backend.stopCount() != 1 {
		t.Error("the backend was never asked to stop")
	}
}

func TestAFailureCarriesTheReasonRatherThanAnEmptyOk(t *testing.T) {
	// The user reads this string. "device not found: a" is actionable;
	// a bare failure is not.
	r := newRig(t)
	socket := r.serve(t)

	resp := r.ask(t, socket, Request{Cmd: CmdStart, DeviceID: "ghost", Mode: session.ModeMirror})

	if resp.Ok {
		t.Fatal("starting an unknown device reported success")
	}
	if !strings.Contains(resp.Error, "ghost") {
		t.Errorf("error = %q, want it to name the device", resp.Error)
	}
}

func TestAMalformedRequestIsAnsweredRatherThanDroppingTheConnection(t *testing.T) {
	// A client that gets no reply hangs until its own timeout, which looks
	// exactly like a daemon that died.
	r := newRig(t)
	socket := r.serve(t)
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	if _, err := conn.Write([]byte("this is not json\n")); err != nil {
		t.Fatal(err)
	}

	raw, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatalf("no reply to a malformed request: %v", err)
	}
	var resp Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Ok {
		t.Error("a malformed request was answered with success")
	}
}

func TestAnUnknownCommandIsRefusedByName(t *testing.T) {
	r := newRig(t)
	socket := r.serve(t)

	resp := r.ask(t, socket, Request{Cmd: "reboot"})

	if resp.Ok {
		t.Fatal("an unknown command reported success")
	}
	if !strings.Contains(resp.Error, "reboot") {
		t.Errorf("error = %q, want it to name the command", resp.Error)
	}
}

func TestATruncatedResponseCannotReadAsSuccess(t *testing.T) {
	// Ok is explicit rather than inferred from an empty Error, so a reply
	// that fails to arrive intact decodes as failure.
	var resp Response
	if err := json.Unmarshal([]byte(`{}`), &resp); err != nil {
		t.Fatal(err)
	}

	if resp.Ok {
		t.Error("an empty object decoded as a successful response")
	}
}

func TestTheSocketIsNotReadableByOtherUsers(t *testing.T) {
	// It carries commands that drive the user's display.
	r := newRig(t)
	socket := r.serve(t)

	info, err := os.Stat(socket)
	if err != nil {
		t.Fatal(err)
	}

	if perm := info.Mode().Perm(); perm&fs.FileMode(0o077) != 0 {
		t.Errorf("socket mode = %v, want no group or other access", perm)
	}
}

func TestAddingAManualReceiverMakesItStartable(t *testing.T) {
	// The reason this command exists: a receiver mDNS cannot see, on another
	// subnet, which is a configuration that works.
	r := newRig(t)
	socket := r.serve(t)

	add := r.ask(t, socket, Request{Cmd: CmdAdd, Device: &DeviceJSON{
		ID: "meeting", Name: "Meeting Room", Address: "10.10.10.231"}})
	if !add.Ok {
		t.Fatalf("add failed: %s", add.Error)
	}

	start := r.ask(t, socket, Request{Cmd: CmdStart, DeviceID: "meeting", Mode: session.ModeMirror})
	if !start.Ok {
		t.Errorf("a manually added receiver could not be started: %s", start.Error)
	}
}

func TestAManualReceiverWithoutAnAddressIsRefused(t *testing.T) {
	r := newRig(t)
	socket := r.serve(t)

	resp := r.ask(t, socket, Request{Cmd: CmdAdd, Device: &DeviceJSON{ID: "x", Name: "No Address"}})

	if resp.Ok {
		t.Error("accepted a receiver with no address, which can never be cast to")
	}
}

func TestForgettingSomethingUnknownSaysSoRatherThanReportingSuccess(t *testing.T) {
	r := newRig(t)
	socket := r.serve(t)

	resp := r.ask(t, socket, Request{Cmd: CmdForget, DeviceID: "nothing"})

	if resp.Ok {
		t.Error("forgetting an unknown receiver reported success")
	}
}

func TestQuitStopsTheDaemon(t *testing.T) {
	r := newRig(t)
	socket := r.serve(t)

	if resp := r.ask(t, socket, Request{Cmd: CmdQuit}); !resp.Ok {
		t.Fatalf("quit failed: %s", resp.Error)
	}

	select {
	case <-r.daemon.Stopping():
	case <-time.After(2 * time.Second):
		t.Error("quit did not stop the daemon")
	}
}

func TestServeReplacesItsOwnStaleSocketFile(t *testing.T) {
	// A daemon that died badly leaves the file behind. Refusing to start
	// because of it would need a manual rm before casting again -- and the
	// lock, not the socket file, is what prevents a second daemon.
	r := newRig(t)
	dir := t.TempDir()
	socket := filepath.Join(dir, "daemon.sock")
	if err := os.WriteFile(socket, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.daemon.Serve(ctx, socket) }()
	defer r.daemon.Shutdown()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("unix", socket); err == nil {
			c.Close()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("Serve never came up over a stale socket file")
}

func TestTheSocketIsRemovedOnShutdown(t *testing.T) {
	r := newRig(t)
	socket := filepath.Join(t.TempDir(), "daemon.sock")
	done := make(chan error, 1)
	go func() { done <- r.daemon.Serve(context.Background(), socket) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("unix", socket); err == nil {
			c.Close()
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.daemon.Shutdown()
	<-done

	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Error("the socket file outlived the daemon")
	}
}

func TestASocketPathTheKernelWillRefuseIsExplained(t *testing.T) {
	// "bind: invalid argument" says nothing about a path being too long, and
	// the limit is about 100 bytes -- reachable with a deep XDG_STATE_HOME.
	r := newRig(t)
	long := filepath.Join(t.TempDir(), strings.Repeat("d/", 60), "daemon.sock")

	err := r.daemon.Serve(context.Background(), long)

	if err == nil {
		t.Fatal("an impossible socket path was accepted")
	}
	if !strings.Contains(err.Error(), "too") && !strings.Contains(err.Error(), "past the") {
		t.Errorf("err = %q, want it to say the path is too long", err)
	}
}

func TestTheDefaultSocketLivesInTheRuntimeDirectory(t *testing.T) {
	// Per-user, cleared at logout, and short enough for the kernel.
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")

	got := DefaultSocketPath()

	if !strings.HasPrefix(got, "/run/user/1000/") {
		t.Errorf("socket = %q, want it under XDG_RUNTIME_DIR", got)
	}
	if err := CheckSocketPath(got); err != nil {
		t.Errorf("the default path is not usable: %v", err)
	}
}

func TestAnAbsentRuntimeDirectoryFallsBackToTheStateDirectory(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("XDG_STATE_HOME", "/tmp/castr-test-state")

	got := DefaultSocketPath()

	if !strings.HasPrefix(got, "/tmp/castr-test-state/") {
		t.Errorf("socket = %q, want the state directory fallback", got)
	}
}
