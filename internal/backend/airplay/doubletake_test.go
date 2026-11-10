package airplay

import (
	"strings"
	"testing"

	"github.com/mrCode/castr/internal/discovery"
)

func config() Config {
	return Config{
		PortRange: "60000-60010", FPS: 30, Encoder: "vaapi",
		TargetLatencyMS: 100, Audio: true,
	}
}

func argvString(argv []string) string { return strings.Join(argv, " ") }

func TestNeverUsesDaemonMode(t *testing.T) {
	// doubletake's daemon config carries no port fields, so -port-range is
	// silently dropped and the receiver's reverse handshake lands on random
	// ephemeral ports that a default-DROP firewall discards. Measured: the
	// same Apple TV stalls under -daemonize and mirrors fine under -target.
	got := argvString(BuildArgv(config(), "", "10.0.0.5", 7000, "", ""))

	if strings.Contains(got, "-daemonize") {
		t.Fatalf("argv uses daemon mode: %q", got)
	}
	if !strings.Contains(got, "-target 10.0.0.5") {
		t.Errorf("argv = %q, want -target", got)
	}
}

func TestPortRangeIsAlwaysPassed(t *testing.T) {
	// Without it the receiver connects back on ephemeral ports no firewall
	// rule can anticipate, and AirPlay hangs with no error.
	got := argvString(BuildArgv(config(), "", "10.0.0.5", 7000, "", ""))

	if !strings.Contains(got, "-port-range 60000-60010") {
		t.Errorf("argv = %q, want the configured port range", got)
	}
}

func TestEncoderVocabularyIsTranslated(t *testing.T) {
	cases := map[string]string{
		"auto": "auto", "vaapi": "vaapi", "nvenc": "nvenc",
		"x264":     "none", // castr calls it x264; doubletake calls it none
		"nonsense": "auto", // never pass through an unknown value
	}

	for encoder, want := range cases {
		cfg := config()
		cfg.Encoder = encoder

		got := argvString(BuildArgv(cfg, "", "10.0.0.5", 7000, "", ""))

		if !strings.Contains(got, "-hwaccel "+want) {
			t.Errorf("encoder %q gave %q, want -hwaccel %s", encoder, got, want)
		}
	}
}

func TestOptionalFlagsAreOmittedWhenUnset(t *testing.T) {
	cfg := config()
	cfg.Bitrate = 0

	got := argvString(BuildArgv(cfg, "", "10.0.0.5", 7000, "", ""))

	if strings.Contains(got, "-bitrate") {
		t.Errorf("argv = %q, want no -bitrate when it is zero", got)
	}
	if strings.Contains(got, "-creds") {
		t.Errorf("argv = %q, want no -creds when none is given", got)
	}
}

func TestAudioIsDisabledOnlyWhenAskedFor(t *testing.T) {
	cfg := config()
	cfg.Audio = false

	if !strings.Contains(argvString(BuildArgv(cfg, "", "10.0.0.5", 7000, "", "")), "-no-audio") {
		t.Error("want -no-audio when audio is off")
	}
	if strings.Contains(argvString(BuildArgv(config(), "", "10.0.0.5", 7000, "", "")), "-no-audio") {
		t.Error("want audio streamed by default")
	}
}

// Each cast's sound is routed to a source of its own, and the helper must
// capture that one: left to itself it captures the default output's monitor,
// which is every cast's sound at once, or none of it once castr has moved the
// cast away from the default.
func TestTheCastsAudioSourceIsPassedToTheHelper(t *testing.T) {
	got := argvString(BuildArgv(config(), "", "10.0.0.5", 7000, "", "castr_cast_1.monitor"))
	if !strings.Contains(got, "-audio-source castr_cast_1.monitor") {
		t.Errorf("argv = %q, want the cast's audio source", got)
	}

	off := config()
	off.Audio = false
	if got := argvString(BuildArgv(off, "", "10.0.0.5", 7000, "", "castr_cast_1.monitor")); strings.Contains(got, "-audio-source") {
		t.Errorf("argv = %q, want no audio source when audio is off", got)
	}
	if got := argvString(BuildArgv(config(), "", "10.0.0.5", 7000, "", "")); strings.Contains(got, "-audio-source") {
		t.Errorf("argv = %q, want no audio source when there is none", got)
	}
}

func TestCredentialsPathIsPassedWhenGiven(t *testing.T) {
	// Each mode captures a different virtual output, so each needs its own
	// token. Reusing the default would replay one pointing at the real panel.
	got := argvString(BuildArgv(config(), "", "10.0.0.5", 7000, "/state/mirror.json", ""))

	if !strings.Contains(got, "-creds /state/mirror.json") {
		t.Errorf("argv = %q, want the credentials path", got)
	}
}

// -- output scanning --------------------------------------------------------

func TestSessionReadyIsNotStreaming(t *testing.T) {
	// The four-second trap: this line arrives before the capture pipeline runs
	// and before the share prompt has necessarily been answered.
	var s Scanner
	s.Absorb("connected to: Meeting Room\n")
	s.Absorb("FairPlay setup complete\n")
	s.Absorb("mirror session ready (data port: 49277)\n")

	if s.Ready() {
		t.Error("'mirror session ready' must not count as streaming")
	}
}

func TestCaptureStartedIsStreaming(t *testing.T) {
	var s Scanner
	s.Absorb("mirror session ready (data port: 49277)\n")
	s.Absorb("screen capture started\n")

	if !s.Ready() {
		t.Error("'screen capture started' means pixels are flowing")
	}
}

func TestPinPromptIsSeenWithoutANewline(t *testing.T) {
	// doubletake prints it with fmt.Print, so a line-based scanner never sees
	// it and the session hangs until the ready timeout.
	var s Scanner
	s.Absorb("pairing required. " + PinPrompt + ": ")

	if !s.NeedsPin() {
		t.Error("the PIN prompt was missed because it has no trailing newline")
	}
}

func TestEveryDoubletakePromptWordingIsSeen(t *testing.T) {
	// doubletake 0.5.0 reworded the prompt ("the receiver" rather than
	// "Apple TV") and added password variants. Observed against a real Apple
	// TV: the television showed a code, castr sat in connecting, and
	// `castr pin` answered "is not waiting for a PIN".
	for _, prompt := range PinPrompts {
		var s Scanner
		s.Absorb("[PAIR] pair-setup\n" + prompt + ": ")
		if !s.NeedsPin() {
			t.Errorf("prompt %q was not recognised", prompt)
		}
	}
}

func TestOrdinaryOutputIsNotAPinPrompt(t *testing.T) {
	var s Scanner
	s.Absorb("searching for Apple TVs...\nmirror session ready (data port: 49217)\n")
	if s.NeedsPin() {
		t.Error("ordinary progress output was taken for a PIN prompt")
	}
}

func TestPortalFailureIsReportedVerbatim(t *testing.T) {
	// castr used to discard this and guess, and the guess blamed the firewall
	// while doubletake had already printed the real reason.
	var s Scanner
	s.Absorb("mirror session ready (data port: 49217)\n")
	s.Absorb("screen capture failed: screencast portal: session response: " +
		"timeout waiting for portal response\n")

	got := s.PortalFailure()

	if !strings.Contains(got, "screencast portal") {
		t.Errorf("PortalFailure() = %q, want doubletake's own message", got)
	}
}

func TestNoPortalFailureWhenThingsAreFine(t *testing.T) {
	var s Scanner
	s.Absorb("screen capture started\naudio capture started\n")

	if got := s.PortalFailure(); got != "" {
		t.Errorf("PortalFailure() = %q, want empty", got)
	}
}

func TestMarkersSplitAcrossChunksAreStillFound(t *testing.T) {
	// Output arrives in arbitrary chunks from a pipe, not tidy lines.
	var s Scanner
	s.Absorb("screen capt")
	s.Absorb("ure started\n")

	if !s.Ready() {
		t.Error("a marker split across reads was missed")
	}
}

func TestOutputIsBounded(t *testing.T) {
	// A chatty child must not grow the buffer without limit.
	var s Scanner
	for i := 0; i < 500; i++ {
		s.Absorb(strings.Repeat("noise ", 100))
	}

	if len(s.text) > maxBuffer {
		t.Errorf("buffer = %d bytes, want at most %d", len(s.text), maxBuffer)
	}
}

func TestReadinessSurvivesBufferTrimming(t *testing.T) {
	// Trimming must not silently drop a marker the caller still needs. The
	// recent tail is what matters, so a late marker stays visible.
	var s Scanner
	s.Absorb(strings.Repeat("noise ", 5000))
	s.Absorb(ReadyMarker + "\n")

	if !s.Ready() {
		t.Error("the ready marker was trimmed away")
	}
}

func TestTailIsWhitespaceNormalised(t *testing.T) {
	var s Scanner
	s.Absorb("error:   something\n\n  went    wrong\n")

	if got := s.Tail(200); strings.Contains(got, "\n") || strings.Contains(got, "  ") {
		t.Errorf("Tail() = %q, want a flat single-spaced line", got)
	}
}

// Reported against a REDMI display (issue #2): it advertises AirPlay on port
// 47000 and refuses connections on 7000. castr discovered the port and never
// passed it on, so doubletake dialled its default of 7000 and the cast failed
// before pairing even began.
func TestTheDiscoveredPortReachesDoubletake(t *testing.T) {
	argv := strings.Join(BuildArgv(Config{PortRange: "60000-60010", FPS: 30}, "", "192.168.1.50", 47000, "", ""), " ")
	if !strings.Contains(argv, "-port 47000") {
		t.Errorf("argv %q does not carry the receiver's port", argv)
	}
}

// A receiver whose port castr never learned leaves doubletake on its own
// default rather than dialling port 0.
func TestAnUnknownPortLeavesDoubletakesDefault(t *testing.T) {
	for _, argv := range BuildArgv(Config{PortRange: "60000-60010", FPS: 30}, "", "192.168.1.50", 0, "", "") {
		if argv == "-port" {
			t.Fatal("an unknown port was passed to doubletake")
		}
	}
}

// -port is not -port-range. The receiver's own port and the range it calls
// back into are different things, and confusing them would break both.
func TestThePortIsNotConfusedWithThePortRange(t *testing.T) {
	argv := BuildArgv(Config{PortRange: "60000-60010", FPS: 30}, "", "192.168.1.50", 47000, "", "")
	for i, a := range argv {
		if a == "-port-range" && (i+1 >= len(argv) || argv[i+1] != "60000-60010") {
			t.Errorf("the port range became %q", argv[i+1])
		}
	}
}

// castr-airplay refuses a device at the address that names itself as some
// other receiver while the one castr means has a saved pairing -- but only if
// castr says which receiver it means. Without -device-id an impersonator could
// name itself as a receiver never seen before and be paired with from scratch.
func TestTheReceiversOwnDeviceIDIsPassed(t *testing.T) {
	argv := BuildArgv(config(), "airplay:AA:BB:CC:DD:EE:FF", "10.0.0.5", 7000, "", "")
	if !strings.Contains(argvString(argv), "-device-id AA:BB:CC:DD:EE:FF") {
		t.Fatalf("argv %q does not name the receiver meant", argv)
	}
}

// A receiver that advertised no device ID is known to castr by its address,
// and an address says nothing about which device answers there.
//
// A receiver the user added by address is the same case under another name:
// castr calls it "airplay:manual:<address>", and no pairing is ever saved
// under "manual:<address>". Passing that would leave the helper looking for a
// receiver that does not exist, and hide the warning it gives when it cannot
// tell which receiver is meant. Without a device ID it binds the receiver to
// its address itself.
func TestNoDeviceIDIsInventedFromAnAddressOrAnotherProtocol(t *testing.T) {
	for _, tc := range []struct{ id, address string }{
		{"airplay:10.0.0.5", "10.0.0.5"},
		{"airplay:", "10.0.0.5"},
		{"", "10.0.0.5"},
		{"chromecast:8a308a938ce275d9", "10.0.0.5"},
		{"airplay:manual:10.0.0.5", "10.0.0.5"},
		// The id keeps the address it was added with, even once the
		// receiver has been reached somewhere else.
		{"airplay:manual:10.0.0.5", "10.0.0.9"},
		{"airplay:manual:", "10.0.0.5"},
	} {
		if got := argvString(BuildArgv(config(), tc.id, tc.address, 7000, "", "")); strings.Contains(got, "-device-id") {
			t.Errorf("id %q at %s: argv %q passes a device ID", tc.id, tc.address, got)
		}
	}
}

// Nothing on PATH stands in for castr's own helper: a doubletake found there
// need not authenticate receivers at all.
func TestTheHelperIsCastrsOwnNotWhateverIsOnPath(t *testing.T) {
	if got := binary(func(string) string { return "" }); got != "/usr/lib/castr/castr-airplay" {
		t.Errorf("helper = %q, want castr's own /usr/lib/castr/castr-airplay", got)
	}
	override := func(k string) string {
		if k == "CASTR_AIRPLAY" {
			return "/home/me/src/doubletake/castr-airplay"
		}
		return ""
	}
	if got := binary(override); got != "/home/me/src/doubletake/castr-airplay" {
		t.Errorf("with CASTR_AIRPLAY set, helper = %q", got)
	}
	if argv := BuildArgv(config(), "", "10.0.0.5", 7000, "", ""); argv[0] != Binary {
		t.Errorf("argv[0] = %q, want the helper %q", argv[0], Binary)
	}
}

func latencyArg(cfg Config) string {
	argv := BuildArgv(cfg, "", "10.0.0.5", 7000, "", "")
	for i, a := range argv {
		if a == "-target-latency-ms" && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

func TestUnsetLatencyIsChosenPerCast(t *testing.T) {
	cfg := config()
	cfg.TargetLatencyMS = 0

	cfg.Audio = true
	if got := latencyArg(cfg); got != "150" {
		t.Errorf("unset with sound: -target-latency-ms = %q, want 150", got)
	}
	cfg.Audio = false
	if got := latencyArg(cfg); got != "100" {
		t.Errorf("unset without sound: -target-latency-ms = %q, want 100", got)
	}
}

func TestExplicitLatencyIsUsedExactly(t *testing.T) {
	for _, audio := range []bool{true, false} {
		cfg := config()
		cfg.TargetLatencyMS = 120
		cfg.Audio = audio
		if got := latencyArg(cfg); got != "120" {
			t.Errorf("audio=%v: -target-latency-ms = %q, want 120", audio, got)
		}
	}
}

// Defaults change nothing: with no codec or geometry configured the helper is
// started exactly as before, and picks its own codec and canvas.
func TestTheCodecAndCapAreForwardedOnlyWhenSet(t *testing.T) {
	plain := argvString(BuildArgv(config(), "", "10.0.0.5", 7000, "", ""))
	if strings.Contains(plain, "-video-codec") || strings.Contains(plain, "-max-size") {
		t.Errorf("argv = %q, want no -video-codec or -max-size by default", plain)
	}

	auto := config()
	auto.Codec = "auto"
	if got := argvString(BuildArgv(auto, "", "10.0.0.5", 7000, "", "")); strings.Contains(got, "-video-codec") {
		t.Errorf("argv = %q, want auto left to the helper", got)
	}

	for _, codec := range []string{"h264", "hevc"} {
		cfg := config()
		cfg.Codec = codec
		if got := argvString(BuildArgv(cfg, "", "10.0.0.5", 7000, "", "")); !strings.Contains(got, "-video-codec "+codec) {
			t.Errorf("argv = %q, want -video-codec %s", got, codec)
		}
	}

	capped := config()
	capped.MaxSize = "2560x1440"
	if got := argvString(BuildArgv(capped, "", "10.0.0.5", 7000, "", "")); !strings.Contains(got, "-max-size 2560x1440") {
		t.Errorf("argv = %q, want -max-size 2560x1440", got)
	}
}

// Real refusals, as castr-airplay (doubletake branch castr) prints them: the
// format strings in cmd/doubletake/main.go and internal/airplay, filled in.
var refusals = map[string]string{
	"identity": "2026/11/10 20:10:42 receiver did not prove its pairing identity: pair-verify " +
		"V2 signature does not verify; refusing to cast. The device answering " +
		"as \"Basem's Office\" is not the receiver this machine paired with, or will not prove it. " +
		"If you reset or replaced that receiver, run again with -pair and enter the code it shows; " +
		"a receiver that shows no code is not the one you paired with.\n",
	"password": "2026/11/10 20:10:42 the saved pairing was made with the receiver's pairing " +
		"password, which is not sent again unless asked; refusing to pair with \"TV\" again " +
		"automatically. If it is the receiver you paired with, run again with -pair to pair with it again.\n",
	"streamkey": "2026/11/10 20:10:42 mirror setup failed: no protected way to give the receiver " +
		"the video key: the control channel is not encrypted and the receiver offered no FairPlay " +
		"key; run with -no-encrypt to cast to it without video encryption\n",
	"unstable": "2026/11/10 20:10:42 the receiver named itself \"AA:BB\", then \"CC:DD\" after " +
		"reconnecting; refusing to cast\n",
	"saved": "2026/11/10 20:10:42 pair-verify with saved creds failed: connection reset; refusing " +
		"to replace the saved pairing with an unauthenticated one. If you reset or replaced that " +
		"receiver, remove its entry from /x/extend.json and connect again.\n",
	"nopin": "2026/11/10 20:10:42 pairing failed: the receiver will not show a PIN to pair again " +
		"with (403 Forbidden); if this receiver uses a pairing password rather than an on-screen " +
		"code, remove its entry from the credentials file and connect again\n",
	"raw": "2026/11/10 20:10:42 receiver did not prove its pairing identity: it was paired with " +
		"without a code, but now asks for one; if the receiver really changed, remove its entry " +
		"from the credentials file and connect again; refusing to cast\n",
	"impostor": "2026/11/10 20:10:42 receiver did not prove its pairing identity: the device at " +
		"this address names itself \"11:11:11:11:11:11\", not \"FE:5C:81:22:50:38\", which this " +
		"machine paired with (if \"FE:5C:81:22:50:38\" has left and another receiver now has its " +
		"address, discover again); refusing to cast. FE:5C:81:22:50:38 is the receiver this machine " +
		"paired with at 10.10.10.231; if a different receiver now has that address, pair with it " +
		"explicitly: run again with -pair and enter the code it shows.\n",
	"tie": "2026/11/10 20:10:42 receiver did not prove its pairing identity: the device at " +
		"10.10.10.231 names itself \"CC:CC\", but this machine paired there with AA:AA and BB:BB, " +
		"recorded as paired at the same time, and it is none of them; remove the stale entry from " +
		"the credentials file; refusing to cast\n",
	"unproven": "2026/11/10 20:10:42 pairing failed: receiver did not prove its pairing identity: " +
		"pair-setup M6 carried no receiver identity\n",
}

func TestEachRefusalIsRecognised(t *testing.T) {
	want := map[string]refusal{
		"identity": refusalIdentity, "password": refusalPassword, "streamkey": refusalStreamKey,
		"unstable": refusalUnstable, "saved": refusalSavedPairing, "nopin": refusalNoPIN,
		"raw": refusalIdentity, "impostor": refusalImpostor, "tie": refusalImpostor,
		"unproven": refusalUnproven,
	}
	for name, line := range refusals {
		var s Scanner
		s.Absorb("connected to: TV (model: AppleTV11,1)\n" + line)
		kind, detail := s.Refusal()
		if kind != want[name] {
			t.Errorf("%s: kind = %v, want %v", name, kind, want[name])
		}
		if detail == "" || strings.HasPrefix(detail, "2026/") {
			t.Errorf("%s: detail = %q, want the reason without the log timestamp", name, detail)
		}
	}
}

func TestOrdinaryOutputIsNoRefusal(t *testing.T) {
	var s Scanner
	s.Absorb("connected to: TV\nusing saved credentials (file)\nmirror session ready\n")
	if kind, detail := s.Refusal(); kind != refusalNone || detail != "" {
		t.Errorf("Refusal() = %v, %q; want none", kind, detail)
	}
}

func TestOnlyTheLastLineCanBeARefusal(t *testing.T) {
	// The helper logs a failed pair-verify and carries on, pairing again with
	// a code. If the user then mistypes it, that is the failure -- not a
	// refused saved pairing, which would send them to pair again for nothing.
	var s Scanner
	s.Absorb("2026/11/10 20:10:42 pair-verify with saved creds failed: connection reset\n" +
		"Enter the PIN shown on the receiver: \n" +
		"2026/11/10 20:10:50 pairing failed: pair-setup M4 error: 2\n\n")
	if kind, _ := s.Refusal(); kind != refusalNone {
		t.Errorf("kind = %v, want none: the earlier line was not the end", kind)
	}
}

func device1() discovery.Device {
	return discovery.Device{ID: "airplay:FE:5C:81:22:50:38", Name: "Basem's Office", Address: "10.10.10.231"}
}

func TestNoRefusalMessageRepeatsAdviceCastrUsersCannotFollow(t *testing.T) {
	// castr runs the helper itself: its users cannot add -pair or
	// -no-encrypt, and should not hand-edit a file holding private keys.
	for name, line := range refusals {
		var s Scanner
		s.Absorb(line)
		msg := refusalMessage(device1(), &s)
		for _, banned := range []string{"-pair ", "-pair.", "-no-encrypt", "credentials file", ".json",
			"run again", "discover again", "; pair again", "/usr/lib"} {
			if strings.Contains(msg, banned) {
				t.Errorf("%s: message passes on %q: %s", name, banned, msg)
			}
		}
		if !strings.Contains(msg, "Basem's Office") {
			t.Errorf("%s: message does not name the receiver: %s", name, msg)
		}
	}
}

func TestPairingAgainIsSuggestedOnlyWhereItIsTheFix(t *testing.T) {
	for _, name := range []string{"identity", "password", "saved", "raw"} {
		var s Scanner
		s.Absorb(refusals[name])
		if msg := refusalMessage(device1(), &s); !strings.Contains(msg, "castr pair-again airplay:FE:5C:81:22:50:38") {
			t.Errorf("%s: message does not say how to pair again: %s", name, msg)
		}
	}
	// A device naming itself as another receiver is never a reset one -- a
	// reset Apple TV keeps its device ID. Suggesting pairing again there, or
	// where it would change nothing, teaches users to do it whenever a cast
	// fails.
	for _, name := range []string{"impostor", "tie", "streamkey", "unstable", "nopin", "unproven"} {
		var s Scanner
		s.Absorb(refusals[name])
		if msg := refusalMessage(device1(), &s); strings.Contains(msg, "pair-again") {
			t.Errorf("%s: message suggests pairing again: %s", name, msg)
		}
	}
}

func TestPairingAgainAdviceSaysWhenNotToEnterACode(t *testing.T) {
	// The real receiver shows the code. One that does not is not the user's,
	// and the code prompt is where they would find out.
	for _, name := range []string{"identity", "saved", "raw"} {
		var s Scanner
		s.Absorb(refusals[name])
		if msg := refusalMessage(device1(), &s); !strings.Contains(msg, "shows none") {
			t.Errorf("%s: message lost the warning: %s", name, msg)
		}
	}
}

func TestAReasonAfterAPromptLosesThePromptAndTheTimestamp(t *testing.T) {
	var s Scanner
	s.Absorb("Enter the PIN shown on the receiver: 2026/11/10 20:10:50 pairing failed: " +
		"receiver did not prove its pairing identity: pair-setup M6 carried no receiver identity\n")
	_, reason := s.Refusal()
	if !strings.HasPrefix(reason, "pairing failed:") {
		t.Errorf("reason = %q", reason)
	}
}
