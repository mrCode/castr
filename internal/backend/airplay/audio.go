package airplay

// a virtual output for the TV.
//
// castr-airplay records the monitor of whatever output is the default when
// the cast starts, so without this the sound plays on the laptop AND the TV,
// and nothing about the cast shows up in the desktop's audio panel. For as long
// as an AirPlay cast carries audio, castr now adds a virtual output named after
// the receiver and makes it the default:
//
//   - desktop sound goes to the TV only, not to the laptop at the same time;
//   - the output exists only while casting, so the audio panel lists it then;
//   - its slider and mute set what the TV receives (monitor.channel-volumes).
//
// The last cast to end puts the previous default back and removes the output.
// It is best effort: if pactl fails, the cast goes ahead as before.

import (
	"strings"
	"unicode"
)

// SinkName is the virtual output's node name.
const SinkName = "castr_cast"

// Pactl runs pactl with the given arguments and returns its output. A Backend
// whose Pactl is nil leaves the desktop's audio alone.
type Pactl func(args ...string) (string, error)

// audioRoute is what the running casts share: one virtual output, and the
// default it replaced.
type audioRoute struct {
	users    int
	module   string
	previous string
}

// routeAudio sends the desktop's sound to the virtual output for this session.
// Called right before castr-airplay starts, which reads the default output's
// monitor when it sets up audio.
func (b *Backend) routeAudio(cs *castSession) {
	if b.Pactl == nil || !b.Config.Audio {
		return
	}
	b.audioMu.Lock()
	defer b.audioMu.Unlock()

	if b.audio.users == 0 {
		// A daemon killed while casting could not clean up after itself.
		b.removeStaleSinks()

		previous, err := b.Pactl("get-default-sink")
		if err != nil {
			return
		}
		previous = strings.TrimSpace(previous)
		if previous == SinkName {
			previous = ""
		}
		module, err := b.Pactl("load-module", "module-null-sink",
			"sink_name="+SinkName,
			`sink_properties=device.description="`+sinkDescription(cs.device.Name)+`" monitor.channel-volumes=true`)
		if err != nil {
			return
		}
		module = strings.TrimSpace(module)
		if _, err := b.Pactl("set-default-sink", SinkName); err != nil {
			_, _ = b.Pactl("unload-module", module)
			return
		}
		b.audio = audioRoute{module: module, previous: previous}
	}
	b.audio.users++
	cs.audio = true
}

// releaseAudio gives the session's share of the virtual output back; the last
// one out restores the previous default and removes the output. Safe to call
// more than once for a session.
func (b *Backend) releaseAudio(cs *castSession) {
	if b.Pactl == nil {
		return
	}
	b.audioMu.Lock()
	defer b.audioMu.Unlock()

	if !cs.audio {
		return
	}
	cs.audio = false
	b.audio.users--
	if b.audio.users > 0 {
		return
	}
	// Default first, so the apps follow it back before the output goes away.
	if b.audio.previous != "" {
		_, _ = b.Pactl("set-default-sink", b.audio.previous)
	}
	_, _ = b.Pactl("unload-module", b.audio.module)
	b.audio = audioRoute{}
}

// removeStaleSinks unloads virtual outputs a previous daemon left behind.
func (b *Backend) removeStaleSinks() {
	out, err := b.Pactl("list", "short", "modules")
	if err != nil {
		return
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[1] != "module-null-sink" {
			continue
		}
		for _, arg := range fields[2:] {
			if arg == "sink_name="+SinkName {
				_, _ = b.Pactl("unload-module", fields[0])
				break
			}
		}
	}
}

// sinkDescription is the label the audio panel shows. The receiver's name comes
// from the network, so only plain characters get through: it ends up inside a
// quoted pactl property list.
func sinkDescription(receiver string) string {
	var sb strings.Builder
	for _, r := range receiver {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			sb.WriteRune(r)
		case strings.ContainsRune(" .-_()'", r):
			sb.WriteRune(r)
		default:
			sb.WriteRune(' ')
		}
	}
	name := strings.Join(strings.Fields(sb.String()), " ")
	if runes := []rune(name); len(runes) > 40 {
		name = strings.TrimSpace(string(runes[:40]))
	}
	if name == "" {
		name = "TV"
	}
	return name + " (cast)"
}
