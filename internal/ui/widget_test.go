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

func TestTheWaybarModuleDrivesCastrThroughItsCommands(t *testing.T) {
	// waybar cannot host a panel, so it stays on the standalone commands: the
	// menu for choosing, stop for stopping, bar for the icon.
	jsonc := repoFile(t, "share/waybar/cast-indicator.jsonc")

	for _, want := range []string{"castr bar", "castr menu", "castr stop"} {
		if !strings.Contains(jsonc, want) {
			t.Errorf("the waybar module does not run %q", want)
		}
	}
	// `castr status` spawns a daemon; waybar polls its exec every 2s.
	if strings.Contains(jsonc, "castr status") {
		t.Error("the waybar module polls `castr status`, which spawns a daemon")
	}
}

func TestTheWidgetPollsOnlyTheCommandThatCannotSpawnADaemon(t *testing.T) {
	// The Quickshell widget has its own panel and does NOT shell out to
	// `castr menu`. What it must not do is poll anything that starts a daemon:
	// a 2s timer doing that keeps one alive forever and the idle timeout --
	// which exists so discovery stays warm and no longer -- means nothing.
	//
	// Opening the panel is a different matter: the user asked for it.
	qml := repoFile(t, "share/quickshell/castr-indicator/Widget.qml")

	if cmd := polledCommand(t, qml); !strings.Contains(cmd, `"bar"`) {
		t.Errorf("the widget polls %s on a timer; only `castr bar` is safe there", cmd)
	}
	for _, want := range []string{`"start"`, `"stop"`, `"list"`} {
		if !strings.Contains(qml, want) {
			t.Errorf("the panel cannot %s anything", want)
		}
	}
}

// polledCommand returns the command attached to the widget's repeating status
// timer -- the one that runs whether or not anybody is looking.
func polledCommand(t *testing.T, qml string) string {
	t.Helper()
	i := strings.Index(qml, "id: statusProc")
	if i < 0 {
		t.Fatal("no statusProc in the widget; this checker has drifted from it")
	}
	rest := qml[i:]
	j := strings.Index(rest, "command:")
	if j < 0 {
		t.Fatal("statusProc has no command")
	}
	line := rest[j:]
	if end := strings.Index(line, "\n"); end >= 0 {
		line = line[:end]
	}
	return line
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
