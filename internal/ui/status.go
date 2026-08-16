package ui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mrCode/castr/internal/daemon"
	"github.com/mrCode/castr/internal/session"
)

// The bar icons.
const (
	IconIdle   = "󰄡"
	IconActive = "󰄠"
)

// Hints live in the tooltip because that is where people actually look. Right
// -click works but is invisible: a user reported that the bar "doesn't support
// stopping" when it did.
const (
	HintIdle   = "Click to cast"
	HintActive = "Left-click: menu   Right-click: stop"
)

// Status is the bar indicator's payload. The field names are waybar's, and the
// Quickshell widget reads the same JSON.
type Status struct {
	Text    string `json:"text"`
	Tooltip string `json:"tooltip"`
	Class   string `json:"class"`
}

// Render builds the indicator.
//
// It stays VISIBLE in every state and colour-codes the class instead, matching
// the other toggle indicators in this bar. Hiding it when idle makes it
// impossible to find, which is the point of an indicator you click.
func Render(sessions []daemon.SessionJSON) Status {
	if len(sessions) == 0 {
		return Status{Text: IconIdle, Tooltip: "Not casting\n" + HintIdle, Class: "idle"}
	}

	for _, s := range sessions {
		if s.State == string(session.Failed) {
			reason := s.Error
			if reason == "" {
				reason = "unknown error"
			}
			return Status{Text: IconActive,
				Tooltip: fmt.Sprintf("Cast failed: %s\n%s", reason, HintActive),
				Class:   "failed"}
		}
	}

	for _, s := range sessions {
		if s.State != string(session.Connecting) && s.State != string(session.AwaitingPin) {
			continue
		}
		tooltip := fmt.Sprintf("Connecting to %s...", s.Name)
		if s.State == string(session.AwaitingPin) {
			tooltip = fmt.Sprintf("%s: enter the PIN shown on the receiver", s.Name)
		}
		return Status{Text: IconActive, Tooltip: tooltip + "\n" + HintActive, Class: "connecting"}
	}

	names := make([]string, 0, len(sessions))
	seen := map[string]bool{}
	modes := make([]string, 0, 2)
	for _, s := range sessions {
		names = append(names, s.Name)
		// A stray emit can omit the mode; skip it rather than render a blank
		// pair of parentheses.
		if s.Mode != "" && !seen[s.Mode] {
			seen[s.Mode] = true
			modes = append(modes, s.Mode)
		}
	}
	sort.Strings(modes)

	text := IconActive
	if len(sessions) > 1 {
		text = fmt.Sprintf("%s %d", IconActive, len(sessions))
	}
	label := ""
	if len(modes) > 0 {
		label = " (" + strings.Join(modes, "/") + ")"
	}
	return Status{Text: text,
		Tooltip: fmt.Sprintf("Casting to %s%s\n%s", strings.Join(names, ", "), label, HintActive),
		Class:   "streaming"}
}
