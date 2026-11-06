// Package proc starts the children that must not outlive castrd: the
// AirPlay helper and the Chromecast capture pipeline. castrd stops them
// itself on a stop or a clean exit; when it is killed outright -- SIGKILL,
// the OOM killer, a crash -- only the kernel is left to tell them, and only
// a child that asked to be told is.
package proc

import (
	"os/exec"
	"runtime"
	"syscall"
)

// Attr is the children's SysProcAttr. Setpgid gives the child its own process
// group, which castrd signals as a whole on a stop. Pdeathsig has the kernel
// send it SIGTERM when its parent goes; Start keeps that parent alive for as
// long as the child runs.
func Attr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
}

// Start starts cmd with Pdeathsig set to SIGTERM and returns a channel that
// gets Wait's result once it has exited. It must be Wait's only caller.
//
// Pdeathsig fires when the OS THREAD that started the child exits, not the
// process (golang/go#27505), and Go ends a thread whenever a goroutine
// locked to it returns. So the child is started from a goroutine locked to
// its own thread, which then waits for it: that thread lives exactly as
// long as the child, and only castrd's death ends it early.
func Start(cmd *exec.Cmd) (<-chan error, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGTERM

	started := make(chan error, 1)
	exited := make(chan error, 1)
	go func() {
		// Never unlocked: when this returns, the child is gone, and the
		// thread may end with it.
		runtime.LockOSThread()
		if err := cmd.Start(); err != nil {
			started <- err
			return
		}
		started <- nil
		exited <- cmd.Wait()
	}()
	if err := <-started; err != nil {
		return nil, err
	}
	return exited, nil
}
