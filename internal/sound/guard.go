package sound

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// GuardFlag starts castrd as a sound guard instead of a daemon.
const GuardFlag = "--sound-guard"

// Guard watches a daemon that has changed the laptop's sound. Every poll it
// checks the state file: gone means the daemon restored cleanly, so Guard
// returns. Otherwise, if the daemon is no longer alive, it calls recover and
// returns. It also returns when ctx ends.
func Guard(ctx context.Context, daemonPID int, stateDir string, alive func(pid int) bool, recover func() error, poll time.Duration) {
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		if _, pending, _ := loadState(stateDir); !pending {
			return
		}
		if !alive(daemonPID) {
			_ = recover()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// StartGuard starts castrd again as a guard for this process, in its own
// session so it outlives the daemon, with no stdio. It is not waited on.
func StartGuard(stateDir string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding castrd to start the sound guard: %w", err)
	}
	cmd := exec.Command(exe, GuardFlag, strconv.Itoa(os.Getpid()), stateDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting the sound guard: %w", err)
	}
	go cmd.Wait() // reaps the guard if it ends while the daemon lives
	return nil
}
