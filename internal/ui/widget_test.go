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

func TestTheWidgetSaysWhenCastrIsNotInstalled(t *testing.T) {
	// A Process whose binary is missing emits NO onExited in Quickshell -- it
	// fails to start and says nothing -- so the obvious "did it exit non-zero"
	// check never fires and the widget shows "Not casting" forever. That is
	// indistinguishable from a working install with nothing casting.
	//
	// The probe therefore goes through sh, which always exists, so its exit
	// code is a signal that actually arrives.
	qml := repoFile(t, "share/quickshell/castr-indicator/Widget.qml")

	if !strings.Contains(qml, "command -v castr") {
		t.Error("the widget never checks whether castr exists")
	}
	if !strings.Contains(qml, `"sh"`) {
		t.Error("the check does not go through sh; a missing binary would be silent")
	}
	if !strings.Contains(qml, "not installed") {
		t.Error("nothing tells the user castr is missing")
	}
	// The way out has to be in the message; a bare complaint is not actionable.
	if !strings.Contains(qml, "yay -S castr") {
		t.Error("the message does not say how to install it")
	}
}

func TestReceiverTextIsNeverRenderedAsRichText(t *testing.T) {
	// Receiver names, models and addresses come from mDNS — from anything on
	// the network that cares to advertise. QML's default textFormat is
	// AutoText, which sniffs for rich text, so a receiver advertising itself as
	// `<img src="http://attacker/x">` would make the widget FETCH that URL.
	//
	// Not hypothetical: a name of `<b>PWNED</b>` rendered in bold before this
	// was fixed. Reported by @ryanrhughes against commit eb22e2c.
	//
	// The rule is every Text, not only the ones carrying remote data today, so
	// a Text added later is safe by default rather than by review.
	qml := repoFile(t, "share/quickshell/castr-indicator/Widget.qml")
	lines := strings.Split(qml, "\n")

	opener := regexp.MustCompile(`^(\s*)([A-Z][A-Za-z]*)\s*\{`)
	textProp := regexp.MustCompile(`^(\s*)text:`)

	for i, line := range lines {
		m := textProp.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		// Which component owns this property? Only a Text renders markup.
		owner := ""
		for j := i - 1; j >= 0; j-- {
			if o := opener.FindStringSubmatch(lines[j]); o != nil && len(o[1]) < len(m[1]) {
				owner = o[2]
				break
			}
		}
		if owner != "Text" && owner != "PanelSectionHeader" {
			continue // e.g. BarIconButton, whose text is our own icon glyph
		}

		guarded := false
		for j := i + 1; j < len(lines) && j <= i+6; j++ {
			if strings.Contains(lines[j], "textFormat: Text.PlainText") {
				guarded = true
				break
			}
			trimmed := strings.TrimSpace(lines[j])
			if trimmed != "" && !strings.HasPrefix(trimmed, ":") &&
				!strings.HasPrefix(trimmed, "+") && !strings.HasPrefix(trimmed, "?") {
				break
			}
		}
		if !guarded {
			t.Errorf("%s at line %d has an unguarded text:, which defaults to AutoText:\n  %s",
				owner, i+1, strings.TrimSpace(line))
		}
	}
}

func TestTooltipsCarryingReceiverNamesAreNeutered(t *testing.T) {
	// Tooltips do not render through a Text this file owns: the shell's
	// PanelToolTip has its own bare Text, which defaults to AutoText. Anything
	// receiver-controlled must be stripped before it leaves here.
	qml := repoFile(t, "share/quickshell/castr-indicator/Widget.qml")

	if !strings.Contains(qml, "function plain(") {
		t.Fatal("no plain() helper; receiver text reaches shell tooltips raw")
	}
	for _, carrier := range []string{"root.tooltip", "modelData.name"} {
		for _, line := range strings.Split(qml, "\n") {
			if strings.Contains(line, "tooltipText") && strings.Contains(line, carrier) &&
				!strings.Contains(line, "plain(") {
				t.Errorf("tooltip passes %s unneutered:\n  %s", carrier, strings.TrimSpace(line))
			}
		}
	}
}
