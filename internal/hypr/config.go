package hypr

import (
	"fmt"
	"strings"
)

// Everything in this file exists because Hyprland 0.56.2 -- shipped by the
// Omarchy "Quarto" update -- changed how monitors are configured and how
// virtual outputs are removed, and BOTH old commands now fail while exiting 0.
//
// Measured on 0.56.2:
//
//	hyprctl keyword monitor <cfg>   "keyword can't work with non-legacy
//	                                 parsers. Use eval."          exit 0
//	hyprctl output remove <mirrored> "output not found"           exit 0
//
// The exit status is the trap. castr ran both, saw success, created a mirror
// output that mirrored nothing, and would have shown an Apple TV an empty
// desktop with no error anywhere. The same two calls are in omarchy-cast, so
// mirror mode broke there too on the day Hyprland was updated.

// okResponse is the ONLY thing a mutating hyprctl command says when it worked.
//
// Checking it is what turns those silent no-ops into errors. Do not replace
// this with an exit-status check: hyprctl exits 0 while refusing.
const okResponse = "ok"

// mutate runs a command that changes something and insists it really did.
func mutate(run Runner, args ...string) error {
	out, err := run("hyprctl", args...)
	if err != nil {
		return err
	}
	if response := strings.TrimSpace(out); !strings.EqualFold(response, okResponse) {
		return fmt.Errorf("hyprctl %s: %s", strings.Join(args, " "), response)
	}
	return nil
}

// MonitorSpec is one monitor's configuration.
type MonitorSpec struct {
	Output   string
	Mode     string  // "1920x1080@60", or "preferred"
	Position string  // "auto", or "1920x200"
	Scale    float64 // 0 means 1
	Mirror   string  // "" leaves mirroring alone; MirrorNone clears it
	Disabled bool
}

// MirrorNone clears an output's mirroring.
//
// It has to be set explicitly BEFORE removing a mirrored output. On 0.56.2 a
// mirrored headless output cannot be removed at all -- `output remove` answers
// "output not found" for an output `monitors` is listing -- and it stays on the
// desk forever, stealing windows, until the compositor restarts. Un-mirroring
// first makes the removal work.
const MirrorNone = "none"

// Apply configures a monitor through Hyprland's Lua parser.
//
// `hyprctl eval` rather than `hyprctl keyword`: the config is Lua now, and
// keyword is refused outright by the new parser.
func Apply(run Runner, spec MonitorSpec) error {
	return mutate(run, "eval", spec.lua())
}

// lua renders the spec as an hl.monitor call.
func (s MonitorSpec) lua() string {
	scale := s.Scale
	if scale <= 0 {
		scale = 1
	}
	mode := s.Mode
	if mode == "" {
		mode = "preferred"
	}
	position := s.Position
	if position == "" {
		position = "auto"
	}

	fields := []string{
		fmt.Sprintf("output = %s", luaString(s.Output)),
		fmt.Sprintf("mode = %s", luaString(mode)),
		fmt.Sprintf("position = %s", luaString(position)),
		fmt.Sprintf("scale = %g", scale),
	}
	if s.Mirror != "" {
		fields = append(fields, fmt.Sprintf("mirror = %s", luaString(s.Mirror)))
	}
	if s.Disabled {
		fields = append(fields, "disabled = true")
	}
	return "hl.monitor({ " + strings.Join(fields, ", ") + " })"
}

// luaString quotes a value for Lua.
//
// Monitor names come from the compositor and from castr's own constants, so
// this is belt and braces -- but an unescaped quote would turn a monitor name
// into executable Lua, and this string is handed to an interpreter.
func luaString(s string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`)
	return `"` + replacer.Replace(s) + `"`
}
