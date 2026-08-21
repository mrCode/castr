// Package portal opens an xdg-desktop-portal ScreenCast session and hands back
// the PipeWire connection the compositor granted.
//
// It exists because Chromecast capture is castr's own job: AirPlay delegates
// the whole of it to doubletake, which does its own portal handshake.
//
// THE RULE THIS PACKAGE EXISTS TO ENFORCE: the returned fd is a PipeWire
// connection on which the granted node is the ONLY thing visible. A capture
// pipeline must be given that fd, and must inherit it. omarchy-cast's Chromecast
// backend was disabled after it was observed encoding the built-in WEBCAM and
// streaming it to a television; the pipeline had fallen back to the ordinary
// PipeWire daemon and picked the default video source. A cast that might
// broadcast someone's camera is not a feature that ships behind a warning.
package portal

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	busName    = "org.freedesktop.portal.Desktop"
	objectPath = "/org/freedesktop/portal/desktop"
	iface      = "org.freedesktop.portal.ScreenCast"
	requestIfc = "org.freedesktop.portal.Request"
)

// SourceType is the portal's bitmask: 1 monitor, 2 window, 4 virtual.
const (
	SourceMonitor = 1
	SourceWindow  = 2
)

// CursorMode: 1 hidden, 2 embedded, 4 metadata.
const CursorEmbedded = 2

// Session is a granted screen capture.
type Session struct {
	// FD is the PipeWire connection the portal granted. The granted node is
	// the only one visible on it, which is what stops a pipeline reaching a
	// camera. Callers MUST pass it to the child and close it after.
	FD *os.File

	// NodeID identifies the stream on that connection.
	NodeID uint32

	// RestoreToken lets a later session skip the picker, if the user allowed
	// it. Empty when they did not.
	RestoreToken string

	conn   *dbus.Conn
	handle dbus.ObjectPath
	once   sync.Once
}

// Close ends the session and releases the fd.
func (s *Session) Close() error {
	var err error
	s.once.Do(func() {
		if s.FD != nil {
			err = s.FD.Close()
		}
		if s.conn != nil {
			if s.handle != "" {
				_ = s.conn.Object(busName, s.handle).
					Call("org.freedesktop.portal.Session.Close", 0).Err
			}
			_ = s.conn.Close()
		}
	})
	return err
}

// Options configure what to ask the compositor for.
type Options struct {
	// RestoreToken from a previous session; "" asks the user.
	RestoreToken string

	// Persist: 0 never, 1 for this run, 2 until revoked.
	Persist uint32

	// Timeout bounds the whole handshake. It is generous because a human may
	// have to notice and answer a "Select what to share" window, and because a
	// picker that maps no window makes this hang invisibly -- which happened on
	// this machine and cost an afternoon.
	Timeout time.Duration
}

// Open runs CreateSession -> SelectSources -> Start and returns the granted
// stream. The caller owns the Session and must Close it.
func Open(ctx context.Context, opts Options) (*Session, error) {
	if opts.Timeout == 0 {
		opts.Timeout = 3 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	// A PRIVATE connection, not dbus.SessionBus(). The shared connection is
	// handed to every consumer in the process, its signal channels are drained
	// by whoever else registered, and closing it would break them. On a shared
	// bus this handshake timed out at whichever call happened to lose the race
	// for its own Response -- CreateSession one run, SelectSources the next.
	conn, err := dbus.SessionBusPrivate()
	if err != nil {
		return nil, fmt.Errorf("connecting to the session bus: %w", err)
	}
	if err := conn.Auth(nil); err != nil {
		conn.Close()
		return nil, fmt.Errorf("authenticating to the session bus: %w", err)
	}
	if err := conn.Hello(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("greeting the session bus: %w", err)
	}

	sess := &Session{conn: conn}
	replies, err := subscribeResponses(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}

	obj := conn.Object(busName, objectPath)
	token := handleToken()

	// --- CreateSession ---
	res, err := request(ctx, obj, replies, conn, "CreateSession", map[string]dbus.Variant{
		"handle_token":         dbus.MakeVariant(token()),
		"session_handle_token": dbus.MakeVariant(token()),
	})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("CreateSession: %w", err)
	}
	handleStr, ok := res["session_handle"].Value().(string)
	if !ok {
		conn.Close()
		return nil, fmt.Errorf("CreateSession returned no session handle")
	}
	sess.handle = dbus.ObjectPath(handleStr)

	// --- SelectSources ---
	selectOpts := map[string]dbus.Variant{
		"handle_token": dbus.MakeVariant(token()),
		"types":        dbus.MakeVariant(uint32(SourceMonitor | SourceWindow)),
		"multiple":     dbus.MakeVariant(false),
		"cursor_mode":  dbus.MakeVariant(uint32(CursorEmbedded)),
	}
	if opts.Persist > 0 {
		selectOpts["persist_mode"] = dbus.MakeVariant(opts.Persist)
	}
	if opts.RestoreToken != "" {
		selectOpts["restore_token"] = dbus.MakeVariant(opts.RestoreToken)
	}
	if _, err := request(ctx, obj, replies, conn, "SelectSources", selectOpts, sess.handle); err != nil {
		sess.Close()
		return nil, fmt.Errorf("SelectSources: %w", err)
	}

	// --- Start: this is where the user answers the picker ---
	started, err := request(ctx, obj, replies, conn, "Start",
		map[string]dbus.Variant{"handle_token": dbus.MakeVariant(token())},
		sess.handle, "")
	if err != nil {
		sess.Close()
		return nil, fmt.Errorf("Start: %w", err)
	}
	if v, ok := started["restore_token"].Value().(string); ok {
		sess.RestoreToken = v
	}
	nodeID, err := firstStreamNode(started["streams"])
	if err != nil {
		sess.Close()
		return nil, err
	}
	sess.NodeID = nodeID

	// --- OpenPipeWireRemote: the fd, and the whole point of this package ---
	var fd dbus.UnixFD
	if err := obj.CallWithContext(ctx, iface+".OpenPipeWireRemote", 0,
		sess.handle, map[string]dbus.Variant{}).Store(&fd); err != nil {
		sess.Close()
		return nil, fmt.Errorf("OpenPipeWireRemote: %w", err)
	}
	sess.FD = os.NewFile(uintptr(fd), "pipewire-portal")
	if sess.FD == nil {
		sess.Close()
		return nil, fmt.Errorf("the portal returned an unusable file descriptor")
	}
	return sess, nil
}

// request makes a portal call and waits for the Response signal it answers
// with. Every ScreenCast method is asynchronous this way: the method returns a
// request handle and the real answer arrives as a signal.
func request(ctx context.Context, obj dbus.BusObject, replies <-chan *dbus.Signal,
	conn *dbus.Conn, method string, options map[string]dbus.Variant,
	leading ...interface{}) (map[string]dbus.Variant, error) {

	args := append(append([]interface{}{}, leading...), options)
	var handle dbus.ObjectPath
	if err := obj.CallWithContext(ctx, iface+"."+method, 0, args...).Store(&handle); err != nil {
		return nil, err
	}

	for {
		select {
		case sig := <-replies:
			if sig == nil || sig.Path != handle {
				continue // a reply to some other request on this bus
			}
			if len(sig.Body) < 2 {
				return nil, fmt.Errorf("%s: malformed response", method)
			}
			code, _ := sig.Body[0].(uint32)
			results, _ := sig.Body[1].(map[string]dbus.Variant)
			switch code {
			case 0:
				return results, nil
			case 1:
				return nil, fmt.Errorf("%s was cancelled -- the screen-share prompt was dismissed", method)
			default:
				return nil, fmt.Errorf("%s failed (portal response %d)", method, code)
			}
		case <-ctx.Done():
			// The commonest cause by far, and worth saying rather than making
			// the reader guess: nobody answered the dialog.
			return nil, fmt.Errorf(
				"%s timed out. A \"Select what to share\" window may be waiting for "+
					"an answer; if none ever appears, check custom_picker_binary in "+
					"~/.config/hypr/xdph.conf", method)
		}
	}
}

func subscribeResponses(conn *dbus.Conn) (<-chan *dbus.Signal, error) {
	if err := conn.AddMatchSignal(
		dbus.WithMatchInterface(requestIfc),
		dbus.WithMatchMember("Response"),
	); err != nil {
		return nil, fmt.Errorf("subscribing to portal responses: %w", err)
	}
	ch := make(chan *dbus.Signal, 8)
	conn.Signal(ch)
	return ch, nil
}

// firstStreamNode pulls the node id out of the Start response.
func firstStreamNode(v dbus.Variant) (uint32, error) {
	streams, ok := v.Value().([][]interface{})
	if !ok || len(streams) == 0 {
		return 0, fmt.Errorf("the portal granted no streams")
	}
	id, ok := streams[0][0].(uint32)
	if !ok {
		return 0, fmt.Errorf("the portal's stream carried no node id")
	}
	return id, nil
}

// handleToken returns unique tokens for one handshake. The portal builds the
// Response signal's object path from them, so they must not repeat.
func handleToken() func() string {
	n := 0
	prefix := fmt.Sprintf("castr%d", os.Getpid())
	return func() string {
		n++
		return fmt.Sprintf("%s_%d", strings.ReplaceAll(prefix, "-", ""), n)
	}
}
