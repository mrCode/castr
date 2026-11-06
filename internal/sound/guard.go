package sound

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// GuardFlag starts castrd as a sound guard instead of a daemon.
const GuardFlag = "--sound-guard"

// GuardTiming is how often Guard checks on its daemon, and how often and for
// how long it retries a recovery that failed.
type GuardTiming struct {
	Poll, Retry, RetryFor time.Duration
}

// Guard watches a daemon that has changed the laptop's sound. Every poll it
// checks the state file: gone means the daemon restored cleanly, and naming
// another daemon means a new castrd owns the routing now; either way Guard
// returns. Otherwise, once the daemon is dead, it calls recover until that
// succeeds, retrying for a while -- a single pactl failure must not leave the
// laptop silent -- and returns the last error if it never does. It also
// returns when ctx ends.
func Guard(ctx context.Context, daemonPID int, stateDir string, alive func(pid int) bool, recover func() error, timing GuardTiming) error {
	ours := func() bool {
		saved, pending, _ := loadState(stateDir)
		// A state file with no pid predates the pid, or is unreadable: the
		// routing it records is treated as this daemon's.
		return pending && (saved.DaemonPID == 0 || saved.DaemonPID == daemonPID)
	}
	wait := func(d time.Duration) bool {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return true
		}
	}

	for {
		if !ours() {
			return nil
		}
		if !alive(daemonPID) {
			break
		}
		if !wait(timing.Poll) {
			return nil
		}
	}
	giveUp := time.Now().Add(timing.RetryFor)
	for {
		err := recover()
		if err == nil {
			return nil
		}
		if !time.Now().Before(giveUp) || !wait(timing.Retry) {
			return err
		}
		if !ours() {
			return nil
		}
	}
}

// ProcessAlive reports whether pid is a live process. A zombie is dead: it
// has exited, though kill(pid, 0) still finds it until its parent reaps it.
func ProcessAlive(pid int) bool {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false
		}
		return syscall.Kill(pid, 0) == nil
	}
	// pid (comm) state ...; comm may itself hold spaces and parentheses.
	i := bytes.LastIndexByte(raw, ')')
	if i < 0 || i+2 >= len(raw) {
		return syscall.Kill(pid, 0) == nil
	}
	switch raw[i+2] {
	case 'Z', 'X', 'x':
		return false
	}
	return true
}

// StartGuard starts castrd again as a guard for this process, in its own
// session so it outlives the daemon. Its stderr is the daemon's, so a
// recovery that fails for good says why where the daemon's own log goes. It
// is not waited on.
func StartGuard(stateDir string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding castrd to start the sound guard: %w", err)
	}
	cmd := exec.Command(exe, GuardFlag, strconv.Itoa(os.Getpid()), stateDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting the sound guard: %w", err)
	}
	go cmd.Wait() // reaps the guard if it ends while the daemon lives
	return nil
}
