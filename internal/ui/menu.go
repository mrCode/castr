// Package ui builds the text the user sees: menu entries, the ids parsed back
// out of them, and the bar indicator's payload.
package ui

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/mrCode/castr/internal/discovery"

	"github.com/mrCode/castr/internal/daemon"
	"github.com/mrCode/castr/internal/session"
)

// Labels turn a protocol into something worth reading in a menu.
var Labels = map[string]string{"airplay": "AirPlay"}

// ManualEntry exists because mDNS is unusable on networks that do not forward
// multicast -- an Apple TV served AirPlay on port 7000 and answered no mDNS
// query at all, on a network where a MacBook answered both.
const ManualEntry = "Enter an address manually..."

// StopEntry is in the menu because right-clicking the bar indicator is
// undiscoverable and did not work for at least one user, who reported that
// stopping was impossible when it was merely invisible.
const StopEntry = "Stop casting"

// Mode entries. Extend names the output because picking the wrong one at the
// portal prompt silently produces a mirror, and the portal then REMEMBERS that
// choice and repeats it on every later cast.
const (
	MirrorEntry = "Mirror — show this screen on the receiver"
	ExtendEntry = "Extend — second display (pick 'castr' if the portal asks)"
)

// ModeEntries is the second prompt, after a receiver is chosen.
var ModeEntries = []string{MirrorEntry, ExtendEntry}

// ModeEntriesFor is the mode prompt for one receiver.
//
// A Chromecast is offered mirror only. Extend needs a virtual output whose
// contents castr captures and hands to the receiver, and the Chromecast path
// has not been built for that -- so offering it would present a choice that
// can only end in a failure notification. A menu that lists what cannot work
// is worse than a shorter menu.
func ModeEntriesFor(protocol string) []string {
	if protocol == discovery.ProtocolChromecast {
		return []string{MirrorEntry}
	}
	return ModeEntries
}

// ParseMode reads a mode back out of a chosen line, "" if it is neither.
func ParseMode(line string) string {
	line = strings.TrimSpace(line)
	switch {
	case strings.HasPrefix(line, "Mirror"):
		return session.ModeMirror
	case strings.HasPrefix(line, "Extend"):
		return session.ModeExtend
	}
	return ""
}

// idPattern anchors on the trailing brackets because ids embed colons --
// AirPlay uses a MAC address -- so splitting on ":" finds the wrong thing.
var idPattern = regexp.MustCompile(`\[((?:airplay|cast):.+)\]$`)

// ParseSelection reads the device id back out of a chosen line.
func ParseSelection(line string) string {
	if m := idPattern.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
		return m[1]
	}
	return ""
}

// Entries builds the menu: any stoppable cast first, then the receivers, then
// the manual escape hatch.
func Entries(devices []daemon.DeviceJSON, sessions []daemon.SessionJSON) []string {
	ordered := append([]daemon.DeviceJSON(nil), devices...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return strings.ToLower(ordered[i].Name) < strings.ToLower(ordered[j].Name)
	})

	entries := make([]string, 0, len(ordered)+len(sessions)+1)
	// First, so stopping is always one click away while casting.
	for _, s := range sessions {
		entries = append(entries, fmt.Sprintf("%s (%s)", StopEntry, s.Name))
	}
	for _, d := range ordered {
		label, ok := Labels[d.Protocol]
		if !ok {
			label = d.Protocol
		}
		model := ""
		if d.Model != "" {
			model = " · " + d.Model
		}
		entries = append(entries, fmt.Sprintf("%s (%s%s) [%s]", d.Name, label, model, d.ID))
	}
	entries = append(entries, ManualEntry)
	return entries
}

// StoppingSession returns the name in a "Stop casting (Name)" line, "" if the
// line is not one.
func StoppingSession(line string) string {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, StopEntry+" (") || !strings.HasSuffix(line, ")") {
		return ""
	}
	return line[len(StopEntry)+2 : len(line)-1]
}
