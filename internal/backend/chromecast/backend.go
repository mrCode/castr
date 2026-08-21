// Package chromecast casts the desktop to a Chromecast.
//
// The shape is different from AirPlay's. There, a separate program
// (doubletake) owns capture and the protocol, and castr supervises it. Here
// castr owns everything: it captures the screen itself, serves the result over
// HTTP, and tells the television where to fetch it. What the television needs
// is recorded in docs/chromecast.md; what makes the capture safe is in
// docs/capture-safety.md.
package chromecast

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/mrCode/castr/internal/capture"
	"github.com/mrCode/castr/internal/discovery"
	"github.com/mrCode/castr/internal/session"
)

// Emit reports a state change to the daemon.
type Emit func(device discovery.Device, state session.State, reason string)

// Portal is a screen-capture grant.
type Portal interface {
	// Node is the PipeWire node the user agreed to share.
	Node() uint32
	// Descriptor is the PipeWire remote, handed to the capture process.
	Descriptor() *os.File
	Close() error
}

// Pipeline is a running capture process.
type Pipeline interface {
	// Pid identifies it in the PipeWire graph, which is how castr proves what
	// it is capturing.
	Pid() int
	// Wait blocks until it exits.
	Wait() error
	// Terminate stops it and everything it spawned.
	Terminate() error
}

// Server serves the segments a receiver fetches.
type Server interface {
	URLFor(name string) string
	Stats() (bytes int64, requests, playlistReads int)
	Close() error
}

// Caster is the connection to a receiver.
type Caster interface {
	Launch(ctx context.Context, appID string) (App, error)
	Load(ctx context.Context, app App, url, contentType, title string) error
	StopApp(ctx context.Context, app App) error
	Close() error
}

// App is a launched receiver application.
type App struct {
	SessionID   string
	TransportID string
	AppID       string
}

// Config holds what the user can set.
type Config struct {
	FPS            int
	Encoder        string
	Bitrate        int
	Width, Height  int
	Port           int
	SegmentSeconds int
	// StartTimeout bounds the whole start, including the time the user spends
	// choosing a screen at the share prompt.
	StartTimeout time.Duration
}

// Backend implements the daemon's Backend interface for Chromecast.
type Backend struct {
	Config Config

	// Everything the backend touches outside its own process is injected, so
	// its tests need no compositor, no network and no television.
	OpenPortal   func(ctx context.Context) (Portal, error)
	SerialOf     func(node uint32) (uint32, error)
	Graph        capture.Graph
	StartCapture func(opts capture.Options) (Pipeline, error)
	Serve        func(bindIP string, port int, dir string) (Server, error)
	// Restrict, when set, tells the server which address may fetch the
	// stream. Applied before the server can be reached rather than after,
	// since "after" is a window in which anything on the network can read it.
	Restrict     func(server Server, receiverIP string)
	Dial         func(ctx context.Context, addr string) (Caster, error)
	LocalAddress func(host string) (string, error)
	TempDir      func() (string, error)
	Emit         Emit

	mu       sync.Mutex
	sessions map[string]*castSession
}

type castSession struct {
	device   discovery.Device
	granted  uint32
	portal   Portal
	pipeline Pipeline
	server   Server
	caster   Caster
	app      App
	dir      string

	stopping bool
}

// ErrNoPin exists because the daemon offers pin submission for every backend.
var ErrNoPin = errors.New("a Chromecast does not ask for a pairing code")

// errStopped ends a start that the user cancelled while it was still setting
// up. Starting a cast can take minutes -- most of it spent waiting at the
// share prompt -- and a stop arriving in that window used to tear down an
// empty record while the start went on to bring up a capture, an HTTP server
// serving the screen to the network, and a session on the television, none of
// which anything was left tracking. The user had pressed stop; their screen
// was still being broadcast.
var errStopped = errors.New("the cast was stopped while it was starting")

// resources is everything a session holds. Taking them is what makes teardown
// happen exactly once: whichever goroutine takes them owns releasing them, and
// the other finds nothing left to release.
type resources struct {
	caster   Caster
	app      App
	server   Server
	pipeline Pipeline
	portal   Portal
	dir      string
}

// take removes the session's resources and returns them.
func (b *Backend) take(cs *castSession) resources {
	b.mu.Lock()
	defer b.mu.Unlock()

	r := resources{cs.caster, cs.app, cs.server, cs.pipeline, cs.portal, cs.dir}
	cs.caster, cs.server, cs.pipeline, cs.portal, cs.dir = nil, nil, nil, nil, ""
	cs.app = App{}
	return r
}

// adopt records a resource the session now owns, and reports whether the cast
// has been stopped underneath it.
//
// The resource is stored even when the answer is "stopped", so that the error
// path releases it rather than the caller having to unwind by hand at every
// step.
func (b *Backend) adopt(cs *castSession, assign func()) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	assign()
	if cs.stopping {
		return errStopped
	}
	return nil
}

// SubmitPin is never valid here.
func (b *Backend) SubmitPin(context.Context, discovery.Device, string) error {
	return ErrNoPin
}

func (b *Backend) init() {
	if b.sessions == nil {
		b.sessions = map[string]*castSession{}
	}
}

// Start captures the screen and asks the receiver to play it.
func (b *Backend) Start(ctx context.Context, device discovery.Device, mode string) error {
	b.mu.Lock()
	b.init()
	if _, running := b.sessions[device.ID]; running {
		b.mu.Unlock()
		return fmt.Errorf("already casting to %s", device.Name)
	}
	cs := &castSession{device: device}
	b.sessions[device.ID] = cs
	b.mu.Unlock()

	b.emit(device, session.Connecting, "")

	err := b.start(ctx, cs, mode)

	// Checked once more with everything in place: a stop that arrived during
	// the last step must not leave a cast running that nothing is tracking.
	b.mu.Lock()
	if cs.stopping && err == nil {
		err = errStopped
	}
	pipeline := cs.pipeline
	b.mu.Unlock()

	if err != nil {
		b.release(b.take(cs))
		b.forget(device.ID)
		if errors.Is(err, errStopped) {
			// Stop already announced itself; saying it failed as well would
			// report the user's own cancellation as a fault.
			return nil
		}
		b.emit(device, session.Failed, err.Error())
		return err
	}

	b.emit(device, session.Streaming, "")
	go b.watch(cs, pipeline)
	go b.keepVerifying(cs, pipeline)
	return nil
}

// RecheckInterval is how often a running cast re-proves what it is capturing.
//
// Verifying once at startup would leave the rest of the cast -- possibly hours
// -- unchecked, while the docs promise that castr never streams a source it
// has not identified. The granted node can go away underneath a running
// pipeline: pipewire.service restarts on an upgrade, the user revokes the
// grant, a monitor is unplugged. The source has autoconnect on, because no
// configuration of it fails safe, so what it reaches for next is the default
// video device.
//
// One pw-dump every few seconds is nothing beside H.264 encoding.
const RecheckInterval = 3 * time.Second

// keepVerifying tears the cast down if it stops capturing the granted screen.
func (b *Backend) keepVerifying(cs *castSession, pipeline Pipeline) {
	guard := &capture.Guard{Graph: b.Graph, Granted: cs.granted}
	ticker := time.NewTicker(RecheckInterval)
	defer ticker.Stop()

	for range ticker.C {
		b.mu.Lock()
		live := b.sessions[cs.device.ID] == cs && !cs.stopping
		b.mu.Unlock()
		if !live {
			return
		}

		err := guard.Recheck(pipeline.Pid())
		if err == nil {
			continue
		}

		b.release(b.take(cs))
		b.forget(cs.device.ID)
		b.emit(cs.device, session.Failed, err.Error())
		return
	}
}

func (b *Backend) start(ctx context.Context, cs *castSession, mode string) error {
	timeout := b.Config.StartTimeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// The receiver has to reach this machine, so the address it will use is
	// settled before anything is captured. A cast that gets as far as asking
	// the user to pick a screen and then fails on routing has wasted the one
	// step that needs a person.
	bind, err := b.LocalAddress(cs.device.Address)
	if err != nil {
		return err
	}

	portal, err := b.OpenPortal(ctx)
	if err != nil {
		return fmt.Errorf("getting permission to capture the screen: %w", err)
	}
	if err := b.adopt(cs, func() { cs.portal, cs.granted = portal, portal.Node() }); err != nil {
		return err
	}

	serial, err := b.SerialOf(portal.Node())
	if err != nil {
		return err
	}

	dir, err := b.TempDir()
	if err != nil {
		return fmt.Errorf("making room for the stream: %w", err)
	}
	if err := b.adopt(cs, func() { cs.dir = dir }); err != nil {
		return err
	}

	opts := capture.Options{
		NodeID: portal.Node(), Serial: serial, FD: portal.Descriptor(),
		FPS: b.Config.FPS, Encoder: b.Config.Encoder, Bitrate: b.Config.Bitrate,
		Width: b.Config.Width, Height: b.Config.Height,
		Container: capture.HLS, Dir: dir,
		SegmentSeconds: b.Config.SegmentSeconds,
	}
	pipeline, err := b.StartCapture(opts)
	if err != nil {
		return fmt.Errorf("starting the capture: %w", err)
	}
	if err := b.adopt(cs, func() { cs.pipeline = pipeline }); err != nil {
		return err
	}

	// Before a single byte can be fetched. The guard is the only thing
	// standing between a mismatched capture node and the user's webcam on a
	// television; running it after the stream is reachable would defeat it.
	guard := &capture.Guard{Graph: b.Graph, Granted: portal.Node()}
	if err := guard.Verify(ctx, pipeline.Pid()); err != nil {
		return err
	}

	server, err := b.Serve(bind, b.Config.Port, dir)
	if err != nil {
		return err
	}
	if b.Restrict != nil {
		b.Restrict(server, cs.device.Address)
	}
	if err := b.adopt(cs, func() { cs.server = server }); err != nil {
		return err
	}

	// A receiver will not start on a playlist holding one segment: HLS clients
	// want a few target durations of media available before they begin.
	if err := waitForPlaylist(ctx, dir, 4); err != nil {
		return err
	}

	caster, err := b.Dial(ctx, net.JoinHostPort(cs.device.Address, strconv.Itoa(castPort(cs.device))))
	if err != nil {
		return err
	}
	if err := b.adopt(cs, func() { cs.caster = caster }); err != nil {
		return err
	}

	app, err := caster.Launch(ctx, DefaultMediaReceiver)
	if err != nil {
		return err
	}
	if err := b.adopt(cs, func() { cs.app = app }); err != nil {
		return err
	}

	return caster.Load(ctx, app,
		server.URLFor(capture.PlaylistName), capture.HLSContentType, "castr")
}

// DefaultMediaReceiver is Google's own player, present on every receiver.
const DefaultMediaReceiver = "CC1AD845"

func castPort(d discovery.Device) int {
	if d.Port > 0 {
		return d.Port
	}
	return 8009
}

// watch reports a capture that dies on its own, rather than leaving the
// session green while the television shows nothing.
//
// The pipeline is passed in rather than read off the session, because by the
// time it exits the session may have been torn down and its fields taken.
func (b *Backend) watch(cs *castSession, pipeline Pipeline) {
	_ = pipeline.Wait()

	b.mu.Lock()
	stopping := cs.stopping
	b.mu.Unlock()
	if stopping {
		return
	}

	b.release(b.take(cs))
	b.forget(cs.device.ID)
	b.emit(cs.device, session.Failed, "the capture stopped unexpectedly")
}

// Stop ends a cast.
func (b *Backend) Stop(ctx context.Context, device discovery.Device) error {
	b.mu.Lock()
	b.init()
	cs, ok := b.sessions[device.ID]
	if ok {
		cs.stopping = true
	}
	b.mu.Unlock()
	if !ok {
		return nil
	}

	// Announced before the work, so a stop that takes a moment does not look
	// like a cast that is still running.
	b.emit(device, session.Stopping, "")

	held := b.take(cs)
	if held.caster != nil && held.app.TransportID != "" {
		_ = held.caster.StopApp(ctx, held.app)
	}
	err := b.release(held)
	b.forget(device.ID)
	b.emit(device, session.Idle, "")
	return err
}

// release closes everything, in the reverse of the order it was taken, and
// keeps going after a failure so one stuck resource cannot strand the rest.
//
// It takes the resources by value rather than reading the session, so it never
// touches shared state and can safely run while a start is still in flight.
func (b *Backend) release(r resources) error {
	var first error
	fail := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}

	if r.caster != nil {
		fail(r.caster.Close())
	}
	if r.server != nil {
		fail(r.server.Close())
	}
	if r.pipeline != nil {
		fail(r.pipeline.Terminate())
	}
	if r.portal != nil {
		fail(r.portal.Close())
	}
	if r.dir != "" {
		fail(os.RemoveAll(r.dir))
	}
	return first
}

func (b *Backend) forget(id string) {
	b.mu.Lock()
	delete(b.sessions, id)
	b.mu.Unlock()
}

func (b *Backend) emit(device discovery.Device, state session.State, reason string) {
	if b.Emit != nil {
		b.Emit(device, state, reason)
	}
}

// waitForPlaylist blocks until the playlist lists enough segments.
func waitForPlaylist(ctx context.Context, dir string, want int) error {
	path := dir + "/" + capture.PlaylistName
	for {
		if n, err := countSegments(path); err == nil && n >= want {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the capture produced no video to send: %w", ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func countSegments(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	count := 0
	for i := 0; i+8 <= len(raw); i++ {
		if string(raw[i:i+8]) == "#EXTINF:" {
			count++
		}
	}
	return count, nil
}
