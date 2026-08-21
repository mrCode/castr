package cast

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	want := Message{
		Source:      "sender-0",
		Destination: "receiver-0",
		Namespace:   "urn:x-cast:com.google.cast.receiver",
		Payload:     `{"type":"LAUNCH","appId":"CC1AD845","requestId":1}`,
	}
	got, err := ReadMessage(bytes.NewReader(Encode(want)))
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip changed the message:\n got %+v\nwant %+v", got, want)
	}
}

// The length prefix is read before anything about the sender is known, so a
// four-byte lie must not become a four-gigabyte allocation.
func TestReadMessageRefusesAnEnormousFrame(t *testing.T) {
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], 1<<30)

	_, err := ReadMessage(bytes.NewReader(prefix[:]))
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("got %v, want ErrFrameTooLarge", err)
	}
}

func TestReadMessageRejectsATruncatedBody(t *testing.T) {
	full := Encode(Message{Source: "sender-0", Namespace: "ns", Payload: "hello"})

	_, err := ReadMessage(bytes.NewReader(full[:len(full)-3]))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("got %v, want ErrUnexpectedEOF", err)
	}
}

// Receivers in the field span a decade of firmware. A message carrying a field
// this code has never heard of is still readable, and dropping the whole
// message because of one is worse than ignoring it.
func TestDecodeSkipsUnknownFields(t *testing.T) {
	body := Encode(Message{Source: "sender-0", Payload: "kept"})[4:]
	body = appendVarintField(body, 99, 12345)
	body = appendBytesField(body, 98, []byte("something new"))

	got, err := Decode(body)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Payload != "kept" || got.Source != "sender-0" {
		t.Errorf("known fields lost: %+v", got)
	}
}

func TestDecodeRejectsAFieldRunningPastTheEnd(t *testing.T) {
	body := appendBytesField(nil, fieldPayloadUTF8, []byte("twelve chars"))
	body = body[:len(body)-4] // the length still claims twelve

	_, err := Decode(body)
	if err == nil || !strings.Contains(err.Error(), "past end") {
		t.Fatalf("got %v, want an error about running past the end", err)
	}
}

func TestEncodeMarksBinaryPayloadsAsBinary(t *testing.T) {
	got, err := Decode(Encode(Message{Namespace: "auth", Binary: []byte{0x00, 0xff}})[4:])
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !bytes.Equal(got.Binary, []byte{0x00, 0xff}) {
		t.Errorf("binary payload lost: %+v", got)
	}
	if got.Payload != "" {
		t.Errorf("binary payload leaked into the string field: %q", got.Payload)
	}
}
