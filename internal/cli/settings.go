package cli

// `castr set` and `castr settings`.
// castrd re-reads config.toml before each cast, so a change applies to the
// next cast; one already running keeps what it started with.

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/mrCode/castr/internal/config"
)

const maxFPS = 120

func (a *App) configPath() string {
	if a.ConfigPath != "" {
		return a.ConfigPath
	}
	return config.Path()
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// settings prints the settings the bar offers.
func (a *App) settings(args []string) int {
	cfg, err := config.Load(a.configPath())
	if err != nil {
		return a.fail(err.Error())
	}
	s := config.CurrentSettings(cfg)
	if jsonRequested(args) {
		return a.emitJSON(s)
	}
	fmt.Fprintf(a.Out, "frame rate   %d fps\n", s.FPS)
	fmt.Fprintf(a.Out, "gpu capture  %s (AirPlay)\n", onOff(s.GPUCapture))
	return 0
}

// set changes one setting.
//
//	castr set fps <1-120>
//	castr set gpu-capture on|off
func (a *App) set(args []string) int {
	if len(args) != 2 {
		return a.fail("usage: castr set fps <1-120> | castr set gpu-capture on|off")
	}
	path := a.configPath()
	var what string
	switch strings.ToLower(args[0]) {
	case "fps", "framerate", "frame-rate":
		fps, err := strconv.Atoi(strings.TrimSuffix(strings.ToLower(args[1]), "fps"))
		if err != nil || fps < 1 || fps > maxFPS {
			return a.fail(fmt.Sprintf("fps must be a whole number from 1 to %d, got %q", maxFPS, args[1]))
		}
		if err := config.SetValue(path, "capture", "fps", strconv.Itoa(fps)); err != nil {
			return a.fail(err.Error())
		}
		what = fmt.Sprintf("Frame rate: %d fps", fps)
	case "gpu-capture", "gpu":
		var on bool
		switch strings.ToLower(args[1]) {
		case "on", "true", "yes", "1":
			on = true
		case "off", "false", "no", "0":
		default:
			return a.fail(fmt.Sprintf("gpu-capture is on or off, got %q", args[1]))
		}
		// castr's own setting is the inverse: it hides the GPU element.
		if err := config.SetValue(path, "airplay", "hide_vapostproc", strconv.FormatBool(!on)); err != nil {
			return a.fail(err.Error())
		}
		what = "GPU capture (AirPlay): " + onOff(on)
	default:
		return a.fail(fmt.Sprintf("unknown setting %q; expected fps or gpu-capture", args[0]))
	}
	fmt.Fprintf(a.Out, "%s. Applies from the next cast.\n", what)
	return 0
}
