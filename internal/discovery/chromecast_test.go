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

// A receiver's name is an unauthenticated TXT record, and avahi's \NNN escapes
// decode any byte -- including a newline. The menu joins its entries with
// newlines, so a name carrying one adds LINES to the menu, which lets whoever
// advertised it forge the trailing "[id]" the selection is parsed back out of.
func TestADeviceNameCannotInjectExtraMenuLines(t *testing.T) {
	hostile := `=;wlo1;IPv4;evil;_googlecast._tcp;local;e.local;192.168.100.90;8009;` +
		`"id=deadbeef" "fn=Living Room\010Stop casting (Living Room)" "md=Ultra\010\010"`

	devices := Parse(hostile, ProtocolChromecast)
	if len(devices) != 1 {
		t.Fatalf("got %d devices, want 1", len(devices))
	}
	for _, field := range []string{devices[0].Name, devices[0].Model} {
		if strings.ContainsAny(field, "\n\r\x00") {
			t.Errorf("control characters survived into %q", field)
		}
	}
}

// Entries are passed as arguments to the menu program, where a leading dash
// reads as an option rather than as a receiver.
func TestADeviceNameCannotBeginWithADash(t *testing.T) {
	line := `=;wlo1;IPv4;evil;_googlecast._tcp;local;e.local;192.168.100.90;8009;` +
		`"id=deadbeef" "fn=--help" "md=x"`

	devices := Parse(line, ProtocolChromecast)
	if len(devices) != 1 {
		t.Fatalf("got %d devices", len(devices))
	}
	if strings.HasPrefix(devices[0].Name, "-") {
		t.Errorf("Name = %q, which the menu program reads as an option", devices[0].Name)
	}
}

func TestADeviceNameCannotFillTheMenu(t *testing.T) {
	long := strings.Repeat("A", 500)
	line := `=;wlo1;IPv4;evil;_googlecast._tcp;local;e.local;192.168.100.90;8009;` +
		`"id=deadbeef" "fn=` + long + `" "md=x"`

	devices := Parse(line, ProtocolChromecast)
	if len(devices) != 1 {
		t.Fatalf("got %d devices", len(devices))
	}
	if len(devices[0].Name) > maxNameLength {
		t.Errorf("name is %d characters; one receiver can crowd out the rest", len(devices[0].Name))
	}
}

// An advertised port outside the valid range would reach net.Dial as garbage.
func TestAnOutOfRangePortIsIgnored(t *testing.T) {
	for _, port := range []string{"0", "-1", "99999999", "4294967296"} {
		line := `=;wlo1;IPv4;evil;_googlecast._tcp;local;e.local;192.168.100.90;` + port +
			`;"id=deadbeef" "fn=x" "md=x"`
		if devices := Parse(line, ProtocolChromecast); len(devices) != 0 {
			t.Errorf("port %q was accepted as %d", port, devices[0].Port)
		}
	}
}
