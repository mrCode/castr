// Package client talks to the daemon, starting one if none is listening.
package client

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/mrCode/castr/internal/daemon"
)

// SpawnFunc starts a daemon in the background. Injected: no test spawns one.
type SpawnFunc func() error

// SpawnWait is how long to wait for a freshly spawned daemon to bind its
// socket. Generous because the daemon takes the lock first, and that itself
// waits for a departing daemon to let go.
const SpawnWait = 6 * time.Second

const spawnPoll = 50 * time.Millisecond

// Timeout bounds one request. Long because `start` legitimately blocks while
// the receiver handshakes and the user answers the screen-share prompt --
// capture began 23s after session-ready on real hardware, and a client that
// gave up at 30s reported a failure for a cast that then worked.
const Timeout = 3 * time.Minute

// Client is a connection-per-request client.
type Client struct {
	Socket  string
	Spawn   SpawnFunc
	Timeout time.Duration

	// Now and Sleep exist so the spawn wait is testable without really waiting.
	Now   func() time.Time
	Sleep func(time.Duration)
}

// New returns a client with the production timings.
func New(socket string, spawn SpawnFunc) *Client {
	return &Client{Socket: socket, Spawn: spawn, Timeout: Timeout,
		Now: time.Now, Sleep: time.Sleep}
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) sleep(d time.Duration) {
	if c.Sleep != nil {
		c.Sleep(d)
		return
	}
	time.Sleep(d)
}

// ErrNoDaemon means no daemon is listening and none could be started.
var ErrNoDaemon = errors.New("no castr daemon is running")

// Do sends one request, starting a daemon first if nothing is listening.
func (c *Client) Do(req daemon.Request) (daemon.Response, error) {
	conn, err := c.connect()
	if err != nil {
		return daemon.Response{}, err
	}
	defer conn.Close()

	timeout := c.Timeout
	if timeout == 0 {
		timeout = Timeout
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))

	line, err := json.Marshal(req)
	if err != nil {
		return daemon.Response{}, fmt.Errorf("encoding %s: %w", req.Cmd, err)
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return daemon.Response{}, fmt.Errorf("sending %s: %w", req.Cmd, err)
	}

	raw, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil && len(raw) == 0 {
		return daemon.Response{}, fmt.Errorf("no reply to %s: %w", req.Cmd, err)
	}

	var resp daemon.Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return daemon.Response{}, fmt.Errorf("decoding the reply to %s: %w", req.Cmd, err)
	}
	if !resp.Ok {
		// The daemon's message is the useful one; it names the device or the
		// reason. Wrapping it in "request failed:" only pushes that off the
		// end of a notification.
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}

// connect dials the socket, spawning a daemon if nothing answers.
//
// The retry loop matters more than it looks: the daemon exits when idle, so a
// client arriving during that shutdown finds a socket file that no longer
// accepts. Failing there put "no daemon is running" on screen for a system
// that was working perfectly a second later.
func (c *Client) connect() (net.Conn, error) {
	if conn, err := net.Dial("unix", c.Socket); err == nil {
		return conn, nil
	}

	if c.Spawn == nil {
		return nil, ErrNoDaemon
	}
	if err := c.Spawn(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoDaemon, err)
	}

	deadline := c.now().Add(SpawnWait)
	for {
		if conn, err := net.Dial("unix", c.Socket); err == nil {
			return conn, nil
		}
		if !c.now().Before(deadline) {
			return nil, fmt.Errorf("%w: it did not start within %v", ErrNoDaemon, SpawnWait)
		}
		c.sleep(spawnPoll)
	}
}

// Devices asks for the known receivers.
func (c *Client) Devices() ([]daemon.DeviceJSON, error) {
	resp, err := c.Do(daemon.Request{Cmd: daemon.CmdList})
	if err != nil {
		return nil, err
	}
	// An "ok" carrying no device list is NOT an empty network. Reading it that
	// way is how an empty cast menu gets shown for a room full of receivers,
	// so it is an error the user can act on instead.
	if len(resp.Data) == 0 {
		return nil, errors.New("the daemon answered list with no device list")
	}
	var data struct {
		Devices []daemon.DeviceJSON `json:"devices"`
	}
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return nil, fmt.Errorf("decoding the device list: %w", err)
	}
	return data.Devices, nil
}

// Sessions asks for the live casts.
func (c *Client) Sessions() ([]daemon.SessionJSON, error) {
	resp, err := c.Do(daemon.Request{Cmd: daemon.CmdStatus})
	if err != nil {
		return nil, err
	}
	if len(resp.Data) == 0 {
		return nil, errors.New("the daemon answered status with no session list")
	}
	var data struct {
		Sessions []daemon.SessionJSON `json:"sessions"`
	}
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return nil, fmt.Errorf("decoding the session list: %w", err)
	}
	return data.Sessions, nil
}

// Start begins a cast.
func (c *Client) Start(deviceID, mode string) error {
	_, err := c.Do(daemon.Request{Cmd: daemon.CmdStart, DeviceID: deviceID, Mode: mode})
	return err
}

// Stop ends a cast.
func (c *Client) Stop(deviceID string) error {
	_, err := c.Do(daemon.Request{Cmd: daemon.CmdStop, DeviceID: deviceID})
	return err
}

// SubmitPin forwards a pairing code.
func (c *Client) SubmitPin(deviceID, pin string) error {
	_, err := c.Do(daemon.Request{Cmd: daemon.CmdPin, DeviceID: deviceID, Pin: pin})
	return err
}

// Add registers a receiver by address.
func (c *Client) Add(device daemon.DeviceJSON) error {
	_, err := c.Do(daemon.Request{Cmd: daemon.CmdAdd, Device: &device})
	return err
}

// Forget drops a manually registered receiver.
func (c *Client) Forget(deviceID string) error {
	_, err := c.Do(daemon.Request{Cmd: daemon.CmdForget, DeviceID: deviceID})
	return err
}

// Quit asks the daemon to exit.
func (c *Client) Quit() error {
	_, err := c.Do(daemon.Request{Cmd: daemon.CmdQuit})
	return err
}

// SessionsQuietly returns the live casts, and NO error when there is simply no
// daemon.
//
// The bar indicator polls this several times a minute. Spawning a daemon from
// a status poll would keep one alive forever -- defeating the idle timeout --
// and printing an error would put a red indicator on the bar for the normal
// state of not casting.
func SessionsQuietly(socket string) []daemon.SessionJSON {
	c := &Client{Socket: socket, Timeout: 2 * time.Second}
	sessions, err := c.Sessions()
	if err != nil {
		return nil
	}
	return sessions
}
