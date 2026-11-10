package airplay

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/mrCode/castr/internal/discovery"
)

// refusal is a kind of cast castr-airplay refused on purpose -- most often
// because a receiver this machine has paired with could not prove it is that
// receiver, which is the defence against a device on the network answering in
// its place. castr-airplay explains each one with advice for its own command
// line (run again with -pair, remove an entry from the credentials file, run
// with -no-encrypt), none of which a castr user can follow. So castr
// recognises the refusal and says what to do in castr's own terms.
type refusal int

const (
	refusalNone refusal = iota
	// The receiver did not prove its pairing identity: reset or replaced,
	// or not the receiver at all.
	refusalIdentity
	// The device names itself as a receiver other than the one paired with.
	// A reset Apple TV keeps its device ID, so this is never a reset one.
	refusalImpostor
	// The pairing was made with a pairing password, not sent again unasked.
	refusalPassword
	// The receiver offers no encrypted way to get the video key.
	refusalStreamKey
	// The receiver changed its identity between two connections.
	refusalUnstable
	// The saved pairing failed, and is not replaced without a code.
	refusalSavedPairing
	// The receiver will not show a PIN to pair again with.
	refusalNoPIN
	// Pairing with a code finished without the receiver proving itself.
	refusalUnproven
	// Some other refusal: reported without the helper's advice.
	refusalOther
)

// refusalKinds maps a phrase in the helper's final line to its kind, most
// specific first: several wrap the same "did not prove" error.
var refusalKinds = []struct {
	phrase string
	kind   refusal
}{
	{"after reconnecting; refusing", refusalUnstable},
	{"names itself", refusalImpostor},
	{"pairing password, which is not sent again", refusalPassword},
	{"no protected way to give the receiver the video key", refusalStreamKey},
	{"will not show a PIN to pair again", refusalNoPIN},
	{"refusing to replace the saved pairing", refusalSavedPairing},
	{"pairing failed: receiver did not prove", refusalUnproven},
	{"did not prove its pairing identity", refusalIdentity},
	{"refusing to", refusalOther},
}

// adviceMarkers start the helper's advice within a line. The reason is what
// comes before the first of them.
var adviceMarkers = []string{"; refusing", "; if the receiver", "; if this receiver",
	"; remove ", "; run with", "; run again", "; pair again", " (if "}

// Refusal reports whether the helper refused the cast, and why, without its
// advice. Only its final line counts: the helper exits on a refusal, so it is
// the last thing printed, while earlier lines may be failures it recovered
// from -- a saved pairing that failed verification is logged, and then paired
// with again by code.
func (s *Scanner) Refusal() (refusal, string) {
	s.mu.Lock()
	text := strings.TrimRight(s.text, " \t\r\n")
	s.mu.Unlock()
	line := text[strings.LastIndex(text, "\n")+1:]
	for _, k := range refusalKinds {
		if strings.Contains(line, k.phrase) {
			return k.kind, refusalReason(line)
		}
	}
	return refusalNone, ""
}

// refusalReason is a refusal line without its log timestamp or its advice.
func refusalReason(line string) string {
	line = strings.TrimSpace(line)
	// log's default prefix, which the helper keeps: "2026/11/10 20:10:42 ".
	if f := strings.SplitN(line, " ", 3); len(f) == 3 && strings.Count(f[0], "/") == 2 &&
		strings.Count(f[1], ":") == 2 {
		line = f[2]
	}
	cut := len(line)
	for _, m := range adviceMarkers {
		if i := strings.Index(line, m); i >= 0 && i < cut {
			cut = i
		}
	}
	return strings.TrimSpace(line[:cut])
}

// refusalMessage explains a refused cast, or returns "" when the helper did
// not refuse it. Call it only once the helper has exited.
//
// Where the fix is pairing again, it says how -- `castr pair-again`, which
// makes the next cast pair with a code while keeping the saved pairing's
// device ID -- and that the code must appear on the receiver: one that shows
// none is not the user's, and no code should be entered. Where pairing again
// is no fix, above all where a device names itself as another receiver, it
// does not suggest it, so that nobody learns to do it whenever a cast fails.
func refusalMessage(d discovery.Device, s *Scanner) string {
	kind, reason := s.Refusal()
	pairAgain := fmt.Sprintf("castr pair-again %s", d.ID)
	const onlyIfShown = " The receiver then shows a code: enter it only if it appears on your " +
		"receiver. If it shows none, something else answered -- enter nothing, and stop the cast."
	var msg string
	switch kind {
	case refusalNone:
		return ""
	case refusalIdentity:
		msg = fmt.Sprintf("castr did not cast to %s: the device answering did not prove it is the "+
			"receiver this computer paired with. If you reset that receiver, run `%s` and cast "+
			"again.", d.Name, pairAgain) + onlyIfShown
	case refusalSavedPairing:
		msg = fmt.Sprintf("castr did not cast to %s: it did not accept this computer's saved "+
			"pairing, and castr will not replace that pairing without a code. If you reset that "+
			"receiver or removed this computer from it, run `%s` and cast again.",
			d.Name, pairAgain) + onlyIfShown
	case refusalPassword:
		msg = fmt.Sprintf("castr did not cast to %s: it was paired with using its pairing "+
			"password, which castr does not send again without being asked. If you reset that "+
			"receiver or changed its password, run `%s`, cast, and enter the password -- only if "+
			"you know it is your receiver at %s, since a device pretending to be it could try "+
			"to work the password out.", d.Name, pairAgain, d.Address)
	case refusalImpostor:
		msg = fmt.Sprintf("castr did not cast to %s: the device at %s names itself as a different "+
			"receiver from the one this computer paired with there. A reset receiver keeps its "+
			"name, so something else is answering at that address; check what it is.",
			d.Name, d.Address)
	case refusalStreamKey:
		msg = fmt.Sprintf("castr did not cast to %s: it offers no encrypted way to receive the "+
			"video key, and castr will not send the key in the clear for anyone on the network "+
			"to read.", d.Name)
	case refusalUnstable:
		msg = fmt.Sprintf("castr did not cast to %s: it gave a different identity when castr "+
			"reconnected, so something else may be answering at %s. Try again, and if it happens "+
			"again, check what is at that address.", d.Name, d.Address)
	case refusalNoPIN:
		msg = fmt.Sprintf("castr did not cast to %s: it would not show a code to pair again with. "+
			"If it now takes a pairing password instead of an on-screen code, castr cannot pair "+
			"with it again yet.", d.Name)
	case refusalUnproven:
		msg = fmt.Sprintf("castr did not finish pairing with %s: it did not prove its identity "+
			"while pairing, so nothing was saved and nothing was cast.", d.Name)
	default:
		msg = fmt.Sprintf("castr did not cast to %s.", d.Name)
	}
	return fmt.Sprintf("%s (%s: %s)", msg, filepath.Base(Binary), reason)
}
