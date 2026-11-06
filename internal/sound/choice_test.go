package sound

import (
	"strings"
	"testing"
)

func TestParseChoice(t *testing.T) {
	for in, want := range map[string]Choice{"tv": TV, "TV": TV, "laptop": Laptop, "both": Both, "Both": Both} {
		got, err := ParseChoice(in)
		if err != nil || got != want {
			t.Errorf("ParseChoice(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "speaker"} {
		_, err := ParseChoice(in)
		if err == nil {
			t.Errorf("ParseChoice(%q) accepted it", in)
			continue
		}
		for _, name := range []string{"tv", "laptop", "both"} {
			if !strings.Contains(err.Error(), name) {
				t.Errorf("ParseChoice(%q) error %q does not name %q", in, err, name)
			}
		}
	}
}
