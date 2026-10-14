package ui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The pairing card's code field is a plain Item that takes keystrokes itself,
// because an editable text input pulls the clipboard owner's data into the
// shell on every clipboard change. Hand-written key handling is exactly where
// subtle bugs live -- a dropped Backspace, a paste that slips through, Shift
// mistaken for Ctrl -- so this types at it with real Qt key events.
//
// The handler is cut out of Widget.qml verbatim and dropped into a stub panel,
// so the shipped code is what runs. Skipped where Qt's test runner is not
// installed, so the suite still needs nothing but Go.
func TestTheCodeFieldTypesLikeAField(t *testing.T) {
	// Qt 6's runner only. A plain `qmltestrunner` on PATH is often Qt 5's --
	// qt5-declarative still ships /usr/bin/qmltestrunner -- and Qt 5 fails on
	// this Qt 6 QML with no output at all, which reads as a failing widget.
	runner := ""
	for _, candidate := range []string{"/usr/lib/qt6/bin/qmltestrunner", "qmltestrunner6", "qmltestrunner-qt6"} {
		if path, err := exec.LookPath(candidate); err == nil {
			runner = path
			break
		}
	}
	if runner == "" {
		t.Skip("Qt 6's qmltestrunner is not installed")
	}

	qml := repoFile(t, "share/quickshell/castr-indicator/Widget.qml")
	handler := pinInputKeyHandler(t, qml)

	tmpl := repoFile(t, "internal/ui/testdata/tst_pininput.qml.tmpl")
	test := filepath.Join(t.TempDir(), "tst_pininput.qml")
	if err := os.WriteFile(test, []byte(strings.Replace(tmpl, "HANDLER", handler, 1)), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(runner, "-input", test, "-o", "-,txt")
	cmd.Env = append(os.Environ(), "QT_QPA_PLATFORM=offscreen")
	out, err := cmd.CombinedOutput()
	if !strings.Contains(string(out), "Totals:") {
		t.Fatalf("%s ran no tests (%v); the harness, not the widget, is broken:\n%s", runner, err, out)
	}
	if err != nil || !strings.Contains(string(out), " 0 failed") {
		t.Fatalf("the code field does not behave:\n%s", out)
	}
}

// pinInputKeyHandler returns pinInput's Keys.onPressed block, braces and all.
func pinInputKeyHandler(t *testing.T, qml string) string {
	t.Helper()
	i := strings.Index(qml, "id: pinInput")
	if i < 0 {
		t.Fatal("no pinInput in the widget; this checker has drifted from it")
	}
	start := strings.Index(qml[i:], "Keys.onPressed: function(event) {")
	if start < 0 {
		t.Fatal("pinInput has no key handler")
	}
	start += i
	open := start + strings.Index(qml[start:], "{")
	return qml[start : open+len(braceBlock(qml, open))]
}
