package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/mrCode/castr/internal/picker"
	"github.com/mrCode/castr/internal/ui"
)

// menu is what the cast keybind runs: pick a receiver, pick a mode, cast.
func (a *App) menu() int {
	if a.Picker.Kind() == picker.KindNone {
		return a.fail(picker.MissingMessage())
	}

	// Both are fetched before the menu opens, so a live cast can be stopped
	// from the same list it was started from.
	devices, err := a.Client.Devices()
	if err != nil {
		return a.fail(err.Error())
	}
	sessions, err := a.Client.Sessions()
	if err != nil {
		return a.fail(err.Error())
	}

	choice, err := a.Picker.Pick("Cast to", ui.Entries(devices, sessions))
	if err != nil {
		return a.fail(err.Error())
	}
	if choice == "" {
		return 0 // cancelled, which is not a failure
	}

	if name := ui.StoppingSession(choice); name != "" {
		for _, s := range sessions {
			if s.Name == name {
				return a.run(func() error { return a.Client.Stop(s.DeviceID) })
			}
		}
		// The cast ended between opening the menu and choosing from it. Saying
		// so beats an error about a device id the user never saw.
		fmt.Fprintln(a.Out, "That cast has already stopped.")
		return 0
	}

	deviceID := ui.ParseSelection(choice)
	if choice == ui.ManualEntry {
		deviceID, err = a.addManually()
		if err != nil {
			return a.fail(err.Error())
		}
		if deviceID == "" {
			return 0 // cancelled at the address prompt
		}
	}
	if deviceID == "" {
		return a.fail(fmt.Sprintf("could not tell which receiver %q means", choice))
	}

	mode, err := a.pickMode()
	if err != nil {
		return a.fail(err.Error())
	}
	if mode == "" {
		return 0 // cancelled at the mode prompt
	}

	return a.run(func() error { return a.Client.Start(deviceID, mode) })
}

// pickMode asks mirror or extend, returning "" if the user cancelled.
func (a *App) pickMode() (string, error) {
	choice, err := a.Picker.Pick("Mode", ui.ModeEntries)
	if err != nil {
		return "", err
	}
	if choice == "" {
		return "", nil
	}
	mode := ui.ParseMode(choice)
	if mode == "" {
		return "", fmt.Errorf("could not tell which mode %q means", choice)
	}
	return mode, nil
}

// addManually prompts for an address and registers it, returning the new id.
func (a *App) addManually() (string, error) {
	address, err := a.Picker.Ask("Receiver address")
	if err != nil {
		return "", err
	}
	address = strings.TrimSpace(address)
	if address == "" {
		return "", nil
	}
	if strings.ContainsAny(address, " \t") {
		return "", errors.New("an address cannot contain spaces")
	}

	device := manualDevice(address, "")
	if err := a.Client.Add(device); err != nil {
		return "", err
	}
	return device.ID, nil
}
