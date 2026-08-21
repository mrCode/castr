package ui

import (
	"testing"

	"github.com/mrCode/castr/internal/discovery"
)

// Offering a mode that cannot work turns a menu into a way to reach a failure
// notification.
func TestAChromecastIsOfferedMirrorOnly(t *testing.T) {
	got := ModeEntriesFor(discovery.ProtocolChromecast)
	if len(got) != 1 || got[0] != MirrorEntry {
		t.Errorf("got %v, want just the mirror entry", got)
	}
}

func TestAnAirPlayReceiverIsStillOfferedBoth(t *testing.T) {
	if got := ModeEntriesFor(discovery.ProtocolAirPlay); len(got) != 2 {
		t.Errorf("got %v, want both modes", got)
	}
}

// An unknown protocol gets the full menu rather than an empty one: a receiver
// castr cannot classify should not become unusable.
func TestAnUnknownProtocolGetsTheFullMenu(t *testing.T) {
	if got := ModeEntriesFor("something-new"); len(got) != 2 {
		t.Errorf("got %v, want both modes", got)
	}
}
