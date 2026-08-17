package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mrCode/castr/internal/backend/airplay"
	"github.com/mrCode/castr/internal/config"
)

// termGrace is how long a child gets to exit after SIGTERM before SIGKILL.
const termGrace = 3 * time.Second

// drainGrace is how long the scanner gets to read what a dead child left in
// the pipe before the reader is forced closed. Its last lines are the ones
// that say why the cast failed.
const drainGrace = 500 * time.Millisecond

// child is a real doubletake process.
type child struct {
	cmd    *exec.Cmd
	output io.ReadCloser
	input  io.WriteCloser
	done   chan struct{}
}

func (c *child) Read(b []byte) (int, error) { return c.output.Read(b) }

func (c *child) Write(b []byte) (int, error) {
	if c.input == nil {
		return 0, fmt.Errorf("no stdin")
	}
	return c.input.Write(b)
}

// Terminate signals the process GROUP, not the process.
//
// This is the whole reason for Setpgid below. doubletake spawns GStreamer
// capture pipelines; signalling only doubletake leaves them running, reparented
// to init, still holding a portal node and the GPU. Five accumulated during one
// bad session before this was fixed, and the only way out was to find and kill
// them by hand.
//
// The negative pid is what makes it a group signal.
func (c *child) Terminate() error {
	if c.cmd.Process == nil {
		return nil
	}
	pgid := -c.cmd.Process.Pid

	if err := syscall.Kill(pgid, syscall.SIGTERM); err != nil {
		// ESRCH means it is already gone, which is the outcome we wanted.
		if err != syscall.ESRCH {
			return fmt.Errorf("terminating the process group: %w", err)
		}
		return nil
	}

	select {
	case <-c.done:
		return nil
	case <-time.After(termGrace):
		// It ignored SIGTERM. A capture pipeline holding the GPU is worse than
		// an ungraceful exit.
		_ = syscall.Kill(pgid, syscall.SIGKILL)
		return nil
	}
}

func (c *child) Wait() error {
	<-c.done
	return nil
}

// spawner returns a Spawner that starts doubletake in its own process group.
func spawner(cfg config.Config, stateDir string) airplay.Spawner {
	return func(ctx context.Context, argv []string) (airplay.Process, error) {
		cmd := exec.Command(argv[0], argv[1:]...)

		// Setpgid is load-bearing, not hygiene: without it Terminate's negative
		// pid signals OUR OWN process group -- the daemon included.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Env = childEnv(cfg, stateDir)

		// Plain os.Pipe rather than cmd.StdoutPipe: os/exec closes the pipes it
		// owns as part of cmd.Wait, and "it is incorrect to call Wait before
		// all reads have completed". Waiting in a goroutine while the scanner
		// reads is exactly that race, and what it truncates is the child's
		// LAST output -- the error text castr shows the user to explain why a
		// cast failed.
		outR, outW, err := os.Pipe()
		if err != nil {
			return nil, fmt.Errorf("capturing output: %w", err)
		}
		// Merged: doubletake prints the ready marker on one stream and its
		// capture errors on the other, and the scanner needs both.
		cmd.Stdout = outW
		cmd.Stderr = outW

		inR, inW, err := os.Pipe()
		if err != nil {
			outR.Close()
			outW.Close()
			return nil, fmt.Errorf("opening stdin: %w", err)
		}
		cmd.Stdin = inR

		if err := cmd.Start(); err != nil {
			outR.Close()
			outW.Close()
			inR.Close()
			inW.Close()
			return nil, err
		}
		// The child holds the only remaining writer, so the reader sees EOF
		// when the whole process group is gone -- not when we stop waiting.
		outW.Close()
		inR.Close()

		c := &child{cmd: cmd, output: outR, input: inW, done: make(chan struct{})}
		go func() {
			_ = cmd.Wait()
			close(c.done)

			// A capture pipeline that survived its parent would hold the write
			// end open and the reader would block forever, so the crash would
			// never be announced. Give the scanner a moment to drain, then
			// force it to unblock.
			time.AfterFunc(drainGrace, func() { outR.Close() })
		}()
		return c, nil
	}
}

// childEnv builds doubletake's environment.
func childEnv(cfg config.Config, stateDir string) []string {
	env := os.Environ()

	if cfg.AirPlay.Code != "" {
		env = append(env, "DOUBLETAKE_CODE="+cfg.AirPlay.Code)
	}
	if cfg.AirPlay.HideVAPostproc {
		if dir, err := vapostprocShim(stateDir); err == nil {
			env = append(env, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		}
	}
	return env
}

// vapostprocShim builds a PATH directory that reports vapostproc as missing.
//
// doubletake probes with `gst-inspect-1.0 vapostproc` and uses the element if
// it is there. On Hyprland the portal's DMA-BUF is padded -- 16 MiB for a
// 2560x1600 RGBA frame against a 15.6 MiB descriptor -- and GStreamer's VA
// allocator refuses it, so the receiver shows a black screen with no error
// anywhere. Hiding the element makes doubletake fall back to videoconvert,
// which works and costs some CPU.
func vapostprocShim(stateDir string) (string, error) {
	dir := filepath.Join(stateDir, "shim")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}

	script := "#!/bin/sh\n" +
		"# Generated by castr. Reports vapostproc as unavailable so doubletake\n" +
		"# falls back to videoconvert; the VA path shows a black screen on Hyprland.\n" +
		"[ \"$1\" = vapostproc ] && exit 1\n" +
		"exec /usr/bin/gst-inspect-1.0 \"$@\"\n"

	path := filepath.Join(dir, "gst-inspect-1.0")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		return "", err
	}
	return dir, nil
}
