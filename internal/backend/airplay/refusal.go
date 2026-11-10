package airplay

import (
	"fmt"
	"strings"

	"github.com/mrCode/castr/internal/discovery"
)

// refusal is a kind of cast castr-airplay refused on purpose -- most often
// because a receiver this machine has paired with could not prove it is that
// receiver. castr-airplay explains each one with advice for its own command
// line (run again with -pair, remove an entry from the credentials file, run
// with -no-encrypt), none of which a castr user can follow: castr runs the
// helper itself, and the file holds private keys. So castr recognises the
// refusal and says what to do in castr's own terms.
type refusal int

const (
	refusalNone refusal = iota
	// The receiver did not prove its pairing identity.
	refusalIdentity
	// The pairing was made with a pairing password, not sent again unasked.
	refusalPassword
	// The receiver offers no encrypted way to get the video key.
	refusalStreamKey
	// The receiver changed its identity between two connections.
	refusalUnstable
	// The saved pairing failed, and is not replaced without a code.
	refusalSavedPairing
	// Some other refusal: reported without the helper's advice.
	refusalOther
)

// refusalKinds maps a phrase in the helper's message to its kind, most
// specific first.
var refusalKinds = []struct {
	phrase string
	kind   refusal
}{
	{"after reconnecting; refusing", refusalUnstable},
	{"pairing password, which is not sent again", refusalPassword},
	{"no protected way to give the receiver the video key", refusalStreamKey},
	{"will not show a PIN to pair again", refusalSavedPairing},
	{"pair-verify with saved creds failed", refusalSavedPairing},
	{"did not prove its pairing identity", refusalIdentity},
	{"refusing to", refusalOther},
}

// adviceMarkers start the helper's advice within a line. The reason is what
// comes before the first of them.
var adviceMarkers = []string{"; refusing", "; if the receiver", "; if this receiver",
	"; remove ", "; run with", "; run again"}

// Refusal reports whether the helper refused the cast, and why, without its
// advice. It looks at the last line that names a refusal: the helper exits on
// it, so it is the last thing printed.
func (s *Scanner) Refusal() (refusal, string) {
	s.mu.Lock()
	lines := strings.Split(s.text, "\n")
	s.mu.Unlock()
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		for _, k := range refusalKinds {
			if strings.Contains(line, k.phrase) {
				return k.kind, refusalReason(line)
			}
		}
	}
	return refusalNone, ""
}

// refusalReason is a refusal line without its log timestamp or its advice.
func refusalReason(line string) string {
	line = strings.TrimSpace(line)
	// log's default prefix: "2026/11/10 20:10:42 ".
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
// not refuse it.
//
// Where pairing again is the fix, it says how: `castr unpair`, then a cast,
// which pairs from scratch. It keeps the helper's warning that a receiver
// showing no code is not the one paired with -- unpairing from an impostor is
// exactly what one needs. Where pairing again is no fix, it does not suggest
// it, so that nobody learns to throw a pairing away whenever a cast fails.
func refusalMessage(d discovery.Device, s *Scanner) string {
	kind, reason := s.Refusal()
	unpair := fmt.Sprintf("castr unpair %s", d.ID)
	var msg string
	switch kind {
	case refusalNone:
		return ""
	case refusalIdentity:
		msg = fmt.Sprintf("castr did not cast to %s: the device answering there did not prove it "+
			"is the receiver this computer paired with. If you reset or replaced that receiver, "+
			"run `%s` and cast again, and enter the new code it shows. If it shows no code, the "+
			"device castr reached is not your receiver: keep the pairing, and check what is at %s.",
			d.Name, unpair, d.Address)
	case refusalPassword:
		msg = fmt.Sprintf("castr did not cast to %s: it was paired with using its pairing "+
			"password, which castr does not send again without being asked. To pair again, run "+
			"`%s`, cast, and enter the password.", d.Name, unpair)
	case refusalSavedPairing:
		msg = fmt.Sprintf("castr did not cast to %s: it did not accept this computer's saved "+
			"pairing, and castr will not replace that pairing without a code. If you reset that "+
			"receiver or removed this computer from it, run `%s` and cast again.", d.Name, unpair)
	case refusalStreamKey:
		msg = fmt.Sprintf("castr did not cast to %s: it offers no encrypted way to receive the "+
			"video key, and castr will not send the key in the clear for anyone on the network "+
			"to read.", d.Name)
	case refusalUnstable:
		msg = fmt.Sprintf("castr did not cast to %s: it gave a different identity when castr "+
			"reconnected, so something else may be answering at %s. Try again, and if it happens "+
			"again, check what is at that address.", d.Name, d.Address)
	default:
		msg = fmt.Sprintf("castr did not cast to %s.", d.Name)
	}
	return fmt.Sprintf("%s (%s: %s)", msg, Binary, reason)
}
