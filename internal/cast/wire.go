// Package cast speaks the Chromecast v2 protocol: length-prefixed protobuf
// messages over TLS to port 8009.
//
// The protocol carries exactly one protobuf message type, CastMessage, whose
// payload is almost always a JSON string. Pulling in the protobuf runtime to
// encode eight fields would be the largest dependency in the project, for a
// schema that has not changed since 2013 and is reproduced in full below. So
// this file encodes those eight fields directly.
package cast

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// The schema, from Chromium's cast_channel.proto:
//
//	message CastMessage {
//	  enum ProtocolVersion { CASTV2_1_0 = 0; }
//	  required ProtocolVersion protocol_version = 1;
//	  required string source_id       = 2;
//	  required string destination_id  = 3;
//	  required string namespace       = 4;
//	  enum PayloadType { STRING = 0; BINARY = 1; }
//	  required PayloadType payload_type = 5;
//	  optional string payload_utf8     = 6;
//	  optional bytes  payload_binary   = 7;
//	}
const (
	fieldProtocolVersion = 1
	fieldSourceID        = 2
	fieldDestinationID   = 3
	fieldNamespace       = 4
	fieldPayloadType     = 5
	fieldPayloadUTF8     = 6
	fieldPayloadBinary   = 7

	wireVarint = 0
	wireBytes  = 2

	payloadTypeString = 0
	payloadTypeBinary = 1
)

// maxFrame caps what a single message may claim to be.
//
// The length prefix arrives from the network before any of it is
// authenticated, so a hostile or broken device can ask this process to
// allocate 4 GiB by sending four bytes. Receivers send status JSON measured in
// kilobytes; a megabyte is already generous.
const maxFrame = 1 << 20

// Message is one CastMessage. Only the string payload is used in practice --
// binary payloads belong to the device-authentication namespace, which castr
// does not speak.
type Message struct {
	Source      string
	Destination string
	Namespace   string
	Payload     string
	Binary      []byte
}

// Encode renders a message as the wire format, length prefix included.
func Encode(m Message) []byte {
	var body []byte
	body = appendVarintField(body, fieldProtocolVersion, 0) // CASTV2_1_0
	body = appendBytesField(body, fieldSourceID, []byte(m.Source))
	body = appendBytesField(body, fieldDestinationID, []byte(m.Destination))
	body = appendBytesField(body, fieldNamespace, []byte(m.Namespace))
	if m.Binary != nil {
		body = appendVarintField(body, fieldPayloadType, payloadTypeBinary)
		body = appendBytesField(body, fieldPayloadBinary, m.Binary)
	} else {
		body = appendVarintField(body, fieldPayloadType, payloadTypeString)
		body = appendBytesField(body, fieldPayloadUTF8, []byte(m.Payload))
	}

	out := make([]byte, 4, 4+len(body))
	binary.BigEndian.PutUint32(out, uint32(len(body)))
	return append(out, body...)
}

// ErrFrameTooLarge is returned for a length prefix beyond maxFrame.
var ErrFrameTooLarge = errors.New("cast: frame too large")

// ReadMessage reads one length-prefixed message.
func ReadMessage(r io.Reader) (Message, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return Message{}, err
	}
	size := binary.BigEndian.Uint32(prefix[:])
	if size > maxFrame {
		return Message{}, fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, size)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(r, body); err != nil {
		return Message{}, err
	}
	return Decode(body)
}

// Decode parses a message body, the length prefix already removed.
//
// Unknown fields are skipped rather than rejected: the receivers in the field
// span a decade of firmware, and a message that carries a field this code has
// never heard of is still a message worth reading.
func Decode(body []byte) (Message, error) {
	var m Message
	for len(body) > 0 {
		key, n := binary.Uvarint(body)
		if n <= 0 {
			return Message{}, errors.New("cast: malformed field key")
		}
		body = body[n:]
		field, wire := key>>3, key&7

		switch wire {
		case wireVarint:
			_, n := binary.Uvarint(body)
			if n <= 0 {
				return Message{}, errors.New("cast: malformed varint")
			}
			body = body[n:]
		case wireBytes:
			size, n := binary.Uvarint(body)
			if n <= 0 {
				return Message{}, errors.New("cast: malformed length")
			}
			body = body[n:]
			if size > uint64(len(body)) {
				return Message{}, errors.New("cast: field runs past end of message")
			}
			value := body[:size]
			body = body[size:]
			switch field {
			case fieldSourceID:
				m.Source = string(value)
			case fieldDestinationID:
				m.Destination = string(value)
			case fieldNamespace:
				m.Namespace = string(value)
			case fieldPayloadUTF8:
				m.Payload = string(value)
			case fieldPayloadBinary:
				m.Binary = append([]byte(nil), value...)
			}
		default:
			return Message{}, fmt.Errorf("cast: unsupported wire type %d", wire)
		}
	}
	return m, nil
}

func appendVarintField(b []byte, field, value uint64) []byte {
	b = binary.AppendUvarint(b, field<<3|wireVarint)
	return binary.AppendUvarint(b, value)
}

func appendBytesField(b []byte, field uint64, value []byte) []byte {
	b = binary.AppendUvarint(b, field<<3|wireBytes)
	b = binary.AppendUvarint(b, uint64(len(value)))
	return append(b, value...)
}
