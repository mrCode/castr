// Package sound decides where a cast's sound plays: on the receiver (TV), on
// the laptop, or on both.
//
// While anything is cast with sound, all laptop sound is routed into castr's
// own virtual output, castr_mix, which is made the default. Each cast gets its
// own output, castr_cast_<n>, whose monitor its receiver captures. Loopbacks
// copy castr_mix into each cast's output and into the real output; each is
// loaded once, and its stream is muted while its end should be silent: a
// cast's unless the cast is on TV or Both, the laptop's unless any cast is on
// Laptop or Both. Switching only mutes and unmutes those streams, so the
// graph is never re-linked and the receiver's capture -- and the picture --
// never restarts.
//
// The laptop is never silenced by muting it: a muted sink's monitor carries
// silence too, measured on PipeWire. Only castr's own loopback streams are
// ever muted.
package sound

import (
	"fmt"
	"strings"
)

// Choice is where one cast's sound plays.
type Choice string

const (
	TV     Choice = "tv"
	Laptop Choice = "laptop"
	Both   Choice = "both"
)

// ParseChoice accepts tv, laptop or both, in any case.
func ParseChoice(s string) (Choice, error) {
	switch c := Choice(strings.ToLower(s)); c {
	case TV, Laptop, Both:
		return c, nil
	}
	return "", fmt.Errorf("unknown sound choice %q: choose tv, laptop or both", s)
}

// toReceiver reports whether the cast's receiver gets the sound.
func (c Choice) toReceiver() bool { return c == TV || c == Both }

// toLaptop reports whether the laptop plays the sound.
func (c Choice) toLaptop() bool { return c == Laptop || c == Both }
