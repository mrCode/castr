package main

import (
	"os"
	"strconv"
	"testing"
	"time"
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
