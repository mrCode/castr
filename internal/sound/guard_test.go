package sound

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeStateFile(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, StateFile), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
}

var fast = GuardTiming{Poll: time.Millisecond, Retry: time.Millisecond, RetryFor: time.Second}

func TestTheGuardRestoresWhenTheDaemonDies(t *testing.T) {
	dir := t.TempDir()
	writeStateFile(t, dir)
	polls, recovered := 0, 0
	alive := func(int) bool { polls++; return polls <= 2 }
	recover := func() error { recovered++; os.Remove(filepath.Join(dir, StateFile)); return nil }
	if err := Guard(context.Background(), 42, dir, alive, recover, fast); err != nil {
		t.Fatal(err)
	}
	if recovered != 1 {
		t.Fatalf("recover called %d times, want 1", recovered)
	}
}

func TestTheGuardRetriesAFailedRecovery(t *testing.T) {
	// One pactl failure must not leave the laptop silent with nothing
	// retrying.
	dir := t.TempDir()
	writeStateFile(t, dir)
	recovered := 0
	recover := func() error {
		recovered++
		if recovered < 3 {
			return errors.New("pactl: Connection refused")
		}
		os.Remove(filepath.Join(dir, StateFile))
		return nil
	}
	if err := Guard(context.Background(), 42, dir, func(int) bool { return false }, recover, fast); err != nil {
		t.Fatal(err)
	}
	if recovered != 3 {
		t.Fatalf("recover called %d times, want 3", recovered)
	}
}

func TestTheGuardGivesUpAfterAWhileAndReportsWhy(t *testing.T) {
	dir := t.TempDir()
	writeStateFile(t, dir)
	recovered := 0
	recover := func() error { recovered++; return errors.New("pactl: Connection refused") }
	timing := GuardTiming{Poll: time.Millisecond, Retry: 5 * time.Millisecond, RetryFor: 50 * time.Millisecond}
	start := time.Now()
	err := Guard(context.Background(), 42, dir, func(int) bool { return false }, recover, timing)
	if err == nil || !strings.Contains(err.Error(), "Connection refused") {
		t.Fatalf("Guard = %v, want the last recovery error", err)
	}
	if recovered < 2 {
		t.Errorf("recover called %d times, want retries", recovered)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the guard kept going for %v", took)
	}
}

func TestTheGuardLeavesAnotherDaemonsRoutingAlone(t *testing.T) {
	// A new castrd took over within one poll of the old one dying: its
	// routing is not the dead daemon's to recover.
	dir := t.TempDir()
	if err := saveState(dir, savedState{PreviousDefault: "speakers", DaemonPID: 77}); err != nil {
		t.Fatal(err)
	}
	err := Guard(context.Background(), 42, dir, func(int) bool { return false },
		func() error { t.Error("recovered another daemon's routing"); return nil }, fast)
	if err != nil {
		t.Fatal(err)
	}
}

func TestTheGuardStopsRetryingOnceAnotherDaemonTakesOver(t *testing.T) {
	dir := t.TempDir()
	if err := saveState(dir, savedState{PreviousDefault: "speakers", DaemonPID: 42}); err != nil {
		t.Fatal(err)
	}
	recovered := 0
	recover := func() error {
		recovered++
		// Meanwhile a new daemon recovered it and started a cast of its own.
		if err := saveState(dir, savedState{PreviousDefault: "speakers", DaemonPID: 77}); err != nil {
			t.Fatal(err)
		}
		return errors.New("pactl: No such entity")
	}
	if err := Guard(context.Background(), 42, dir, func(int) bool { return false }, recover, fast); err != nil {
		t.Fatal(err)
	}
	if recovered != 1 {
		t.Fatalf("recover called %d times, want 1", recovered)
	}
}

func TestAZombieDaemonIsDead(t *testing.T) {
	if !ProcessAlive(os.Getpid()) {
		t.Fatal("this process is not alive")
	}
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	// Not reaped yet: a zombie, which kill(pid, 0) still finds.
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", cmd.Process.Pid))
		if strings.Contains(string(raw), ") Z ") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the child never became a zombie: %q", raw)
		}
		time.Sleep(time.Millisecond)
	}
	if ProcessAlive(cmd.Process.Pid) {
		t.Error("a zombie counts as alive")
	}
	cmd.Wait()
	if ProcessAlive(cmd.Process.Pid) {
		t.Error("a reaped process counts as alive")
	}
}

func TestTheGuardLeavesWhenTheDaemonRestoredItself(t *testing.T) {
	dir := t.TempDir()
	writeStateFile(t, dir)
	polls, recovered := 0, 0
	alive := func(int) bool {
		polls++
		if polls == 2 {
			os.Remove(filepath.Join(dir, StateFile))
		}
		return true
	}
	if err := Guard(context.Background(), 42, dir, alive, func() error { recovered++; return nil }, fast); err != nil {
		t.Fatal(err)
	}
	if recovered != 0 {
		t.Fatalf("recover called %d times, want 0", recovered)
	}
}

func TestTheGuardStopsWhenCancelled(t *testing.T) {
	dir := t.TempDir()
	writeStateFile(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = Guard(ctx, 42, dir, func(int) bool { return true }, func() error { t.Error("recovered"); return nil }, fast)
}
