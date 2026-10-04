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

// The panel chooses one mode for every receiver in the list, so a Chromecast
// clicked while "Extend" is selected would start a cast that cannot work.
// Extend needs a virtual output the Chromecast backend does not build.
func TestTheWidgetNeverStartsAnExtendCastOnAChromecast(t *testing.T) {
	source := repoFile(t, "share/quickshell/castr-indicator/Widget.qml")

	if !strings.Contains(source, `function modeFor(device)`) {
		t.Fatal("the widget has no per-device mode; the pill applies to every receiver")
	}
	if !strings.Contains(source, `device.protocol === "chromecast" ? "mirror" : root.mode`) {
		t.Error("modeFor does not force mirror for a Chromecast")
	}
	// The command that actually runs must use the per-device mode, not the pill.
	if strings.Contains(source, `"castr", "start", device.id, root.mode]`) {
		t.Error("startCast passes the panel's mode rather than the receiver's")
	}
	if !strings.Contains(source, `root.modeFor(device)]`) {
		t.Error("startCast does not use modeFor")
	}
	// And the row has to say so, rather than quietly doing something else.
	if !strings.Contains(source, "Mirror only") {
		t.Error("a Chromecast row does not say it will mirror regardless of the pill")
	}
}

// The always-on timer runs whether or not anybody is looking, so nothing it
// starts may spawn a daemon: a 2s timer that did would keep one alive forever
// and make the idle timeout meaningless.
//
// `castr bar` never spawns one. Anything else the timer starts must be gated on
// a state that proves a daemon is ALREADY running -- a cast connecting, or a
// session waiting for a pairing code -- because asking a running daemon cannot
// create another.
//
// The checker above looks only at the command bound to statusProc, so it kept
// passing when the timer began starting a second process (`castr status`, for
// the pairing card). That poll is safe because it is gated; this test is what
// says so, and fails the day the gate is removed.
func TestEveryProcessTheAlwaysOnTimerStartsIsSafeOrGated(t *testing.T) {
	qml := repoFile(t, "share/quickshell/castr-indicator/Widget.qml")
	body := alwaysOnTimerBody(t, qml)

	starts := regexp.MustCompile(`(\w+)\.running\s*=\s*true`).FindAllStringSubmatchIndex(body, -1)
	if len(starts) == 0 {
		t.Fatal("the always-on timer starts no processes; this checker has drifted from the widget")
	}

	for _, m := range starts {
		id := body[m[2]:m[3]]
		cmd := processCommand(t, qml, id)

		// The install probe asks the shell whether castr exists; it never
		// talks to a daemon.
		if strings.Contains(cmd, `"bar"`) || strings.Contains(cmd, "command -v castr") {
			continue
		}

		// The statement, including an `if` on the line above it.
		stmt := body[:m[1]]
		if k := strings.LastIndex(stmt[:m[0]], "\n"); k >= 0 {
			if k2 := strings.LastIndex(stmt[:k], "\n"); k2 >= 0 {
				stmt = stmt[k2:]
			}
		}
		if !strings.Contains(stmt, "root.busy") && !strings.Contains(stmt, "root.awaitingPin") {
			t.Errorf("the always-on timer starts %s (%s) without gating it on a running "+
				"cast; that can spawn a daemon and keep it alive forever", id, strings.TrimSpace(cmd))
		}
	}

	// The gate is only worth something if busy really means "a daemon holds a
	// cast". Widened to cover idle, it would let the poll run with no daemon.
	busy := regexp.MustCompile(`property bool busy:\s*([^\n]+)`).FindStringSubmatch(qml)
	if busy == nil || !strings.Contains(busy[1], `"connecting"`) || strings.Contains(busy[1], `"idle"`) {
		t.Errorf("busy no longer means a cast is connecting (%v); the timer's gate is meaningless", busy)
	}
}

// alwaysOnTimerBody returns the onTriggered block of the Timer that runs
// unconditionally and repeats.
func alwaysOnTimerBody(t *testing.T, qml string) string {
	t.Helper()
	for _, loc := range regexp.MustCompile(`Timer\s*\{`).FindAllStringIndex(qml, -1) {
		timer := braceBlock(qml, loc[1]-1)
		if !strings.Contains(timer, "running: true") || !strings.Contains(timer, "repeat: true") {
			continue
		}
		i := strings.Index(timer, "onTriggered:")
		if i < 0 {
			continue
		}
		j := strings.Index(timer[i:], "{")
		return braceBlock(timer, i+j)
	}
	t.Fatal("no always-on repeating Timer in the widget")
	return ""
}

// processCommand returns the command line of the Process with the given id.
func processCommand(t *testing.T, qml, id string) string {
	t.Helper()
	i := strings.Index(qml, "id: "+id+"\n")
	if i < 0 {
		t.Fatalf("no Process with id %s", id)
	}
	rest := qml[i:]
	j := strings.Index(rest, "command:")
	if j < 0 {
		// Commands set at call time, like pinProc's: find the assignment.
		k := strings.Index(qml, id+".command =")
		if k < 0 {
			t.Fatalf("Process %s has no command", id)
		}
		rest, j = qml[k:], 0
	}
	line := rest[j:]
	if end := strings.Index(line, "\n"); end >= 0 {
		line = line[:end]
	}
	return line
}

// braceBlock returns the text from the brace at open to its matching close.
func braceBlock(s string, open int) string {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[open : i+1]
			}
		}
	}
	return s[open:]
}
