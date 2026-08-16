package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// SocketFilename lives in the state directory beside the lock.
const SocketFilename = "daemon.sock"

// SocketPath returns the socket inside a state directory.
func SocketPath(stateDir string) string { return filepath.Join(stateDir, SocketFilename) }

// StateDir is where the lock, the socket, and the portal credentials live.
func StateDir() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join(os.TempDir(), "castr")
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "castr")
}

// Commands the daemon answers.
const (
	CmdList   = "list"
	CmdStatus = "status"
	CmdStart  = "start"
	CmdStop   = "stop"
	CmdPin    = "pin"
	CmdAdd    = "add"
	CmdForget = "forget"
	CmdQuit   = "quit"
)

// Request is one command. Clients send exactly one per connection, as a single
// JSON line.
type Request struct {
	Cmd      string      `json:"cmd"`
	DeviceID string      `json:"device_id,omitempty"`
	Mode     string      `json:"mode,omitempty"`
	Pin      string      `json:"pin,omitempty"`
	Device   *DeviceJSON `json:"device,omitempty"`
}

// Response is the daemon's answer.
//
// Ok is explicit rather than inferred from Error being empty: a response that
// fails to decode, or arrives truncated, then reads as failure instead of as
// silent success.
type Response struct {
	Ok    bool            `json:"ok"`
	Error string          `json:"error,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// DeviceJSON is a receiver on the wire.
type DeviceJSON struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Address  string `json:"address"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
	Model    string `json:"model,omitempty"`
}

// SessionJSON is a live cast on the wire.
type SessionJSON struct {
	DeviceID string `json:"device_id"`
	Name     string `json:"name"`
	Mode     string `json:"mode"`
	State    string `json:"state"`
	Error    string `json:"error,omitempty"`
}

// OK builds a successful response around any payload.
func OK(payload any) Response {
	if payload == nil {
		return Response{Ok: true}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return Err(fmt.Sprintf("encoding response: %v", err))
	}
	return Response{Ok: true, Data: raw}
}

// Err builds a failure response.
func Err(message string) Response { return Response{Ok: false, Error: message} }
