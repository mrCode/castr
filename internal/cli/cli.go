// Package cli is castr's command-line and menu front end.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/mrCode/castr/internal/client"
	"github.com/mrCode/castr/internal/daemon"
	"github.com/mrCode/castr/internal/picker"
	"github.com/mrCode/castr/internal/session"
	"github.com/mrCode/castr/internal/ui"
)

// App holds what the commands need. Every effect is a field so the whole CLI
// is testable without a daemon, a menu program, or a display.
type App struct {
	Client *client.Client
	Picker picker.Picker
	Out    io.Writer
	Err    io.Writer

	// Socket is only needed by the status command, which deliberately does not
	// go through Client -- see client.SessionsQuietly.
	Socket string
}

// Usage is printed for `castr help` and for an unknown command.
const Usage = `castr — cast this screen to an AirPlay receiver

  castr menu                 pick a receiver and a mode (the keybind uses this)
  castr list                 list the receivers found
  castr start <id> [mode]    cast to a receiver; mode is mirror (default) or extend
  castr stop [id]            stop casting; with no id, stop everything
  castr status               show what is casting
  castr bar                  one line of JSON for the bar indicator
  castr pin <id> <code>      send a pairing code
  castr add <address> [name] register a receiver mDNS cannot see
  castr forget <id>          drop a registered receiver
  castr quit                 stop the background daemon
`

// Run dispatches one command and returns the process exit code.
func (a *App) Run(args []string) int {
	if len(args) == 0 {
		return a.fail(Usage)
	}

	switch args[0] {
	case "menu":
		return a.menu()
	case "list":
		return a.list()
	case "start":
		return a.start(args[1:])
	case "stop":
		return a.stop(args[1:])
	case "status":
		return a.status()
	case "bar", "waybar":
		return a.bar()
	case "pin":
		return a.pin(args[1:])
	case "add":
		return a.add(args[1:])
	case "forget":
		return a.forget(args[1:])
	case "quit":
		return a.run(a.Client.Quit)
	case "help", "-h", "--help":
		fmt.Fprint(a.Out, Usage)
		return 0
	default:
		return a.fail(fmt.Sprintf("unknown command: %s\n\n%s", args[0], Usage))
	}
}

func (a *App) fail(msg string) int {
	fmt.Fprintln(a.Err, strings.TrimRight(msg, "\n"))
	return 1
}

func (a *App) run(f func() error) int {
	if err := f(); err != nil {
		return a.fail(err.Error())
	}
	return 0
}

func (a *App) list() int {
	devices, err := a.Client.Devices()
	if err != nil {
		return a.fail(err.Error())
	}
	if len(devices) == 0 {
		// Said out loud rather than printed as an empty list, because an empty
		// list looks identical to a command that silently did nothing.
		fmt.Fprintln(a.Out, "No receivers found.")
		return 0
	}
	for _, d := range devices {
		model := ""
		if d.Model != "" {
			model = "  " + d.Model
		}
		fmt.Fprintf(a.Out, "%-28s %-16s %s%s\n", d.Name, d.Address, d.ID, model)
	}
	return 0
}

func (a *App) start(args []string) int {
	if len(args) == 0 {
		return a.fail("start needs a receiver id (see: castr list)")
	}
	mode := session.ModeMirror
	if len(args) > 1 {
		mode = args[1]
	}
	if !session.ValidMode(mode) {
		return a.fail(fmt.Sprintf("unknown mode: %s; expected %s or %s",
			mode, session.ModeMirror, session.ModeExtend))
	}
	return a.run(func() error { return a.Client.Start(args[0], mode) })
}

func (a *App) stop(args []string) int {
	if len(args) > 0 {
		return a.run(func() error { return a.Client.Stop(args[0]) })
	}

	// No id: stop whatever is casting. This is what the bar's right-click and
	// the menu's stop entry both want, and neither knows a device id.
	sessions, err := a.Client.Sessions()
	if err != nil {
		return a.fail(err.Error())
	}
	if len(sessions) == 0 {
		fmt.Fprintln(a.Out, "Nothing is casting.")
		return 0
	}
	code := 0
	for _, s := range sessions {
		// Every session is attempted even if one fails, so a single stuck
		// receiver cannot strand the others.
		if err := a.Client.Stop(s.DeviceID); err != nil {
			code = a.fail(fmt.Sprintf("stopping %s: %v", s.Name, err))
		}
	}
	return code
}

func (a *App) status() int {
	sessions, err := a.Client.Sessions()
	if err != nil {
		return a.fail(err.Error())
	}
	if len(sessions) == 0 {
		fmt.Fprintln(a.Out, "Not casting.")
		return 0
	}
	for _, s := range sessions {
		line := fmt.Sprintf("%s  %s  %s", s.Name, s.Mode, s.State)
		if s.Error != "" {
			line += "  " + s.Error
		}
		fmt.Fprintln(a.Out, line)
	}
	return 0
}

// bar prints the indicator payload.
//
// It never spawns a daemon and never fails: the bar polls it several times a
// minute, so spawning would keep a daemon alive forever and defeat the idle
// timeout, and a non-zero exit would show an error indicator for the entirely
// normal state of not casting.
func (a *App) bar() int {
	out, err := json.Marshal(ui.Render(client.SessionsQuietly(a.Socket)))
	if err != nil {
		return a.fail(err.Error())
	}
	fmt.Fprintln(a.Out, string(out))
	return 0
}

func (a *App) pin(args []string) int {
	if len(args) < 2 {
		return a.fail("pin needs a receiver id and a code")
	}
	return a.run(func() error { return a.Client.SubmitPin(args[0], args[1]) })
}

func (a *App) add(args []string) int {
	if len(args) == 0 {
		return a.fail("add needs an address")
	}
	device := manualDevice(args[0], strings.Join(args[1:], " "))
	if err := a.Client.Add(device); err != nil {
		return a.fail(err.Error())
	}
	fmt.Fprintf(a.Out, "Added %s (%s)\n", device.Name, device.ID)
	return 0
}

func (a *App) forget(args []string) int {
	if len(args) == 0 {
		return a.fail("forget needs a receiver id")
	}
	return a.run(func() error { return a.Client.Forget(args[0]) })
}

// manualDevice builds a receiver from an address the user typed.
//
// The id embeds the address so re-adding the same receiver updates it rather
// than accumulating duplicates -- the menu once listed the same Apple TV four
// times because every add made a new id.
func manualDevice(address, name string) daemon.DeviceJSON {
	address = strings.TrimSpace(address)
	if name == "" {
		name = address
	}
	return daemon.DeviceJSON{
		ID:       "airplay:manual:" + address,
		Name:     name,
		Address:  address,
		Port:     7000,
		Protocol: "airplay",
	}
}
