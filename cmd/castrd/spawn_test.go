package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mrCode/castr/internal/config"
)

// alive reports whether a pid is still running. Signal 0 checks without sending.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

func waitGone(pid int, limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return !alive(pid)
}

// stubDoubletake writes a script that behaves like doubletake in the one way
// that matters here: it spawns a long-lived CHILD of its own, the way
// doubletake spawns GStreamer capture pipelines.
func stubDoubletake(t *testing.T, pidFile string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "doubletake-stub")
	script := fmt.Sprintf(`#!/bin/sh
# A grandchild that outlives its parent unless the whole GROUP is signalled.
sleep 600 &
echo $! > %q
echo "mirror session ready"
echo "screen capture started"
# Ignore SIGTERM aimed at us alone, so only a group signal can end this tree.
sleep 600
`, pidFile)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTerminateKillsTheWholeProcessGroup(t *testing.T) {
	// The rule no fake can check. doubletake spawns GStreamer capture
	// pipelines; signalling only doubletake leaves them running, reparented to
	// init, still holding a portal node and the GPU. Five accumulated during
	// one bad session, and the only way out was killing them by hand.
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	stub := stubDoubletake(t, pidFile)

	spawn := spawner(config.Default(), t.TempDir())
	proc, err := spawn(context.Background(), []string{stub})
	if err != nil {
		t.Fatal(err)
	}

	// Read the output so the stub gets past its writes, and wait for the
	// grandchild's pid to appear.
	go func() {
		buf := make([]byte, 256)
		for {
			if _, err := proc.Read(buf); err != nil {
				return
			}
		}
	}()

	grandchild := 0
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && grandchild == 0 {
		if raw, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				grandchild = pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if grandchild == 0 {
		t.Fatal("the stub never reported a grandchild pid")
	}
	if !alive(grandchild) {
		t.Fatal("the grandchild was never running")
	}

	if err := proc.Terminate(); err != nil {
		t.Fatal(err)
	}

	if !waitGone(grandchild, 10*time.Second) {
		// Clean up before failing, so a bad run does not leave a sleep(600)
		// behind on the developer's machine.
		syscall.Kill(grandchild, syscall.SIGKILL)
		t.Error("the grandchild outlived Terminate -- capture pipelines would survive a stop")
	}
}

func TestTerminateEndsAChildThatIgnoresSigterm(t *testing.T) {
	// A capture pipeline holding the GPU is worse than an ungraceful exit.
	dir := t.TempDir()
	path := filepath.Join(dir, "stubborn")
	script := "#!/bin/sh\ntrap '' TERM\necho started\nsleep 600\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	spawn := spawner(config.Default(), t.TempDir())
	proc, err := spawn(context.Background(), []string{path})
	if err != nil {
		t.Fatal(err)
	}
	c := proc.(*child)

	buf := make([]byte, 64)
	if _, err := proc.Read(buf); err != nil {
		t.Fatal(err)
	}
	pid := c.cmd.Process.Pid

	start := time.Now()
	if err := proc.Terminate(); err != nil {
		t.Fatal(err)
	}

	if !waitGone(pid, 5*time.Second) {
		syscall.Kill(-pid, syscall.SIGKILL)
		t.Fatal("a child ignoring SIGTERM was never killed")
	}
	if elapsed := time.Since(start); elapsed > termGrace+3*time.Second {
		t.Errorf("took %v to give up on SIGTERM, want about %v", elapsed, termGrace)
	}
}

func TestTheChildRunsInItsOwnProcessGroup(t *testing.T) {
	// Without Setpgid, Terminate's negative pid signals OUR OWN group -- the
	// daemon and every other cast included.
	spawn := spawner(config.Default(), t.TempDir())
	proc, err := spawn(context.Background(), []string{"/bin/sh", "-c", "echo up; sleep 30"})
	if err != nil {
		t.Fatal(err)
	}
	c := proc.(*child)
	defer proc.Terminate()

	buf := make([]byte, 16)
	if _, err := proc.Read(buf); err != nil {
		t.Fatal(err)
	}

	pgid, err := syscall.Getpgid(c.cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if pgid == syscall.Getpgrp() {
		t.Error("the child shares the daemon's process group; terminating it would kill us")
	}
	if pgid != c.cmd.Process.Pid {
		t.Errorf("pgid = %d, want it to lead its own group (%d)", pgid, c.cmd.Process.Pid)
	}
}

func TestTerminatingAnAlreadyDeadChildIsNotAnError(t *testing.T) {
	// It is the normal case after a crash, and `stop` must not report a
	// failure for a cast that is already gone.
	spawn := spawner(config.Default(), t.TempDir())
	proc, err := spawn(context.Background(), []string{"/bin/sh", "-c", "exit 0"})
	if err != nil {
		t.Fatal(err)
	}
	proc.Wait()

	if err := proc.Terminate(); err != nil {
		t.Errorf("terminating a dead child: %v", err)
	}
}

func TestStderrIsMergedIntoTheOutputStream(t *testing.T) {
	// doubletake prints the ready marker on one stream and its capture errors
	// on the other. Reading only stdout loses every diagnosis.
	spawn := spawner(config.Default(), t.TempDir())
	proc, err := spawn(context.Background(),
		[]string{"/bin/sh", "-c", "echo to-stderr >&2; sleep 0.1"})
	if err != nil {
		t.Fatal(err)
	}
	defer proc.Terminate()

	buf := make([]byte, 128)
	n, err := proc.Read(buf)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(buf[:n]), "to-stderr") {
		t.Errorf("read %q, want stderr merged in", buf[:n])
	}
}

func TestThePinCanBeWrittenToTheChild(t *testing.T) {
	spawn := spawner(config.Default(), t.TempDir())
	proc, err := spawn(context.Background(), []string{"/bin/sh", "-c", "read pin; echo got:$pin"})
	if err != nil {
		t.Fatal(err)
	}
	defer proc.Terminate()

	if _, err := proc.Write([]byte("1234\n")); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 64)
	n, err := proc.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(buf[:n]), "got:1234") {
		t.Errorf("child read %q, want the PIN", buf[:n])
	}
}

func TestTheVapostprocShimHidesOnlyVapostproc(t *testing.T) {
	// Hiding every element would break the whole pipeline; the VA path is the
	// one that shows a black screen on Hyprland.
	dir, err := vapostprocShim(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(dir, "gst-inspect-1.0")

	if err := runShim(shim, "vapostproc"); err == nil {
		t.Error("the shim reported vapostproc as available")
	}
	if _, err := os.Stat("/usr/bin/gst-inspect-1.0"); err != nil {
		t.Skip("gst-inspect-1.0 is not installed; cannot check the pass-through")
	}
	if err := runShim(shim, "videoconvert"); err != nil {
		t.Errorf("the shim hid videoconvert too: %v", err)
	}
}

func runShim(path string, element string) error {
	proc, err := spawner(config.Default(), os.TempDir())(context.Background(),
		[]string{path, element})
	if err != nil {
		return err
	}
	buf := make([]byte, 4096)
	for {
		if _, err := proc.Read(buf); err != nil {
			break
		}
	}
	c := proc.(*child)
	<-c.done
	if c.cmd.ProcessState != nil && !c.cmd.ProcessState.Success() {
		return fmt.Errorf("exit %d", c.cmd.ProcessState.ExitCode())
	}
	return nil
}
