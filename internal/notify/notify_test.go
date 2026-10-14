package notify

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mrCode/castr/internal/discovery"
	"github.com/mrCode/castr/internal/session"
)

func tv() discovery.Device {
	return discovery.Device{ID: "airplay:aa", Name: "Meeting Room",
		Address: "10.10.10.231", Port: 7000, Protocol: discovery.ProtocolAirPlay}
}

type recorder struct {
	calls [][]string
	err   error
}

func (r *recorder) run(argv []string) error {
	r.calls = append(r.calls, argv)
	return r.err
}

func TestOnlyFailuresAndPinPromptsAreAnnounced(t *testing.T) {
	// Narrating connecting, then streaming, then idle at a user who just
	// pressed a key and is watching the screen is the notification flood this
	// project already shipped once.
	quiet := []session.State{session.Connecting, session.Streaming,
		session.Stopping, session.Idle}
	for _, state := range quiet {
		t.Run(string(state), func(t *testing.T) {
			r := &recorder{}
			Notifier{Run: r.run}.OnState(tv(), state, "")
			if len(r.calls) != 0 {
				t.Errorf("state %q produced a banner: %v", state, r.calls)
			}
		})
	}
}

func TestAFailureIsAnnouncedWithItsReason(t *testing.T) {
	r := &recorder{}

	Notifier{Run: r.run}.OnState(tv(), session.Failed, "no route to host")

	if len(r.calls) != 1 {
		t.Fatalf("calls = %v, want one banner", r.calls)
	}
	joined := strings.Join(r.calls[0], " ")
	for _, want := range []string{"Meeting Room", "no route to host"} {
		if !strings.Contains(joined, want) {
			t.Errorf("banner %q does not mention %q", joined, want)
		}
	}
}

func TestAFailureWithNoReasonStillSaysSomething(t *testing.T) {
	msg, _, worth := Message(tv(), session.Failed, "")

	if !worth {
		t.Fatal("a failure was not announced at all")
	}
	if strings.HasSuffix(strings.TrimSpace(msg), ":") {
		t.Errorf("message = %q, ends with a colon and nothing after it", msg)
	}
}

func TestOnlyWhatTheUserMustActOnIsUrgent(t *testing.T) {
	// Urgent means sticky -- a banner that stays until it is answered. A cast
	// that dies while the user is working is worth that.
	if _, urgent, _ := Message(tv(), session.Failed, "gone"); !urgent {
		t.Error("a mid-stream failure was not urgent")
	}
	// So is a pairing code. This used to be non-urgent, on the reasoning that
	// the user is already watching for it -- but they are watching the
	// TELEVISION, where the code is, not the corner of the laptop where a
	// five-second banner came and went. With nothing else saying where to type
	// the code, pairing never completed and the receiver asked on every cast.
	// The cast cannot proceed until the user acts, which is this file's own
	// definition of urgent.
	if _, urgent, _ := Message(tv(), session.AwaitingPin, ""); !urgent {
		t.Error("a pairing prompt vanished on its own; the cast is blocked until it is answered")
	}
}

func TestAPinPromptPointsAtTheReceiver(t *testing.T) {
	// The code is on the television. Nothing else on this screen says so.
	msg, _, worth := Message(tv(), session.AwaitingPin, "")

	if !worth {
		t.Fatal("the PIN prompt was not announced")
	}
	if !strings.Contains(strings.ToLower(msg), "receiver") &&
		!strings.Contains(strings.ToLower(msg), "shown on") {
		t.Errorf("message = %q, want it to point at the receiver", msg)
	}
}

func TestEveryBannerReplacesTheLastRatherThanStacking(t *testing.T) {
	// An afternoon of casting left a column of banners to click away one by
	// one before this hint was added.
	for _, urgent := range []bool{false, true} {
		argv := strings.Join(Argv("anything", urgent), " ")
		if !strings.Contains(argv, SynchronousHint) {
			t.Errorf("urgent=%v argv = %q, want the synchronous hint", urgent, argv)
		}
	}
}

func TestANonUrgentBannerExpiresOnItsOwn(t *testing.T) {
	argv := strings.Join(Argv("anything", false), " ")

	if !strings.Contains(argv, "-t ") {
		t.Errorf("argv = %q, want an expiry so it does not sit in the corner", argv)
	}
	if strings.Contains(argv, "critical") {
		t.Errorf("argv = %q, want normal urgency", argv)
	}
}

func TestAMachineWithNoNotificationDaemonStillCasts(t *testing.T) {
	// notify-send missing is a cosmetic problem, not a casting problem.
	r := &recorder{err: errors.New("notify-send: not found")}

	Notifier{Run: r.run}.OnState(tv(), session.Failed, "gone") // must not panic

	if len(r.calls) != 1 {
		t.Errorf("calls = %v", r.calls)
	}
}

func TestANotifierWithNoRunnerIsHarmless(t *testing.T) {
	Notifier{}.OnState(tv(), session.Failed, "gone") // must not panic
}

// The banner has to say where the code goes. "Enter the PIN" with nowhere to
// enter it is the bug this replaced.
func TestAPinPromptSaysWhereToEnterTheCode(t *testing.T) {
	msg, _, _ := Message(tv(), session.AwaitingPin, "")
	if !strings.Contains(msg, "menu") || !strings.Contains(msg, "castr pin") {
		t.Errorf("message = %q; it must name the cast menu and castr pin", msg)
	}
}

// A receiver's name is whatever it advertises, unauthenticated, and the
// notification body is rendered as markup with hyperlinks on common daemons.
// Unescaped, anything on the network could put a clickable link in a castr
// banner just by choosing its own name.
func TestReceiverNamesCannotInjectMarkupIntoABanner(t *testing.T) {
	hostile := tv()
	hostile.Name = `<a href="http://attacker.example/">Living Room</a>`

	for _, state := range []session.State{session.AwaitingPin, session.Failed} {
		msg, urgent, _ := Message(hostile, state, "<b>reason</b> & more")
		body := Argv(msg, urgent)
		last := body[len(body)-1]
		if strings.ContainsAny(last, "<>") {
			t.Errorf("%s banner body carries raw markup: %q", state, last)
		}
		if !strings.Contains(last, "&lt;a href=") {
			t.Errorf("%s banner body lost the name instead of escaping it: %q", state, last)
		}
	}
	if pair := PairArgv("<i>x</i>", []string{"castr", "pin", "x"}); strings.ContainsAny(pair[len(pair)-1], "<>") {
		t.Errorf("the pairing banner body is not escaped: %q", pair)
	}
}

func pinCommand(d discovery.Device) []string { return []string{"/usr/bin/castr", "pin", d.ID} }

// hintValue returns the JSON argv carried in Omarchy's exec hint, or nil.
func hintValue(t *testing.T, argv []string) []string {
	t.Helper()
	for i, a := range argv {
		prefix := "string:" + ExecArgvHint + ":"
		if a == "-h" && i+1 < len(argv) && strings.HasPrefix(argv[i+1], prefix) {
			var out []string
			if err := json.Unmarshal([]byte(strings.TrimPrefix(argv[i+1], prefix)), &out); err != nil {
				t.Fatalf("the exec hint is not a JSON argv: %v", err)
			}
			return out
		}
	}
	return nil
}

// Omarchy draws no action buttons and invokes only "default" -- and only while
// the sender is still running. The exec hint is what makes a click do
// something there: its shell runs the argv itself.
func TestOnOmarchyClickingTheBannerRunsTheCodePrompt(t *testing.T) {
	got := hintValue(t, PairArgv("x", pinCommand(tv())))
	want := pinCommand(tv())
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("exec hint = %q, want %q", got, want)
	}
}

// Elsewhere, clicking a banner invokes "default", and button-drawing daemons
// show the labelled one.
func TestElsewhereTheBannerIsClickableAndHasAButton(t *testing.T) {
	argv := strings.Join(PairArgv("x", pinCommand(tv())), " ")
	for _, want := range []string{
		"--action=" + DefaultAction + "=Enter code",
		"--action=" + PairAction + "=Enter code",
		"--wait",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv %q is missing %q", argv, want)
		}
	}
}

// The device id is advertised by the receiver. It goes into a JSON array the
// shell executes, so it must not be able to add elements or end the array.
func TestAHostileDeviceIdCannotBreakOutOfTheHint(t *testing.T) {
	hostile := tv()
	hostile.ID = `x", "--evil", "y`
	got := hintValue(t, PairArgv("x", pinCommand(hostile)))
	if len(got) != 3 || got[0] != "/usr/bin/castr" || got[2] != hostile.ID {
		t.Fatalf("hint = %q; the id escaped its own element", got)
	}
}

func startedWith(n *Notifier) chan []string {
	started := make(chan []string, 2)
	n.Start = func(argv []string) { started <- argv }
	return started
}

func TestClickingThePairingBannerOpensTheCodePrompt(t *testing.T) {
	for _, choice := range []string{DefaultAction, PairAction} {
		n := Notifier{
			PairCommand: pinCommand,
			Prompt:      func([]string) (string, error) { return choice + "\n", nil },
		}
		started := startedWith(&n)
		n.OnState(tv(), session.AwaitingPin, "")

		select {
		case argv := <-started:
			if strings.Join(argv, " ") != strings.Join(pinCommand(tv()), " ") {
				t.Errorf("%s: started %q, want the code prompt", choice, argv)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: clicking opened nothing", choice)
		}
	}
}

// Dismissed, expired -- or clicked on Omarchy, where the hint has already run
// the prompt and notify-send prints nothing. Opening another would be two.
func TestADismissedBannerOpensNothing(t *testing.T) {
	done := make(chan struct{})
	n := Notifier{
		PairCommand: pinCommand,
		Prompt:      func([]string) (string, error) { defer close(done); return "", nil },
	}
	started := startedWith(&n)
	n.OnState(tv(), session.AwaitingPin, "")

	<-done
	select {
	case argv := <-started:
		t.Fatalf("a banner nobody clicked started %q", argv)
	case <-time.After(100 * time.Millisecond):
	}
}

// With no way to wait for a click the banner still goes out -- and on Omarchy
// it still works, because the hint needs no waiting.
func TestWithNoWayToWaitTheBannerStillCarriesTheHint(t *testing.T) {
	var shown []string
	n := Notifier{
		Run:         func(argv []string) error { shown = argv; return nil },
		PairCommand: pinCommand,
	}
	n.OnState(tv(), session.AwaitingPin, "")

	if hintValue(t, shown) == nil {
		t.Fatalf("banner %q lost the exec hint", shown)
	}
	// Run waits for notify-send to exit, and --wait makes it stay until the
	// banner is answered. Sent through Run, it would stall the daemon behind
	// a prompt nobody has seen yet.
	for _, a := range shown {
		if a == "--wait" {
			t.Fatal("a banner sent with nothing waiting asks notify-send to wait")
		}
	}
}

// Built without a command at all, castr still says where the code goes.
func TestWithNoCommandThePairingBannerIsStillShown(t *testing.T) {
	var shown []string
	n := Notifier{Run: func(argv []string) error { shown = argv; return nil }}
	n.OnState(tv(), session.AwaitingPin, "")

	if len(shown) == 0 {
		t.Fatal("no banner at all")
	}
}

// The prompt waits for the user, which can be minutes. The daemon has casts to
// run meanwhile, so the state hook must return at once.
func TestAnUnansweredPairingBannerDoesNotHoldUpTheDaemon(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	n := Notifier{
		PairCommand: pinCommand,
		Prompt:      func([]string) (string, error) { <-block; return "", nil },
		Start:       func([]string) {},
	}

	returned := make(chan struct{})
	go func() { n.OnState(tv(), session.AwaitingPin, ""); close(returned) }()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("OnState blocked until the banner was answered")
	}
}
