package config

// audio/video sync offset per receiver.
//
// One small file per receiver holds a signed number of milliseconds: positive
// delays the audio, negative delays the video so the audio comes sooner. A TV's
// own processing delay does not change between casts, so the value is kept per
// receiver and applies to every cast to it. castr-airplay rereads the file while
// casting (-av-offset-file), so a change takes effect within a quarter second.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// AVOffsetLimitMS bounds the offset either way; castr-airplay clamps to the same.
const AVOffsetLimitMS = 1000

// AVOffsetPath is the receiver's offset file under castr's state directory.
// Device IDs carry colons ("airplay:10:23:..."), so only a safe spelling is used.
func AVOffsetPath(stateDir, deviceID string) string {
	var sb strings.Builder
	for _, r := range deviceID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
			sb.WriteRune(r)
		default:
			sb.WriteRune('_')
		}
	}
	name := strings.Trim(sb.String(), ".")
	if name == "" {
		name = "_"
	}
	return filepath.Join(stateDir, "av-offset", name)
}

// ReadAVOffset returns the receiver's offset in milliseconds; none set is 0.
func ReadAVOffset(stateDir, deviceID string) (int, error) {
	data, err := os.ReadFile(AVOffsetPath(stateDir, deviceID))
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	ms, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("unreadable audio/video offset for %s: %q", deviceID, strings.TrimSpace(string(data)))
	}
	return ClampAVOffset(ms), nil
}

// WriteAVOffset stores the receiver's offset; 0 removes the file.
func WriteAVOffset(stateDir, deviceID string, ms int) error {
	path := AVOffsetPath(stateDir, deviceID)
	ms = ClampAVOffset(ms)
	if ms == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".offset-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := fmt.Fprintf(tmp, "%d\n", ms); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	// Renamed into place, so castr-airplay never reads half a number.
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// ClampAVOffset limits ms to ±AVOffsetLimitMS.
func ClampAVOffset(ms int) int {
	if ms > AVOffsetLimitMS {
		return AVOffsetLimitMS
	}
	if ms < -AVOffsetLimitMS {
		return -AVOffsetLimitMS
	}
	return ms
}
