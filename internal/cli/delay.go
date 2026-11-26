package cli

// `castr delay`, the audio/video sync
// offset per receiver (see config/av_offset.go).

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/mrCode/castr/internal/config"
	"github.com/mrCode/castr/internal/daemon"
)

func (a *App) stateDir() string {
	if a.StateDir != "" {
		return a.StateDir
	}
	return daemon.StateDir()
}

// parseDelay reads signed whole milliseconds: 120, +120, -80, 0.
func parseDelay(arg string) (int, bool) {
	ms, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(arg), "ms"))
	return ms, err == nil
}

// describeDelay is the human reading of an offset.
func describeDelay(ms int) string {
	switch {
	case ms > 0:
		return fmt.Sprintf("%+d ms (audio %d ms later)", ms, ms)
	case ms < 0:
		return fmt.Sprintf("%+d ms (video %d ms later, so audio comes sooner)", ms, -ms)
	}
	return "0 ms (no correction)"
}

// delay shows or sets the audio/video offset.
//
//	castr delay              every cast's offset
//	castr delay <id>         that receiver's offset, as a bare number
//	castr delay <id> <ms>    set it; positive delays audio, negative video
//	castr delay <ms>         set it for the one receiver being cast to
func (a *App) delay(args []string) int {
	dir := a.stateDir()

	switch len(args) {
	case 0:
		sessions, err := a.Client.Sessions()
		if err != nil {
			return a.fail(err.Error())
		}
		if len(sessions) == 0 {
			fmt.Fprintln(a.Out, "Nothing is casting. Set a receiver's delay with: castr delay <id> <ms>")
			return 0
		}
		for _, s := range sessions {
			ms, err := config.ReadAVOffset(dir, s.DeviceID)
			if err != nil {
				return a.fail(err.Error())
			}
			fmt.Fprintf(a.Out, "%s  %s\n", s.Name, describeDelay(ms))
		}
		return 0

	case 1:
		if ms, ok := parseDelay(args[0]); ok {
			sessions, err := a.Client.Sessions()
			if err != nil {
				return a.fail(err.Error())
			}
			if len(sessions) != 1 {
				return a.fail(fmt.Sprintf("%d casts are running; name one: castr delay <id> %d", len(sessions), ms))
			}
			return a.setDelay(dir, sessions[0].DeviceID, sessions[0].Name, ms)
		}
		ms, err := config.ReadAVOffset(dir, args[0])
		if err != nil {
			return a.fail(err.Error())
		}
		fmt.Fprintln(a.Out, ms)
		return 0

	case 2:
		ms, ok := parseDelay(args[1])
		if !ok {
			return a.fail(fmt.Sprintf("not a number of milliseconds: %q", args[1]))
		}
		return a.setDelay(dir, args[0], args[0], ms)
	}
	return a.fail("usage: castr delay [id] [ms]")
}

func (a *App) setDelay(dir, deviceID, name string, ms int) int {
	clamped := config.ClampAVOffset(ms)
	if err := config.WriteAVOffset(dir, deviceID, clamped); err != nil {
		return a.fail(err.Error())
	}
	if clamped != ms {
		fmt.Fprintf(a.Out, "%s  %s (limited to ±%d ms)\n", name, describeDelay(clamped), config.AVOffsetLimitMS)
		return 0
	}
	fmt.Fprintf(a.Out, "%s  %s\n", name, describeDelay(clamped))
	return 0
}
