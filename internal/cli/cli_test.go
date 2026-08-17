package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mrCode/castr/internal/client"
	"github.com/mrCode/castr/internal/daemon"
	"github.com/mrCode/castr/internal/picker"
	"github.com/mrCode/castr/internal/session"
	"github.com/mrCode/castr/internal/ui"
)

// stubDaemon answers over a real socket, so the CLI is exercised through the
// same protocol it uses in production.
type stubDaemon struct {
	mu       sync.Mutex
	devices  []daemon.DeviceJSON
	sessions []daemon.SessionJSON
	requests []daemon.Request
	failWith map[string]string
}

func (s *stubDaemon) handle(req daemon.Request) daemon.Response {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, req)

	if msg, ok := s.failWith[req.Cmd]; ok {
		return daemon.Err(msg)
	}
	switch req.Cmd {
	case daemon.CmdList:
		return daemon.OK(map[string]any{"devices": s.devices})
	case daemon.CmdStatus:
		return daemon.OK(map[string]any{"sessions": s.sessions})
	case daemon.CmdAdd:
		if req.Device != nil {
			s.devices = append(s.devices, *req.Device)
		}
		return daemon.OK(nil)
	default:
		return daemon.OK(nil)
	}
}

func (s *stubDaemon) seen(cmd string) []daemon.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []daemon.Request
	for _, r := range s.requests {
		if r.Cmd == cmd {
			out = append(out, r)
		}
	}
	return out
}

// fakeMenu records what it was shown and replies with a scripted sequence.
type fakeMenu struct {
	installed bool
	replies   []string
	asked     string
	shown     [][]string
	prompts   []string
	calls     int
}

func (m *fakeMenu) look(name string) (string, error) {
	if m.installed {
		return "/usr/bin/" + name, nil
	}
	return "", &net.AddrError{Err: "not installed"}
}

func (m *fakeMenu) run(argv []string, _ string) (string, error) {
	m.calls++
	m.prompts = append(m.prompts, argv[1])
	m.shown = append(m.shown, argv[2:])
	if len(m.replies) == 0 {
		return "", nil
	}
	reply := m.replies[0]
	m.replies = m.replies[1:]
	return reply, nil
}

type harness struct {
	app    *App
	daemon *stubDaemon
	menu   *fakeMenu
	out    *bytes.Buffer
	errOut *bytes.Buffer
}

func newHarness(t *testing.T, replies ...string) *harness {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "s")
	d := &stubDaemon{failWith: map[string]string{}}

	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				raw, err := bufio.NewReader(conn).ReadBytes('\n')
				if err != nil {
					return
				}
				var req daemon.Request
				if json.Unmarshal(raw, &req) != nil {
					return
				}
				out, _ := json.Marshal(d.handle(req))
				conn.Write(append(out, '\n'))
			}()
		}
	}()

	m := &fakeMenu{installed: true, replies: replies}
	h := &harness{daemon: d, menu: m, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	h.app = &App{
		Client: client.New(socket, nil),
		Picker: picker.Picker{Look: m.look, Exec: m.run},
		Out:    h.out,
		Err:    h.errOut,
		Socket: socket,
	}
	return h
}

func tv() daemon.DeviceJSON {
	return daemon.DeviceJSON{ID: "airplay:aa:bb", Name: "Meeting Room",
		Address: "10.10.10.231", Port: 7000, Protocol: "airplay", Model: "AppleTV11,1"}
}

func TestListPrintsWhatWasFound(t *testing.T) {
	h := newHarness(t)
	h.daemon.devices = []daemon.DeviceJSON{tv()}

	if code := h.app.Run([]string{"list"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}

	for _, want := range []string{"Meeting Room", "10.10.10.231", "airplay:aa:bb"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("output does not mention %q:\n%s", want, h.out)
		}
	}
}

func TestAnEmptyListSaysSoOutLoud(t *testing.T) {
	// Printing nothing is indistinguishable from a command that silently did
	// nothing, which is how "the menu showed no receivers" got reported as a
	// crash more than once.
	h := newHarness(t)

	if code := h.app.Run([]string{"list"}); code != 0 {
		t.Fatalf("exit %d", code)
	}

	if !strings.Contains(h.out.String(), "No receivers") {
		t.Errorf("output = %q, want it to say so", h.out)
	}
}

func TestStartDefaultsToMirror(t *testing.T) {
	h := newHarness(t)

	if code := h.app.Run([]string{"start", "airplay:aa"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}

	got := h.daemon.seen(daemon.CmdStart)
	if len(got) != 1 || got[0].Mode != session.ModeMirror {
		t.Errorf("start = %+v, want mirror", got)
	}
}

func TestStartAcceptsExtend(t *testing.T) {
	h := newHarness(t)

	if code := h.app.Run([]string{"start", "airplay:aa", "extend"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}

	got := h.daemon.seen(daemon.CmdStart)
	if len(got) != 1 || got[0].Mode != session.ModeExtend {
		t.Errorf("start = %+v, want extend", got)
	}
}

func TestAnUnknownModeIsRejectedBeforeTheDaemonIsTroubled(t *testing.T) {
	h := newHarness(t)

	code := h.app.Run([]string{"start", "airplay:aa", "clone"})

	if code == 0 {
		t.Error("an unknown mode exited 0")
	}
	if len(h.daemon.seen(daemon.CmdStart)) != 0 {
		t.Error("the daemon was asked to start an unknown mode")
	}
	if !strings.Contains(h.errOut.String(), "clone") {
		t.Errorf("stderr = %q, want it to name the bad mode", h.errOut)
	}
}

func TestAFailedCommandExitsNonZeroWithTheDaemonsReason(t *testing.T) {
	// The keybind shows this string. "device not found: x" is actionable.
	h := newHarness(t)
	h.daemon.failWith[daemon.CmdStart] = "device not found: airplay:ghost"

	code := h.app.Run([]string{"start", "airplay:ghost"})

	if code == 0 {
		t.Error("a failed start exited 0")
	}
	if !strings.Contains(h.errOut.String(), "device not found: airplay:ghost") {
		t.Errorf("stderr = %q, want the daemon's own message", h.errOut)
	}
}

func TestStopWithNoIdStopsEverythingThatIsCasting(t *testing.T) {
	// This is what the bar's right-click runs, and it knows no device ids.
	h := newHarness(t)
	h.daemon.sessions = []daemon.SessionJSON{
		{DeviceID: "a", Name: "One", Mode: "mirror", State: "streaming"},
		{DeviceID: "b", Name: "Two", Mode: "extend", State: "streaming"},
	}

	if code := h.app.Run([]string{"stop"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}

	if got := h.daemon.seen(daemon.CmdStop); len(got) != 2 {
		t.Errorf("stopped %d sessions, want both", len(got))
	}
}

func TestOneStuckReceiverDoesNotStrandTheOthers(t *testing.T) {
	h := newHarness(t)
	h.daemon.sessions = []daemon.SessionJSON{
		{DeviceID: "a", Name: "One", State: "streaming"},
		{DeviceID: "b", Name: "Two", State: "streaming"},
	}
	h.daemon.failWith[daemon.CmdStop] = "no route to host"

	code := h.app.Run([]string{"stop"})

	if code == 0 {
		t.Error("exit 0 despite failures")
	}
	if got := h.daemon.seen(daemon.CmdStop); len(got) != 2 {
		t.Errorf("attempted %d stops, want every session tried", len(got))
	}
}

func TestStoppingNothingIsNotAnError(t *testing.T) {
	h := newHarness(t)

	if code := h.app.Run([]string{"stop"}); code != 0 {
		t.Errorf("exit %d, want stopping nothing to be fine", code)
	}
	if !strings.Contains(h.out.String(), "Nothing is casting") {
		t.Errorf("output = %q", h.out)
	}
}

func TestTheBarCommandNeverSpawnsADaemon(t *testing.T) {
	// The bar polls it several times a minute. Spawning from a poll keeps a
	// daemon alive forever and defeats the idle timeout completely.
	h := newHarness(t)
	absent := filepath.Join(t.TempDir(), "absent")
	spawned := false
	h.app.Socket = absent
	h.app.Client = client.New(absent, func() error { spawned = true; return nil })

	code := h.app.Run([]string{"bar"})

	if spawned {
		t.Error("a bar poll started a daemon")
	}
	if code != 0 {
		t.Errorf("exit %d, want 0", code)
	}
}

func TestTheBarCommandNeverFailsWithNoDaemon(t *testing.T) {
	// The bar polls it several times a minute; a non-zero exit shows an error
	// indicator for the entirely normal state of not casting.
	h := newHarness(t)
	h.app.Socket = filepath.Join(t.TempDir(), "absent")

	code := h.app.Run([]string{"bar"})

	if code != 0 {
		t.Errorf("exit %d with no daemon, want 0", code)
	}
	var status ui.Status
	if err := json.Unmarshal(h.out.Bytes(), &status); err != nil {
		t.Fatalf("output is not the JSON the bar parses: %v (%q)", err, h.out)
	}
	if status.Class != "idle" || status.Text == "" {
		t.Errorf("status = %+v, want a visible idle indicator", status)
	}
}

func TestTheBarCommandEmitsExactlyOneLine(t *testing.T) {
	// waybar reads one JSON object per line and drops the module on anything
	// else.
	h := newHarness(t)
	h.daemon.sessions = []daemon.SessionJSON{
		{DeviceID: "a", Name: "TV", Mode: "mirror", State: "streaming"}}

	h.app.Run([]string{"bar"})

	lines := strings.Split(strings.TrimRight(h.out.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("printed %d lines, want 1:\n%s", len(lines), h.out)
	}
	var status ui.Status
	if err := json.Unmarshal([]byte(lines[0]), &status); err != nil {
		t.Fatal(err)
	}
	if status.Class != "streaming" {
		t.Errorf("class = %q", status.Class)
	}
}

func TestAddUsesAnIdDerivedFromTheAddress(t *testing.T) {
	// Re-adding the same receiver must update it, not add it again: the menu
	// once listed the same Apple TV four times.
	h := newHarness(t)

	// Different names on purpose: the id must key on the ADDRESS alone, or
	// renaming a receiver adds a second copy of it to the menu.
	h.app.Run([]string{"add", "10.10.10.231", "Meeting", "Room"})
	h.app.Run([]string{"add", "10.10.10.231", "Boardroom"})

	got := h.daemon.seen(daemon.CmdAdd)
	if len(got) != 2 {
		t.Fatalf("adds = %d", len(got))
	}
	if got[0].Device.ID != got[1].Device.ID {
		t.Errorf("ids %q and %q differ; the same address must give the same id",
			got[0].Device.ID, got[1].Device.ID)
	}
	if got[0].Device.Name != "Meeting Room" {
		t.Errorf("name = %q, want the words joined", got[0].Device.Name)
	}
	if got[1].Device.Name != "Boardroom" {
		t.Errorf("name = %q, want the new name to still apply", got[1].Device.Name)
	}
}

func TestAddWithNoNameFallsBackToTheAddress(t *testing.T) {
	h := newHarness(t)

	h.app.Run([]string{"add", "10.0.0.5"})

	got := h.daemon.seen(daemon.CmdAdd)
	if len(got) != 1 || got[0].Device.Name != "10.0.0.5" {
		t.Errorf("device = %+v, want the address as the name", got[0].Device)
	}
}

func TestAnUnknownCommandPrintsTheUsage(t *testing.T) {
	h := newHarness(t)

	code := h.app.Run([]string{"casst"})

	if code == 0 {
		t.Error("an unknown command exited 0")
	}
	if !strings.Contains(h.errOut.String(), "castr menu") {
		t.Errorf("stderr = %q, want the usage", h.errOut)
	}
}

func TestNoArgumentsPrintsTheUsage(t *testing.T) {
	h := newHarness(t)

	if code := h.app.Run(nil); code == 0 {
		t.Error("no arguments exited 0")
	}
	if !strings.Contains(h.errOut.String(), "castr start") {
		t.Errorf("stderr = %q, want the usage", h.errOut)
	}
}

func TestHelpGoesToStdoutAndSucceeds(t *testing.T) {
	// Asking for help is not an error, and piping it to a pager should work.
	h := newHarness(t)

	if code := h.app.Run([]string{"help"}); code != 0 {
		t.Errorf("exit %d", code)
	}
	if !strings.Contains(h.out.String(), "castr menu") {
		t.Errorf("stdout = %q", h.out)
	}
}

func TestListJSONIsOneParseableLine(t *testing.T) {
	// The bar panel parses this. Scraping the human columns instead would
	// break the UI the moment a column width changed.
	h := newHarness(t)
	h.daemon.devices = []daemon.DeviceJSON{tv()}

	if code := h.app.Run([]string{"list", "--json"}); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errOut)
	}

	lines := strings.Split(strings.TrimRight(h.out.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("printed %d lines, want 1", len(lines))
	}
	var data struct {
		Devices []daemon.DeviceJSON `json:"devices"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &data); err != nil {
		t.Fatalf("not JSON: %v (%q)", err, lines[0])
	}
	if len(data.Devices) != 1 || data.Devices[0].ID != tv().ID {
		t.Errorf("devices = %+v", data.Devices)
	}
}

func TestAFailedListStillAnswersInJSON(t *testing.T) {
	// The panel must be able to tell "no receivers" from "the daemon is not
	// answering". A bare non-zero exit with no output looks like the former.
	h := newHarness(t)
	h.daemon.failWith[daemon.CmdList] = "no castr daemon is running"

	code := h.app.Run([]string{"list", "--json"})

	if code == 0 {
		t.Error("a failed list exited 0")
	}
	var data struct {
		Devices []daemon.DeviceJSON `json:"devices"`
		Error   string              `json:"error"`
	}
	if err := json.Unmarshal(h.out.Bytes(), &data); err != nil {
		t.Fatalf("not JSON: %v (%q)", err, h.out.String())
	}
	if data.Error == "" {
		t.Error("the failure carried no reason the panel could show")
	}
}

func TestStatusJSONCarriesModeAndState(t *testing.T) {
	h := newHarness(t)
	h.daemon.sessions = []daemon.SessionJSON{
		{DeviceID: "a", Name: "TV", Mode: "extend", State: "streaming"}}

	if code := h.app.Run([]string{"status", "--json"}); code != 0 {
		t.Fatalf("exit %d", code)
	}

	var data struct {
		Sessions []daemon.SessionJSON `json:"sessions"`
	}
	if err := json.Unmarshal(h.out.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Sessions) != 1 || data.Sessions[0].Mode != "extend" ||
		data.Sessions[0].State != "streaming" {
		t.Errorf("sessions = %+v", data.Sessions)
	}
}

func TestHumanOutputIsUnchangedWithoutTheFlag(t *testing.T) {
	// The JSON flag must not quietly become the default; people read this.
	h := newHarness(t)
	h.daemon.devices = []daemon.DeviceJSON{tv()}

	h.app.Run([]string{"list"})

	if strings.HasPrefix(strings.TrimSpace(h.out.String()), "{") {
		t.Errorf("plain list emitted JSON: %q", h.out)
	}
}
