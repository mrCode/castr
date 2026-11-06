package sound

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// WirePlumber keeps more than default.configured.audio.sink: every output the
// user chose, most recent first, in its state file, as
// default.configured.audio.sink, then .sink.0, .sink.1, ... (WirePlumber
// 0.5's state-default-nodes.lua). The default is the first of them that
// exists, so it is the order Bluetooth headphones and a USB headset take over
// in. Writing the key moves that output to the top and shifts the rest down;
// deleting it erases the whole stack. castr's own takeover pushes castr_mix
// onto it, so the restore rebuilds the stack castr found.

// WirePlumberHistoryFile is where WirePlumber keeps the stack:
// $XDG_STATE_HOME/wireplumber/default-nodes, by default under
// ~/.local/state. "" when there is no home to find it in.
func WirePlumberHistoryFile() string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home := os.Getenv("HOME")
		if home == "" {
			return ""
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "wireplumber", "default-nodes")
}

// readSinkHistory reads the audio-sink stack from WirePlumber's state file,
// a GKeyFile, top first. It only reads. The stack ends at the first missing
// key, as WirePlumber reads it, so with no top key it is empty: an empty,
// non-nil slice. A file that cannot be read is an error -- the history is
// then unknown, which is not the same as empty.
func readSinkHistory(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	keys := map[string]string{}
	group := ""
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			group = line[1 : len(line)-1]
		case group == "default-nodes":
			if k, v, ok := strings.Cut(line, "="); ok {
				keys[strings.TrimSpace(k)] = unescapeKeyFile(strings.TrimLeft(v, " \t"))
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	history := []string{}
	key := configuredKey
	for i := 0; ; i++ {
		v, ok := keys[key]
		if !ok {
			return history, nil
		}
		history = append(history, v)
		key = configuredKey + "." + strconv.Itoa(i)
	}
}

// unescapeKeyFile undoes GKeyFile's escapes in a value.
func unescapeKeyFile(v string) string {
	if !strings.Contains(v, `\`) {
		return v
	}
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] != '\\' || i+1 == len(v) {
			b.WriteByte(v[i])
			continue
		}
		i++
		switch v[i] {
		case 's':
			b.WriteByte(' ')
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		default:
			b.WriteByte(v[i])
		}
	}
	return b.String()
}

// moveToTop is what WirePlumber does to the stack when the key is written.
// The result is never nil.
func moveToTop(history []string, name string) []string {
	out := []string{name}
	for _, h := range history {
		if h != name {
			out = append(out, h)
		}
	}
	return out
}

// historyAsConfigured makes the stack in the file agree with the configured
// default read from the metadata. WirePlumber saves its state file a moment
// after a change, so the file may lag: a pick it has not saved yet goes on
// top, and a deleted key means an empty stack. castr's own outputs, left by
// an earlier castr, are no choice of the user's and are dropped.
func historyAsConfigured(history []string, configured configuredDefault) []string {
	if configured.Name == "" {
		return []string{}
	}
	return slices.DeleteFunc(moveToTop(history, configured.Name), isCastrName)
}

// writeSinkHistory rebuilds the stack: each output, from the bottom up, is
// written as the key, which moves it to the top, so after the last write the
// stack starts exactly as saved. castr_mix, which castr pushed on, ends up
// below them, where it is never chosen once it is gone. An empty stack is
// put back by deleting the key, which erases what castr pushed on. A write
// that fails stops the rebuild; the retry starts it again from the bottom.
func writeSinkHistory(run Runner, history []string) error {
	if len(history) == 0 {
		return writeConfiguredDefault(run, configuredDefault{Saved: true})
	}
	for i := len(history) - 1; i >= 0; i-- {
		if err := writeConfiguredDefault(run, configuredDefault{Saved: true, Name: history[i]}); err != nil {
			return err
		}
	}
	return nil
}
