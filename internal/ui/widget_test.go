package ui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/mrCode/castr/internal/daemon"
	"github.com/mrCode/castr/internal/session"
)

// repoFile reads a file from the repository root.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return string(raw)
}

func TestTheBarWidgetsPollAndTheStatusPayloadAgree(t *testing.T) {
	// The widget parses this JSON by field name. Renaming a field here without
	// touching the QML leaves an indicator that shows "idle" forever -- with
	// no error anywhere, because the parse "succeeds" against missing keys.
	raw, err := json.Marshal(Render(nil))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}

	qml := repoFile(t, "share/quickshell/castr-indicator/Widget.qml")
	for _, key := range []string{"class", "tooltip"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("the status payload has no %q field", key)
		}
		if !strings.Contains(qml, key) {
			t.Errorf("the widget never reads %q", key)
		}
	}
}

// statusSamples covers every shape Render can be handed, so the class list it
// can emit is discovered rather than assumed.
func statusSamples() [][]daemon.SessionJSON {
	return [][]daemon.SessionJSON{
		nil,
		{cast("a", "TV", session.ModeMirror, "streaming")},
		{cast("a", "TV", session.ModeMirror, string(session.Connecting))},
		{cast("a", "TV", session.ModeMirror, string(session.AwaitingPin))},
		{{DeviceID: "a", Name: "TV", State: string(session.Failed), Error: "gone"}},
		{cast("a", "One", session.ModeMirror, "streaming"),
			cast("b", "Two", session.ModeExtend, "streaming")},
	}
}

func TestEveryClassTheRendererEmitsIsOneTheWidgetStyles(t *testing.T) {
	// An unstyled class renders as an invisible or default-coloured icon, and
	// the user cannot tell a failed cast from an idle one.
	css := repoFile(t, "share/waybar/cast-indicator.css")
	emitted := map[string]bool{}
	for _, s := range statusSamples() {
		emitted[Render(s).Class] = true
	}

	for class := range emitted {
		if !strings.Contains(css, "."+class) {
			t.Errorf("class %q is emitted but has no style", class)
		}
	}
}

func TestBothBarsInvokeTheSameCommands(t *testing.T) {
	// They must agree, and both must use `castr bar` -- the one status command
	// that never spawns a daemon. Polling `castr status` instead would keep a
	// daemon alive forever and defeat the idle timeout.
	qml := repoFile(t, "share/quickshell/castr-indicator/Widget.qml")
	jsonc := repoFile(t, "share/waybar/cast-indicator.jsonc")

	for _, want := range []string{"castr bar", "castr menu", "castr stop"} {
		parts := strings.Fields(want)
		if !strings.Contains(jsonc, want) {
			t.Errorf("the waybar module does not run %q", want)
		}
		// QML passes argv as a list for the poll and a string for the clicks.
		if !strings.Contains(qml, want) && !strings.Contains(qml, `"`+parts[1]+`"`) {
			t.Errorf("the widget does not run %q", want)
		}
	}
	if strings.Contains(jsonc, "castr status") || strings.Contains(qml, `"status"`) {
		t.Error("a bar polls `castr status`, which spawns a daemon")
	}
}

func TestTheWidgetIdIsNotTheOldPackagesId(t *testing.T) {
	// castr and omarchy-cast can be installed at once. Two widgets sharing an
	// id is a collision in the shell's plugin registry.
	manifest := repoFile(t, "share/quickshell/castr-indicator/manifest.json")

	var m struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(manifest), &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != "castr.indicator" {
		t.Errorf("id = %q, want castr.indicator", m.ID)
	}
	if strings.Contains(manifest, "omarchy-cast") {
		t.Error("the manifest still mentions omarchy-cast")
	}
}

func TestTheWidgetShowsAnIconInEveryState(t *testing.T) {
	// Hiding it when idle makes the one control that stops a cast unfindable.
	qml := repoFile(t, "share/quickshell/castr-indicator/Widget.qml")

	// text: <casting icon> : <idle icon> -- both branches must be non-empty.
	pattern := regexp.MustCompile(`text:\s*root\.casting\s*\?\s*"([^"]+)"\s*:\s*"([^"]+)"`)
	m := pattern.FindStringSubmatch(qml)
	if m == nil {
		t.Fatal("could not find the icon expression; it may no longer be conditional")
	}
	if m[1] == "" || m[2] == "" {
		t.Errorf("icons = %q / %q, want one for each state", m[1], m[2])
	}
	if m[1] == m[2] {
		t.Errorf("both states show %q; the user cannot tell them apart", m[1])
	}
}
