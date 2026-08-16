// Package discovery finds receivers by asking avahi.
//
// castr does not run its own mDNS stack. Measured on a real network, a freshly
// started browser took 15.7s to produce its first result and found one receiver
// in ninety seconds, while avahi -- running since boot with a warm cache --
// listed six instantly. A daemon that exits when idle can never win that race,
// so it asks the resolver the rest of the desktop already uses.
package discovery

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
)

// Device is a receiver we can cast to.
type Device struct {
	ID       string
	Name     string
	Address  string
	Port     int
	Protocol string
	Model    string
}

const (
	ProtocolAirPlay = "airplay"

	airplayService = "_airplay._tcp"
)

// Runner executes a command and returns its stdout. Injected so no test ever
// shells out: the real implementation lives in main.
type Runner func(name string, args ...string) (string, error)

var escapePattern = regexp.MustCompile(`\\(\d{3})`)

// Unescape converts avahi's decimal byte escapes back to text.
//
// avahi writes a space as \032 and any non-ASCII byte as its decimal value, so
// a name arrives as `Khalid\226\128\153s\032MacBook`. Passing that through
// unchanged puts it in the user's menu verbatim.
func Unescape(name string) string {
	var out []byte
	for i := 0; i < len(name); {
		if m := escapePattern.FindStringSubmatch(name[i:]); m != nil &&
			strings.HasPrefix(name[i:], m[0]) {
			n, err := strconv.Atoi(m[1])
			if err == nil && n >= 0 && n <= 255 {
				out = append(out, byte(n))
				i += len(m[0])
				continue
			}
		}
		out = append(out, name[i])
		i++
	}
	return string(out)
}

// Parse turns `avahi-browse -rtp` output into devices.
//
// Resolved lines start with '=' and are semicolon separated:
//
//	=;iface;proto;name;type;domain;host;address;port;txt
//
// The `proto` field is the mDNS TRANSPORT, not the address family: real output
// reads IPv6 on lines carrying 10.10.10.231. Filtering on it finds nothing at
// all, so the ADDRESS is what gets validated -- a link-local v6 address is not
// a usable cast target, but the transport tells us nothing about that.
func Parse(output, protocol string) []Device {
	seen := map[string]Device{}
	var order []string

	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, "=") {
			continue
		}
		f := strings.Split(line, ";")
		if len(f) < 10 {
			continue
		}

		address := f[7]
		if !isIPv4(address) {
			continue
		}
		port, err := strconv.Atoi(f[8])
		if err != nil {
			continue
		}

		txt := f[9]
		unique := txtValue(txt, "deviceid")
		if unique == "" {
			unique = address
		}

		d := Device{
			ID:       protocol + ":" + unique,
			Name:     Unescape(f[3]),
			Address:  address,
			Port:     port,
			Protocol: protocol,
			Model:    txtValue(txt, "model"),
		}
		if _, dup := seen[d.ID]; !dup {
			order = append(order, d.ID)
		}
		seen[d.ID] = d
	}

	devices := make([]Device, 0, len(order))
	for _, id := range order {
		devices = append(devices, seen[id])
	}
	return devices
}

func isIPv4(address string) bool {
	ip := net.ParseIP(address)
	return ip != nil && ip.To4() != nil
}

var txtPattern = regexp.MustCompile(`"([^"]*)"`)

// txtValue pulls one key out of avahi's quoted TXT list: "a=1" "b=2".
func txtValue(txt, key string) string {
	for _, m := range txtPattern.FindAllStringSubmatch(txt, -1) {
		entry := m[1]
		if strings.HasPrefix(entry, key+"=") {
			return Unescape(strings.TrimPrefix(entry, key+"="))
		}
	}
	return ""
}

// Browse asks avahi for receivers of the given protocol.
func Browse(run Runner, protocol string) ([]Device, error) {
	out, err := run("avahi-browse", "-rtp", airplayService)
	if err != nil {
		return nil, fmt.Errorf("avahi-browse: %w", err)
	}
	return Parse(out, protocol), nil
}
