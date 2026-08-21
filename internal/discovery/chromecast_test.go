package discovery

import (
	"errors"
	"strings"
	"testing"
)

// Exactly what the Xiaomi stick on this network publishes.
const realChromecastLine = `=;wlo1;IPv4;MiTV-AESP0-8a308a938ce275d9173a777e6003aa07;_googlecast._tcp;local;8a308a93-8ce2-75d9-173a-777e6003aa07.local;192.168.100.48;8009;"id=8a308a938ce275d9173a777e6003aa07" "cd=20CF31AAEF3480D53FFA5DF9A9B03648" "rm=1C2508D67BAE4C49" "ve=05" "md=MiTV-AESP0" "ic=/setup/icon.png" "fn=Cinema" "ca=266757" "st=0" "bs=FA8F960A4BA9" "nf=1" "ct=883290" "rs="`

func TestParseReadsARealChromecastAdvertisement(t *testing.T) {
	devices := Parse(realChromecastLine, ProtocolChromecast)
	if len(devices) != 1 {
		t.Fatalf("got %d devices, want 1", len(devices))
	}
	got := devices[0]

	// The service instance name is the hardware id. What the user named the
	// television lives in the TXT record as fn, and that is what belongs in a
	// menu.
	if got.Name != "Cinema" {
		t.Errorf("Name = %q, want %q", got.Name, "Cinema")
	}
	if got.Model != "MiTV-AESP0" {
		t.Errorf("Model = %q, want %q", got.Model, "MiTV-AESP0")
	}
	if got.Address != "192.168.100.48" || got.Port != 8009 {
		t.Errorf("address = %s:%d, want 192.168.100.48:8009", got.Address, got.Port)
	}
	if got.Protocol != ProtocolChromecast {
		t.Errorf("Protocol = %q", got.Protocol)
	}
	// A Chromecast publishes no deviceid, so an id built from the missing key
	// would collapse every receiver onto one entry.
	if !strings.HasSuffix(got.ID, "8a308a938ce275d9173a777e6003aa07") {
		t.Errorf("ID = %q, want it to carry the device's id", got.ID)
	}
}

func TestBrowseAsksForTheRightService(t *testing.T) {
	for _, tc := range []struct{ protocol, want string }{
		{ProtocolAirPlay, "_airplay._tcp"},
		{ProtocolChromecast, "_googlecast._tcp"},
	} {
		var asked string
		run := func(name string, args ...string) (string, error) {
			asked = args[len(args)-1]
			return "", nil
		}
		if _, err := Browse(run, tc.protocol); err != nil {
			t.Fatalf("Browse(%s): %v", tc.protocol, err)
		}
		if asked != tc.want {
			t.Errorf("%s browsed %q, want %q", tc.protocol, asked, tc.want)
		}
	}
}

// A network with no Chromecast on it must still list Apple TVs. Letting one
// failed browse fail the whole call would make an unrelated receiver's absence
// hide the ones that are there.
func TestBrowseAllSurvivesOneProtocolFailing(t *testing.T) {
	run := func(name string, args ...string) (string, error) {
		if args[len(args)-1] == googlecastService {
			return "", errors.New("no such service")
		}
		return `=;wlo1;IPv4;Apple TV;_airplay._tcp;local;a.local;10.10.10.231;7000;"deviceid=AA" "model=AppleTV11,1"`, nil
	}

	devices, err := BrowseAll(run)
	if err != nil {
		t.Fatalf("BrowseAll: %v", err)
	}
	if len(devices) != 1 || devices[0].Protocol != ProtocolAirPlay {
		t.Fatalf("got %+v, want the one Apple TV", devices)
	}
}

func TestBrowseAllFailsOnlyWhenNothingCanBeBrowsed(t *testing.T) {
	run := func(string, ...string) (string, error) { return "", errors.New("avahi is not running") }
	if _, err := BrowseAll(run); err == nil {
		t.Fatal("BrowseAll succeeded with no working browser")
	}
}

func TestBrowseAllReturnsBothProtocols(t *testing.T) {
	run := func(name string, args ...string) (string, error) {
		if args[len(args)-1] == googlecastService {
			return realChromecastLine, nil
		}
		return `=;wlo1;IPv4;Apple TV;_airplay._tcp;local;a.local;10.10.10.231;7000;"deviceid=AA" "model=AppleTV11,1"`, nil
	}

	devices, err := BrowseAll(run)
	if err != nil {
		t.Fatalf("BrowseAll: %v", err)
	}
	if len(devices) != 2 {
		t.Fatalf("got %d devices, want 2: %+v", len(devices), devices)
	}
}
