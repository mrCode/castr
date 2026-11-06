package sound

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeStateFile(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, StateFile), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTheGuardRestoresWhenTheDaemonDies(t *testing.T) {
	dir := t.TempDir()
	writeStateFile(t, dir)
	polls, recovered := 0, 0
	alive := func(int) bool { polls++; return polls <= 2 }
	Guard(context.Background(), 42, dir, alive, func() error { recovered++; return nil }, time.Millisecond)
	if recovered != 1 {
		t.Fatalf("recover called %d times, want 1", recovered)
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
	Guard(context.Background(), 42, dir, alive, func() error { recovered++; return nil }, time.Millisecond)
	if recovered != 0 {
		t.Fatalf("recover called %d times, want 0", recovered)
	}
}

func TestTheGuardStopsWhenCancelled(t *testing.T) {
	dir := t.TempDir()
	writeStateFile(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	Guard(ctx, 42, dir, func(int) bool { return true }, func() error { t.Error("recovered"); return nil }, time.Millisecond)
}
