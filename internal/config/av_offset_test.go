package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAVOffsetPathStaysInsideItsDirectory(t *testing.T) {
	dir := "/state"
	for id, want := range map[string]string{
		"airplay:10:23:9A:E2:CE:8C": "airplay_10_23_9A_E2_CE_8C",
		"../../etc/passwd":          "_.._etc_passwd",
		"..":                        "_",
		"":                          "_",
	} {
		got := AVOffsetPath(dir, id)
		if got != filepath.Join(dir, "av-offset", want) {
			t.Errorf("AVOffsetPath(%q) = %q, want %q", id, got, filepath.Join(dir, "av-offset", want))
		}
		if filepath.Dir(got) != filepath.Join(dir, "av-offset") {
			t.Errorf("AVOffsetPath(%q) escaped its directory: %q", id, got)
		}
	}
}

func TestAVOffsetRoundTrips(t *testing.T) {
	dir := t.TempDir()
	if ms, err := ReadAVOffset(dir, "airplay:x"); ms != 0 || err != nil {
		t.Fatalf("unset: %d, %v; want 0, nil", ms, err)
	}
	for _, ms := range []int{120, -340, AVOffsetLimitMS, -AVOffsetLimitMS} {
		if err := WriteAVOffset(dir, "airplay:x", ms); err != nil {
			t.Fatal(err)
		}
		if got, err := ReadAVOffset(dir, "airplay:x"); got != ms || err != nil {
			t.Fatalf("wrote %d, read %d, %v", ms, got, err)
		}
	}
	if err := WriteAVOffset(dir, "airplay:x", 99999); err != nil {
		t.Fatal(err)
	}
	if got, _ := ReadAVOffset(dir, "airplay:x"); got != AVOffsetLimitMS {
		t.Fatalf("over the limit read back as %d", got)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "av-offset"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".offset-") {
			t.Errorf("a temporary file was left behind: %s", e.Name())
		}
	}
}

func TestAnUnreadableOffsetIsAnError(t *testing.T) {
	dir := t.TempDir()
	path := AVOffsetPath(dir, "airplay:x")
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte("loud"), 0o600)
	if _, err := ReadAVOffset(dir, "airplay:x"); err == nil {
		t.Fatal("read a non-number without complaint")
	}
}
