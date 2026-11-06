package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/mrCode/castr/internal/sound"
)

// The router holds its lock across every pactl call, and status, stop and
// shutdown all wait on that lock: one hung pactl would freeze castr.
func TestASoundCommandThatHangsIsCutOff(t *testing.T) {
	old := soundTimeout
	soundTimeout = 100 * time.Millisecond
	t.Cleanup(func() { soundTimeout = old })

	start := time.Now()
	_, err := execRunner("sleep", "10")
	if err == nil {
		t.Fatal("a command that hung reported success")
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the hung command held castrd for %v", took)
	}
}

func TestASoundCommandStillReturnsItsOutput(t *testing.T) {
	out, err := execRunner("echo", "castr_mix")
	if err != nil || out != "castr_mix\n" {
		t.Fatalf("execRunner = %q, %v; want the output", out, err)
	}
}

func TestTheSoundGuardChecksItsArguments(t *testing.T) {
	dir := t.TempDir() // no state file: a guard with good arguments has nothing to do
	self := strconv.Itoa(os.Getpid())
	for name, tc := range map[string]struct {
		args []string
		want int
	}{
		"good arguments":    {[]string{self, dir}, 0},
		"no arguments":      {nil, 2},
		"no state dir":      {[]string{self}, 2},
		"an empty dir":      {[]string{self, ""}, 2},
		"a pid that is not": {[]string{"castrd", dir}, 2},
		"pid zero":          {[]string{"0", dir}, 2},
		"a negative pid":    {[]string{"-4", dir}, 2},
		"an extra argument": {[]string{self, dir, "more"}, 2},
	} {
		t.Run(name, func(t *testing.T) {
			if got := runSoundGuard(tc.args); got != tc.want {
				t.Errorf("runSoundGuard(%q) = %d, want %d", tc.args, got, tc.want)
			}
		})
	}
}

func TestTheSoundGuardLeavesAnotherDaemonsRoutingAlone(t *testing.T) {
	// The routing belongs to a live daemon that is not the one this guard
	// watches: the guard must exit at once, changing nothing.
	dir := t.TempDir()
	state := `{"previous_default": "speakers", "laptop_sink": "speakers", "daemon_pid": ` + strconv.Itoa(os.Getpid()) + "}\n"
	if err := os.WriteFile(filepath.Join(dir, sound.StateFile), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() { done <- runSoundGuard([]string{"999999999", dir}) }()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit %d, want 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the guard of a dead daemon kept watching another daemon's routing")
	}
	if _, err := os.Stat(filepath.Join(dir, sound.StateFile)); err != nil {
		t.Errorf("the other daemon's state file: %v", err)
	}
}
