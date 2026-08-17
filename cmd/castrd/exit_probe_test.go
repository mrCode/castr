package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/mrCode/castr/internal/config"
)

// A child that exits immediately must make Read return promptly. If it does
// not, the supervisor waits out its whole ready timeout and then reports a
// reverse-connection problem for a child that simply died.
func TestAChildThatExitsIsNoticedPromptly(t *testing.T) {
	spawn := spawner(config.Default(), t.TempDir())
	proc, err := spawn(context.Background(),
		[]string{"/bin/sh", "-c", "echo dying words >&2; exit 3"})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan string, 1)
	go func() {
		var seen []byte
		buf := make([]byte, 256)
		for {
			n, err := proc.Read(buf)
			seen = append(seen, buf[:n]...)
			if err != nil {
				done <- string(seen)
				return
			}
		}
	}()

	select {
	case text := <-done:
		if text == "" {
			t.Error("the child's output was lost entirely")
		}
		t.Logf("read %q before the reader unblocked", text)
	case <-time.After(5 * time.Second):
		t.Fatal("Read never returned for a child that exited -- the supervisor " +
			"would wait out its entire ready timeout and then blame the network")
	}
	_ = io.EOF
}
