package notify

import (
	"errors"
	"strings"
	"testing"

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

func TestOnlyAFailureIsUrgent(t *testing.T) {
	// Urgent means sticky on mako -- a banner that never expires and has to be
	// clicked away. A cast that dies while the user is working is worth that.
	// A PIN prompt, which they are already waiting on, is not.
	if _, urgent, _ := Message(tv(), session.Failed, "gone"); !urgent {
		t.Error("a mid-stream failure was not urgent")
	}
	if _, urgent, _ := Message(tv(), session.AwaitingPin, ""); urgent {
		t.Error("a PIN prompt was sticky; the user is already watching for it")
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
