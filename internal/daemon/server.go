package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/mrCode/castr/internal/discovery"
)

// maxRequest bounds one request line. Anything larger is not a castr client.
const maxRequest = 64 * 1024

// clientTimeout keeps a stuck client from holding a connection open forever.
// It is generous because `start` legitimately blocks while a receiver
// handshakes and the user answers a screen-share prompt.
const clientTimeout = 3 * time.Minute

// Serve accepts clients until the daemon is asked to stop.
//
// The caller must already hold the daemon lock: binding the socket is exactly
// the step that lets a second daemon take over, so the lock has to come first.
func (d *Daemon) Serve(ctx context.Context, socketPath string) error {
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		return fmt.Errorf("creating the state directory: %w", err)
	}
	// Only safe because the lock is already held: without it this line is how
	// a second daemon silently displaces the first.
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clearing the old socket: %w", err)
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", socketPath, err)
	}
	// Closing a UnixListener that created its own socket unlinks the file, so
	// there is no explicit Remove here. TestTheSocketIsRemovedOnShutdown holds
	// that behaviour in place: a leftover socket makes the next client connect
	// to nothing instead of spawning a daemon.
	defer listener.Close()

	// The socket carries commands that drive the user's display. Nobody else
	// on a multi-user machine gets to send them.
	if err := os.Chmod(socketPath, 0o600); err != nil {
		return fmt.Errorf("securing the socket: %w", err)
	}

	go func() {
		select {
		case <-d.stopping:
		case <-ctx.Done():
			d.Shutdown()
		}
		listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-d.stopping:
				return nil // a deliberate shutdown, not a failure
			default:
			}
			return fmt.Errorf("accepting: %w", err)
		}
		go d.serveClient(ctx, conn)
	}
}

func (d *Daemon) serveClient(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(clientTimeout))

	reader := bufio.NewReaderSize(conn, maxRequest)
	line, err := reader.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return // the client went away before saying anything
	}

	var req Request
	var resp Response
	if err := json.Unmarshal(line, &req); err != nil {
		resp = Err(fmt.Sprintf("malformed request: %v", err))
	} else {
		resp = d.Handle(ctx, req)
	}

	out, err := json.Marshal(resp)
	if err != nil {
		out, _ = json.Marshal(Err("encoding response"))
	}
	// A client that disconnects before reading the reply is routine -- a UI
	// cancelling a request -- so the write error is not worth reporting.
	_, _ = conn.Write(append(out, '\n'))
}

// Handle answers one request. Exported so tests exercise the commands without
// a socket, and so cmd/castrd can dispatch directly if it ever needs to.
func (d *Daemon) Handle(ctx context.Context, req Request) Response {
	switch req.Cmd {
	case CmdList:
		devices := d.List()
		out := make([]DeviceJSON, 0, len(devices))
		for _, dev := range devices {
			out = append(out, toDeviceJSON(dev))
		}
		return OK(map[string]any{"devices": out})

	case CmdStatus:
		sessions := d.Sessions()
		out := make([]SessionJSON, 0, len(sessions))
		for _, s := range sessions {
			out = append(out, SessionJSON{
				DeviceID: s.Device.ID, Name: s.Device.Name,
				Mode: s.Mode, State: string(s.State), Error: s.Err,
			})
		}
		return OK(map[string]any{"sessions": out})

	case CmdStart:
		if err := d.Start(ctx, req.DeviceID, req.Mode); err != nil {
			return Err(err.Error())
		}
		return OK(map[string]any{"device_id": req.DeviceID, "mode": req.Mode})

	case CmdStop:
		if err := d.Stop(ctx, req.DeviceID); err != nil {
			return Err(err.Error())
		}
		return OK(nil)

	case CmdPin:
		if err := d.SubmitPin(ctx, req.DeviceID, req.Pin); err != nil {
			return Err(err.Error())
		}
		return OK(nil)

	case CmdAdd:
		if req.Device == nil {
			return Err("add needs a device")
		}
		device := fromDeviceJSON(*req.Device)
		if device.ID == "" || device.Address == "" {
			return Err("a manual receiver needs at least an id and an address")
		}
		d.Add(device)
		return OK(map[string]any{"device": toDeviceJSON(device)})

	case CmdForget:
		if !d.Forget(req.DeviceID) {
			return Err(fmt.Sprintf("no manually added receiver with id %s", req.DeviceID))
		}
		return OK(nil)

	case CmdQuit:
		d.Shutdown()
		return OK(nil)

	default:
		return Err(fmt.Sprintf("unknown command: %s", req.Cmd))
	}
}

func toDeviceJSON(d discovery.Device) DeviceJSON {
	return DeviceJSON{ID: d.ID, Name: d.Name, Address: d.Address,
		Port: d.Port, Protocol: d.Protocol, Model: d.Model}
}

func fromDeviceJSON(d DeviceJSON) discovery.Device {
	if d.Protocol == "" {
		d.Protocol = discovery.ProtocolAirPlay
	}
	if d.Port == 0 {
		d.Port = 7000
	}
	if d.Name == "" {
		d.Name = d.ID
	}
	return discovery.Device{ID: d.ID, Name: d.Name, Address: d.Address,
		Port: d.Port, Protocol: d.Protocol, Model: d.Model}
}
