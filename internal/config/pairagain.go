package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// PairAgainMark asks that the next cast to one receiver pair with it again,
// with a code, rather than verify its saved pairing.
//
// It is castr's form of castr-airplay's own advice for a receiver that was
// reset or replaced: "run again with -pair and enter the code it shows". The
// next cast passes -pair, and -device-id with the receiver this computer
// paired with. That keeps the saved pairing and its device ID, so a device
// naming itself as some other receiver is still refused; and the code is the
// kind that pairing was made with, so the real receiver has to show it.
//
// Forgetting the pairing instead -- what the helper's other advice, removing
// the entry, amounts to -- turns the next cast into a first contact, which a
// device asking for no code passes without any: the screen would go to
// whatever answered before anyone could see that no code had appeared.
type PairAgainMark struct {
	DeviceID string `json:"device_id"`
	// Address is where the pairing is bound, for a receiver castr knows only
	// by address.
	Address string `json:"address,omitempty"`
}

// PairAgainPath is where marks are kept: castr's own file, which no helper
// writes, so setting one needs no cast stopped.
func PairAgainPath(stateDir string) string {
	return filepath.Join(stateDir, "pair-again.json")
}

// ErrNoCodePairing means there is no pairing made with a code to pair again.
var ErrNoCodePairing = errors.New("no pairing made with a code")

// MarkPairAgain finds the receiver target names -- by its device ID, or by the
// address its pairing is bound to -- among the modes' saved pairings, and marks
// it to pair again on its next cast.
//
// Only a pairing made with a code qualifies: pairing again with -pair is safe
// because the receiver must show the code, and one paired without a code has
// none to show. An address that more than one paired receiver is bound to is
// refused rather than guessed at.
func MarkPairAgain(stateDir string, modes []string, target string) (PairAgainMark, error) {
	found := map[string]PairAgainMark{} // by upper-case device ID
	withoutCode := false
	for _, mode := range modes {
		creds, err := readCreds(CredsPath(stateDir, mode))
		if err != nil {
			return PairAgainMark{}, err
		}
		for id, entry := range creds {
			address, _ := entry["receiver_address"].(string)
			if !strings.EqualFold(id, target) && (address == "" || !strings.EqualFold(address, target)) {
				continue
			}
			if !hasAll(entry, identityFields) {
				continue
			}
			code, _ := entry["pairing_code"].(string)
			if code != "pin" && code != "password" {
				withoutCode = true
				continue
			}
			found[strings.ToUpper(id)] = PairAgainMark{DeviceID: id, Address: address}
		}
	}
	switch {
	case len(found) == 0 && withoutCode:
		return PairAgainMark{}, fmt.Errorf("%w: %s was paired without a code, so it has none to show",
			ErrNoCodePairing, target)
	case len(found) == 0:
		return PairAgainMark{}, fmt.Errorf("%w with %s", ErrNoCodePairing, target)
	case len(found) > 1:
		var ids []string
		for _, m := range found {
			ids = append(ids, m.DeviceID)
		}
		sort.Strings(ids)
		return PairAgainMark{}, fmt.Errorf("more than one receiver is paired at %s (%s); "+
			"name one by its id", target, strings.Join(ids, ", "))
	}
	var mark PairAgainMark
	for _, m := range found {
		mark = m
	}

	marks := readMarks(stateDir)
	kept := marks[:0]
	for _, m := range marks {
		if !strings.EqualFold(m.DeviceID, mark.DeviceID) {
			kept = append(kept, m)
		}
	}
	return mark, writeMarks(stateDir, append(kept, mark))
}

// HasCodePairing reports whether one mode's file holds a pairing made with a
// code for deviceID. castr-airplay reads only the file of the mode it casts in,
// and with -pair it pairs again with that pairing's kind of code; without one
// there, -pair pairs as at a first contact, as the device at the address
// advertises -- after a password and no code, for a device that asks for one.
// A cast that would pair again checks this first, since a mark is set from
// any mode's file and the pull that copies pairings across can be skipped.
func HasCodePairing(stateDir, mode, deviceID string) bool {
	creds, err := readCreds(CredsPath(stateDir, mode))
	if err != nil {
		return false
	}
	for id, entry := range creds {
		if !strings.EqualFold(id, deviceID) || !hasAll(entry, identityFields) {
			continue
		}
		code, _ := entry["pairing_code"].(string)
		return code == "pin" || code == "password"
	}
	return false
}

// PairAgainFor reports the mark for a receiver about to be cast to. One with a
// device ID is matched by it alone, never by address: another receiver can
// hold an address a marked one had. One without is matched by address.
func PairAgainFor(stateDir, deviceID, address string) (PairAgainMark, bool) {
	for _, m := range readMarks(stateDir) {
		if deviceID != "" {
			if strings.EqualFold(m.DeviceID, deviceID) {
				return m, true
			}
			continue
		}
		if address != "" && strings.EqualFold(m.Address, address) {
			return m, true
		}
	}
	return PairAgainMark{}, false
}

// ClearPairAgain removes a receiver's mark, once a cast has paired with it.
func ClearPairAgain(stateDir, deviceID string) error {
	marks := readMarks(stateDir)
	kept := marks[:0]
	for _, m := range marks {
		if !strings.EqualFold(m.DeviceID, deviceID) {
			kept = append(kept, m)
		}
	}
	if len(kept) == len(marks) {
		return nil
	}
	if len(kept) == 0 {
		err := os.Remove(PairAgainPath(stateDir))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return writeMarks(stateDir, kept)
}

// readMarks reads the marks; a missing or unreadable file holds none, which
// costs nothing worse than verifying a saved pairing as usual.
func readMarks(stateDir string) []PairAgainMark {
	raw, err := os.ReadFile(PairAgainPath(stateDir))
	if err != nil {
		return nil
	}
	var marks []PairAgainMark
	if json.Unmarshal(raw, &marks) != nil {
		return nil
	}
	return marks
}

func writeMarks(stateDir string, marks []PairAgainMark) error {
	raw, err := json.MarshalIndent(marks, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(PairAgainPath(stateDir), append(raw, '\n'))
}
