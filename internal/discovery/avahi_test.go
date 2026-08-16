package discovery

import "testing"

// Real captured lines. The format held a trap that a hand-written fixture would
// have hidden: the proto field says IPv6 while the address is IPv4.
const (
	appleTV = `=;wlan0;IPv6;Meeting\032Room;AirPlay Remote Video;local;` +
		`Meeting-Room.local;10.10.10.231;7000;` +
		`"acl=0" "deviceid=FE:5C:81:22:50:38" "model=AppleTV11,1" "srcvers=960.13.1"`

	macBook = `=;wlan0;IPv6;Ibrahim\226\128\153s\032MacBook\032Air;AirPlay Remote Video;` +
		`local;Ibrahims-MacBook-Air.local;10.10.10.224;7000;` +
		`"deviceid=CE:65:83:54:F4:01" "model=MacBookAir10,1"`
)

func TestIPv6TransportWithIPv4AddressIsKept(t *testing.T) {
	// The trap: that field is the mDNS transport, not the address family.
	// Rejecting "IPv6" lines finds nothing at all.
	got := Parse(appleTV, ProtocolAirPlay)

	if len(got) != 1 {
		t.Fatalf("want 1 device, got %d", len(got))
	}
	if got[0].Address != "10.10.10.231" {
		t.Errorf("address = %q, want 10.10.10.231", got[0].Address)
	}
}

func TestFieldsAreExtracted(t *testing.T) {
	d := Parse(appleTV, ProtocolAirPlay)[0]

	for _, c := range []struct{ name, got, want string }{
		{"id", d.ID, "airplay:FE:5C:81:22:50:38"},
		{"name", d.Name, "Meeting Room"},
		{"model", d.Model, "AppleTV11,1"},
		{"protocol", d.Protocol, ProtocolAirPlay},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	if d.Port != 7000 {
		t.Errorf("port = %d, want 7000", d.Port)
	}
}

func TestEscapedNamesAreDecoded(t *testing.T) {
	// Raw, this reads Ibrahim\226\128\153s\032MacBook\032Air in the menu.
	d := Parse(macBook, ProtocolAirPlay)[0]

	if want := "Ibrahim’s MacBook Air"; d.Name != want {
		t.Errorf("name = %q, want %q", d.Name, want)
	}
}

func TestSeveralDevices(t *testing.T) {
	got := Parse(appleTV+"\n"+macBook, ProtocolAirPlay)

	if len(got) != 2 {
		t.Fatalf("want 2 devices, got %d", len(got))
	}
}

func TestUnresolvedAndJunkLinesAreIgnored(t *testing.T) {
	noise := "+;wlan0;IPv6;Something;AirPlay Remote Video;local\nrubbish\n\n"

	if got := Parse(noise+appleTV, ProtocolAirPlay); len(got) != 1 {
		t.Fatalf("want 1 device, got %d", len(got))
	}
}

func TestIPv6OnlyReceiverIsSkipped(t *testing.T) {
	// A Xiaomi box advertised link-local v6 only. Not a usable cast target.
	line := `=;wlan0;IPv6;MiTV;_googlecast._tcp;local;mitv.local;` +
		`fe80::6153:933a:c814:99a4;8009;"id=abc"`

	if got := Parse(line, ProtocolAirPlay); len(got) != 0 {
		t.Errorf("want no devices, got %d", len(got))
	}
}

func TestDeviceWithoutAnIDFallsBackToItsAddress(t *testing.T) {
	line := `=;wlan0;IPv4;Plain;AirPlay Remote Video;local;plain.local;` +
		`10.0.0.7;7000;"model=Thing"`

	if got := Parse(line, ProtocolAirPlay)[0].ID; got != "airplay:10.0.0.7" {
		t.Errorf("id = %q, want airplay:10.0.0.7", got)
	}
}

func TestBrowseNeverShellsOutInTests(t *testing.T) {
	// The runner is injected precisely so this is impossible. The Python suite
	// escaped its sandbox four times; Go enforces it by construction.
	var gotArgs []string
	run := func(name string, args ...string) (string, error) {
		gotArgs = append([]string{name}, args...)
		return appleTV, nil
	}

	devices, err := Browse(run, ProtocolAirPlay)
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("want 1 device, got %d", len(devices))
	}
	want := []string{"avahi-browse", "-rtp", "_airplay._tcp"}
	for i := range want {
		if gotArgs[i] != want[i] {
			t.Fatalf("argv = %v, want %v", gotArgs, want)
		}
	}
}

func TestUnescape(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`Meeting\032Room`, "Meeting Room"},
		{`plain`, "plain"},
		{`Ibrahim\226\128\153s`, "Ibrahim’s"},
		{``, ""},
	} {
		if got := Unescape(c.in); got != c.want {
			t.Errorf("Unescape(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
