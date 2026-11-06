package proc

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestAttrTiesAChildToCastrd(t *testing.T) {
	a := Attr()
	if a.Pdeathsig != syscall.SIGTERM {
		t.Errorf("Pdeathsig = %v, want SIGTERM", a.Pdeathsig)
	}
	if !a.Setpgid {
		t.Error("Setpgid is off: castrd could no longer stop the child's whole group")
	}
}

func TestStartSetsTheDeathSignal(t *testing.T) {
	cmd := exec.Command("true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	exited, err := Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	<-exited
	if cmd.SysProcAttr.Pdeathsig != syscall.SIGTERM || !cmd.SysProcAttr.Setpgid {
		t.Errorf("SysProcAttr %+v, want Pdeathsig SIGTERM and Setpgid kept", cmd.SysProcAttr)
	}
}

func TestStartReportsTheExit(t *testing.T) {
	exited, err := Start(exec.Command("sh", "-c", "exit 3"))
	if err != nil {
		t.Fatal(err)
	}
	var exit *exec.ExitError
	if err := <-exited; !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Errorf("exit %v, want status 3", err)
	}
}

func TestStartReportsAFailedStart(t *testing.T) {
	if _, err := Start(exec.Command("/nonexistent/castr-helper")); err == nil {
		t.Error("a missing binary started")
	}
}

// gone reports whether pid has exited: no longer there, or a zombie.
func gone(pid int) bool {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	i := strings.LastIndexByte(string(raw), ')')
	return i < 0 || i+2 >= len(raw) || raw[i+2] == 'Z' || raw[i+2] == 'X'
}

func waitGone(pid int, limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if gone(pid) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return gone(pid)
}

func TestAChildDiesWithItsParent(t *testing.T) {
	// castrd killed with SIGKILL runs no cleanup: only the kernel can tell
	// its children, and only if each asked to be told.
	if os.Getenv("CASTR_PROC_HELPER") != "" {
		cmd := exec.Command("sleep", "30")
		cmd.SysProcAttr = Attr()
		if _, err := Start(cmd); err != nil {
			os.Exit(3)
		}
		os.Stdout.WriteString(strconv.Itoa(cmd.Process.Pid) + "\n")
		select {} // until killed
	}
	if runtime.GOOS != "linux" {
		t.Skip("Pdeathsig is Linux's")
	}

	helper := exec.Command(os.Args[0], "-test.run=^TestAChildDiesWithItsParent$")
	helper.Env = append(os.Environ(), "CASTR_PROC_HELPER=1")
	out, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		_ = helper.Process.Kill()
		t.Fatalf("the helper printed no pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })

	if err := helper.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = helper.Wait()
	if !waitGone(pid, 2*time.Second) {
		t.Errorf("the child %d still runs 2s after its parent was killed", pid)
	}
}

func TestAChildOutlivesTheThreadThatAskedForIt(t *testing.T) {
	// Pdeathsig fires when the thread that started the child exits
	// (golang/go#27505), and Go ends a thread whose locked goroutine
	// returns. The child must not die with its caller's thread.
	type result struct {
		cmd *exec.Cmd
		err error
	}
	got := make(chan result)
	go func() {
		runtime.LockOSThread() // never unlocked: the thread ends with the goroutine
		cmd := exec.Command("sleep", "30")
		_, err := Start(cmd)
		got <- result{cmd, err}
	}()
	r := <-got
	if r.err != nil {
		t.Fatal(r.err)
	}
	pid := r.cmd.Process.Pid
	defer func() { _ = syscall.Kill(pid, syscall.SIGKILL) }()
	if waitGone(pid, 500*time.Millisecond) {
		t.Error("the child died with the thread of the goroutine that started it")
	}
}
