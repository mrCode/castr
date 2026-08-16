package daemon

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestASecondDaemonIsRefusedWhileTheFirstHoldsTheLock(t *testing.T) {
	path := LockPath(t.TempDir())

	first, err := Acquire(path, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()

	// flock is per open file description, so a second Acquire in this same
	// process is a genuine second contender.
	_, err = Acquire(path, 50*time.Millisecond)

	if !errors.Is(err, ErrLocked) {
		t.Fatalf("second Acquire err = %v, want ErrLocked", err)
	}
}

func TestReleasingLetsTheNextDaemonIn(t *testing.T) {
	path := LockPath(t.TempDir())

	first, err := Acquire(path, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}

	second, err := Acquire(path, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}
	second.Release()
}

func TestAcquireWaitsForADepartingDaemon(t *testing.T) {
	// A client spawns a daemon whenever the socket is momentarily absent --
	// exactly what a departing daemon's shutdown looks like. Failing instantly
	// in that window leaves the user with no daemon at all.
	path := LockPath(t.TempDir())
	first, err := Acquire(path, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		time.Sleep(150 * time.Millisecond)
		first.Release()
	}()

	start := time.Now()
	second, err := Acquire(path, 2*time.Second)
	if err != nil {
		t.Fatalf("gave up on a departing daemon: %v", err)
	}
	defer second.Release()

	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("returned after %v -- it cannot have waited for the release", elapsed)
	}
}

func TestAnUnwritableStateDirectoryStillLetsTheUserCast(t *testing.T) {
	// Losing single-instance protection is bad. Refusing to cast because a
	// lock file could not be created is worse.
	dir := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	lock, err := Acquire(LockPath(filepath.Join(dir, "castr")), 50*time.Millisecond)

	if err != nil {
		t.Fatalf("err = %v, want the daemon to start anyway", err)
	}
	if lock == nil {
		t.Fatal("lock = nil, want a stand-in so callers have one code path")
	}
	if err := lock.Release(); err != nil {
		t.Errorf("releasing a stand-in lock: %v", err)
	}
}

func TestALockDiesWithItsProcess(t *testing.T) {
	// A daemon that crashes must not lock out its successors. This is a
	// property of flock rather than of our code, and the daemon depends on it,
	// so it is worth one real subprocess to know it holds here.
	if os.Getenv("CASTR_LOCK_CHILD") != "" {
		lock, err := Acquire(os.Getenv("CASTR_LOCK_PATH"), time.Second)
		if err != nil {
			os.Exit(3)
		}
		_ = lock
		os.Exit(0) // exit WITHOUT releasing
	}

	path := LockPath(t.TempDir())
	cmd := exec.Command(os.Args[0], "-test.run=TestALockDiesWithItsProcess")
	cmd.Env = append(os.Environ(), "CASTR_LOCK_CHILD=1", "CASTR_LOCK_PATH="+path)
	if err := cmd.Run(); err != nil {
		t.Fatalf("child failed to take the lock: %v", err)
	}

	lock, err := Acquire(path, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("a dead process still holds the lock: %v", err)
	}
	lock.Release()
}
