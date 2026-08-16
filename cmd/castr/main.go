// Command castr casts this screen to an AirPlay receiver.
//
// This is the client. It talks to a background daemon over a unix socket and
// starts one if none is listening. Everything with an effect on the outside
// world -- looking up programs, running them, spawning the daemon -- is wired
// up here and injected into the packages below, which is why those packages
// can be tested without a display, a network, or a receiver.
package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/mrCode/castr/internal/cli"
	"github.com/mrCode/castr/internal/client"
	"github.com/mrCode/castr/internal/daemon"
	"github.com/mrCode/castr/internal/picker"
)

// DaemonBinary is the program that does the work.
const DaemonBinary = "castrd"

// version is set at build time with -X main.version. The default says so
// plainly rather than claiming a release number nobody stamped.
var version = "dev"

func main() {
	socket := daemon.DefaultSocketPath()

	app := &cli.App{
		Client: client.New(socket, spawnDaemon),
		Picker: picker.Picker{Look: exec.LookPath, Exec: runMenu},
		Out:    os.Stdout,
		Err:    os.Stderr,
		Socket: socket,
	}
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Println("castr", version)
		return
	}
	os.Exit(app.Run(os.Args[1:]))
}

// runMenu executes a menu program and returns its stdout.
func runMenu(argv []string, stdin string) (string, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	if stdin != "" {
		cmd.Stdin = bytes.NewBufferString(stdin)
	}
	// Stderr passes through: a menu that complains should be visible in the
	// journal rather than swallowed.
	cmd.Stderr = os.Stderr

	out, err := cmd.Output()
	return string(out), err
}

// spawnDaemon starts castrd detached from this process.
//
// Setsid matters: without it the daemon shares this client's process group, so
// the Ctrl-C or the terminal close that ends the client also kills the daemon
// -- and takes every live cast with it.
func spawnDaemon() error {
	binary, err := exec.LookPath(DaemonBinary)
	if err != nil {
		// Look beside this binary too, so a build directory works without
		// installing anything.
		if self, serr := os.Executable(); serr == nil {
			candidate := filepath.Join(filepath.Dir(self), DaemonBinary)
			if _, statErr := os.Stat(candidate); statErr == nil {
				binary = candidate
			}
		}
	}
	if binary == "" {
		return fmt.Errorf("%s is not installed", DaemonBinary)
	}

	cmd := exec.Command(binary)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", DaemonBinary, err)
	}
	// Not waited on: it outlives this process by design. The kernel reparents
	// it to init, so there is no zombie to reap.
	return nil
}
