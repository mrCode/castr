package chromecast

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mrCode/castr/internal/capture"
)

func TestTheCapturePipelineDiesWithCastrd(t *testing.T) {
	// castrd killed with SIGKILL stops nothing: gst-launch would go on
	// capturing the screen for a receiver nobody controls.
	if dir := os.Getenv("CASTR_CAPTURE_HELPER"); dir != "" {
		// gst-launch-1.0 here is a stand-in that ignores the pipeline.
		os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		fd, err := os.Open(os.DevNull)
		if err != nil {
			os.Exit(3)
		}
		p, err := startCapture(capture.Options{FD: fd, Serial: 1, Container: capture.HLS, Dir: dir})
		if err != nil {
			os.Exit(4)
		}
		os.Stdout.WriteString(strconv.Itoa(p.Pid()) + "\n")
		select {} // until killed
	}
	if runtime.GOOS != "linux" {
		t.Skip("Pdeathsig is Linux's")
	}

	dir := t.TempDir()
	stub := "#!/bin/sh\nexec sleep 30\n"
	if err := os.WriteFile(filepath.Join(dir, "gst-launch-1.0"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	parent := exec.Command(os.Args[0], "-test.run=^TestTheCapturePipelineDiesWithCastrd$")
	parent.Env = append(os.Environ(), "CASTR_CAPTURE_HELPER="+dir)
	out, err := parent.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		_ = parent.Process.Kill()
		t.Fatalf("the stand-in castrd printed no pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	if err := parent.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = parent.Wait()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !exited(pid) {
		time.Sleep(10 * time.Millisecond)
	}
	if !exited(pid) {
		t.Errorf("the pipeline %d still runs 2s after castrd was killed", pid)
	}
}

// exited reports whether pid is gone or a zombie.
func exited(pid int) bool {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	i := strings.LastIndexByte(string(raw), ')')
	return i < 0 || i+2 >= len(raw) || raw[i+2] == 'Z' || raw[i+2] == 'X'
}
