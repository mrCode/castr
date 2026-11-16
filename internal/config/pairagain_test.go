package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

// twoReceivers holds the office TV, paired with a PIN and bound to its
// address, and a receiver paired without a code.
const twoReceivers = `{"FE:5C:81:22:50:38":{"pairing_id":"id-1","ed25519_public":"pub-1",` +
	`"ed25519_seed":"seed-1","pairing_code":"pin","receiver_address":"10.10.10.231",` +
	`"restore_token":"token-office"},` +
	`"42:56:35:AF:76:0F":{"pairing_id":"id-2","ed25519_public":"pub-2","ed25519_seed":"seed-2",` +
	`"receiver_address":"10.10.10.40"}}`

var bothModes = []string{"mirror", "extend"}

func TestPairAgainMarksTheReceiverByIdOrAddress(t *testing.T) {
	for _, target := range []string{"FE:5C:81:22:50:38", "fe:5c:81:22:50:38", "10.10.10.231"} {
		dir := t.TempDir()
		putCreds(t, CredsPath(dir, "mirror"), twoReceivers)

		mark, err := MarkPairAgain(dir, bothModes, target)
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		if mark.DeviceID != "FE:5C:81:22:50:38" || mark.Address != "10.10.10.231" {
			t.Errorf("%s: mark = %+v", target, mark)
		}
		if got, ok := PairAgainFor(dir, "FE:5C:81:22:50:38", ""); !ok || got != mark {
			t.Errorf("%s: the mark is not found by device ID", target)
		}
		if got, ok := PairAgainFor(dir, "", "10.10.10.231"); !ok || got != mark {
			t.Errorf("%s: the mark is not found by address for a receiver known only by it", target)
		}
	}
}

func TestPairAgainLeavesThePairingAlone(t *testing.T) {
	// Forgetting it would make the next cast a first contact, which a device
	// asking for no code passes without one.
	dir := t.TempDir()
	putCreds(t, CredsPath(dir, "mirror"), twoReceivers)

	if _, err := MarkPairAgain(dir, bothModes, "FE:5C:81:22:50:38"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(CredsPath(dir, "mirror"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != twoReceivers {
		t.Errorf("the pairing file was changed:\n%s", raw)
	}
}

func TestAReceiverPairedWithoutACodeCannotBePairedAgain(t *testing.T) {
	// Pairing again is safe only because the real receiver must show a code.
	dir := t.TempDir()
	putCreds(t, CredsPath(dir, "mirror"), twoReceivers)

	_, err := MarkPairAgain(dir, bothModes, "42:56:35:AF:76:0F")
	if !errors.Is(err, ErrNoCodePairing) {
		t.Errorf("err = %v, want ErrNoCodePairing", err)
	}
	if _, ok := PairAgainFor(dir, "42:56:35:AF:76:0F", ""); ok {
		t.Error("it was marked anyway")
	}
}

func TestAnUnknownReceiverIsNotMarked(t *testing.T) {
	dir := t.TempDir()
	putCreds(t, CredsPath(dir, "mirror"), twoReceivers)

	if _, err := MarkPairAgain(dir, bothModes, "11:22:33:44:55:66"); !errors.Is(err, ErrNoCodePairing) {
		t.Errorf("err = %v, want ErrNoCodePairing", err)
	}
}

func TestAnAddressTwoPairingsShareIsNotGuessedAt(t *testing.T) {
	dir := t.TempDir()
	putCreds(t, CredsPath(dir, "mirror"), `{`+
		`"AA:AA":{"pairing_id":"a","ed25519_public":"a","ed25519_seed":"a","pairing_code":"pin","receiver_address":"10.0.0.5"},`+
		`"BB:BB":{"pairing_id":"b","ed25519_public":"b","ed25519_seed":"b","pairing_code":"pin","receiver_address":"10.0.0.5"}}`)

	if _, err := MarkPairAgain(dir, bothModes, "10.0.0.5"); err == nil {
		t.Error("an ambiguous address was marked")
	}
	if _, err := MarkPairAgain(dir, bothModes, "BB:BB"); err != nil {
		t.Errorf("naming one by its id was refused: %v", err)
	}
}

func TestAMarkByIdIsNeverMatchedByAddress(t *testing.T) {
	// A discovered receiver is known by its device ID. Another one now at the
	// address a marked receiver had must not be cast to with -pair and the
	// marked receiver's device ID.
	dir := t.TempDir()
	putCreds(t, CredsPath(dir, "mirror"), twoReceivers)
	if _, err := MarkPairAgain(dir, bothModes, "FE:5C:81:22:50:38"); err != nil {
		t.Fatal(err)
	}
	if _, ok := PairAgainFor(dir, "99:99:99:99:99:99", "10.10.10.231"); ok {
		t.Error("another receiver at the marked one's address matched the mark")
	}
}

func TestClearingAMarkLeavesTheOthers(t *testing.T) {
	dir := t.TempDir()
	putCreds(t, CredsPath(dir, "mirror"), `{`+
		`"AA:AA":{"pairing_id":"a","ed25519_public":"a","ed25519_seed":"a","pairing_code":"pin"},`+
		`"BB:BB":{"pairing_id":"b","ed25519_public":"b","ed25519_seed":"b","pairing_code":"password"}}`)
	for _, id := range []string{"AA:AA", "BB:BB"} {
		if _, err := MarkPairAgain(dir, bothModes, id); err != nil {
			t.Fatal(err)
		}
	}

	if err := ClearPairAgain(dir, "aa:aa"); err != nil {
		t.Fatal(err)
	}
	if _, ok := PairAgainFor(dir, "AA:AA", ""); ok {
		t.Error("the cleared mark is still there")
	}
	if _, ok := PairAgainFor(dir, "BB:BB", ""); !ok {
		t.Error("another receiver's mark went with it")
	}
	if err := ClearPairAgain(dir, "BB:BB"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(PairAgainPath(dir)); !os.IsNotExist(err) {
		t.Error("the file outlived its last mark")
	}
}

func TestACodePairingIsLookedForInTheCastsOwnMode(t *testing.T) {
	dir := t.TempDir()
	putCreds(t, CredsPath(dir, "mirror"), twoReceivers)

	if !HasCodePairing(dir, "mirror", "fe:5c:81:22:50:38") {
		t.Error("mirror's PIN pairing was not found")
	}
	if HasCodePairing(dir, "extend", "FE:5C:81:22:50:38") {
		t.Error("a pairing only mirror holds was found for extend")
	}
	if HasCodePairing(dir, "mirror", "42:56:35:AF:76:0F") {
		t.Error("a pairing made without a code counted")
	}
}

func TestPairAgainMarksSetAtOnceAreAllKept(t *testing.T) {
	// `castr pair-again` and castrd both rewrite the mark file. Two updates at
	// the same moment must not drop one: a lost mark makes the next cast refuse
	// the receiver again, and a lost clear asks for its code twice.
	dir := t.TempDir()
	const n = 24
	var body strings.Builder
	body.WriteString("{")
	for i := 0; i < n; i++ {
		if i > 0 {
			body.WriteString(",")
		}
		fmt.Fprintf(&body, `"AA:00:00:00:00:%02X":{"pairing_id":"id-%d","ed25519_public":"pub",`+
			`"ed25519_seed":"seed","pairing_code":"pin"}`, i, i)
	}
	body.WriteString("}")
	putCreds(t, CredsPath(dir, "mirror"), body.String())
	// Half of them marked before, to be cleared while the rest are marked.
	for i := 0; i < n; i += 2 {
		if _, err := MarkPairAgain(dir, bothModes, fmt.Sprintf("AA:00:00:00:00:%02X", i)); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("AA:00:00:00:00:%02X", i)
			var err error
			if i%2 == 0 {
				err = ClearPairAgain(dir, id)
			} else {
				_, err = MarkPairAgain(dir, bothModes, id)
			}
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		id := fmt.Sprintf("AA:00:00:00:00:%02X", i)
		_, marked := PairAgainFor(dir, id, "")
		if want := i%2 == 1; marked != want {
			t.Errorf("%s: marked = %v, want %v", id, marked, want)
		}
	}
}

func TestPairAgainExplainsAnAddressNoPairingIsBoundTo(t *testing.T) {
	// A pairing made before addresses were recorded is bound to none until a
	// cast to it succeeds. Asked for by address, it cannot be found, and the
	// error has to say why rather than claim there is no pairing at all.
	dir := t.TempDir()
	putCreds(t, CredsPath(dir, "mirror"), `{"FE:5C:81:22:50:38":{"pairing_id":"id-1",`+
		`"ed25519_public":"pub-1","ed25519_seed":"seed-1","pairing_code":"pin"}}`)

	_, err := MarkPairAgain(dir, bothModes, "10.10.10.231")
	if !errors.Is(err, ErrNoCodePairing) {
		t.Fatalf("err = %v, want ErrNoCodePairing", err)
	}
	if !strings.Contains(err.Error(), "no address recorded") {
		t.Errorf("err = %v, want it to explain the unrecorded address", err)
	}

	// Not for a device ID, nor when every pairing has its address.
	if _, err := MarkPairAgain(dir, bothModes, "AA:BB:CC:DD:EE:FF"); err == nil ||
		strings.Contains(err.Error(), "no address recorded") {
		t.Errorf("by device id: err = %v", err)
	}
	putCreds(t, CredsPath(dir, "mirror"), twoReceivers)
	if _, err := MarkPairAgain(dir, bothModes, "10.10.10.99"); err == nil ||
		strings.Contains(err.Error(), "no address recorded") {
		t.Errorf("every pairing bound: err = %v", err)
	}
}
