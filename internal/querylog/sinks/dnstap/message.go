package dnstap

import (
	"encoding/binary"
	"time"
)

// The dnstap message (dnstap.proto, CC0; https://dnstap.info), encoded by
// hand in the protocol buffers wire format: the sink writes these two
// messages and nothing else, so a generated encoder and its runtime are not
// needed. Field numbers are those of dnstap.proto.

// MessageType is dnstap.Message.Type.
type MessageType uint32

// Message types kudurru sends.
const (
	AuthQuery         MessageType = 1
	AuthResponse      MessageType = 2
	ClientQuery       MessageType = 5
	ClientResponse    MessageType = 6
	ForwarderQuery    MessageType = 7
	ForwarderResponse MessageType = 8
)

// SocketFamily is dnstap.SocketFamily.
type SocketFamily uint32

// Socket families.
const (
	FamilyINET  SocketFamily = 1
	FamilyINET6 SocketFamily = 2
)

// SocketProtocol is dnstap.SocketProtocol.
type SocketProtocol uint32

// Socket protocols.
const (
	ProtocolUDP SocketProtocol = 1
	ProtocolTCP SocketProtocol = 2
	ProtocolDOT SocketProtocol = 3
	ProtocolDOH SocketProtocol = 4
	ProtocolDOQ SocketProtocol = 7
)

// dnstapTypeMessage is dnstap.Dnstap.Type MESSAGE.
const dnstapTypeMessage = 1

// Field numbers of dnstap.Dnstap.
const (
	fieldIdentity = 1
	fieldVersion  = 2
	fieldExtra    = 3
	fieldMessage  = 14
	fieldType     = 15
)

// Field numbers of dnstap.Message.
const (
	fieldMsgType          = 1
	fieldSocketFamily     = 2
	fieldSocketProtocol   = 3
	fieldQueryAddress     = 4
	fieldResponseAddress  = 5
	fieldQueryPort        = 6
	fieldResponsePort     = 7
	fieldQueryTimeSec     = 8
	fieldQueryTimeNsec    = 9
	fieldQueryMessage     = 10
	fieldQueryZone        = 11
	fieldResponseTimeSec  = 12
	fieldResponseTimeNsec = 13
	fieldResponseMessage  = 14
)

// Wire types.
const (
	wireVarint  = 0
	wireBytes   = 2
	wireFixed32 = 5
)

// Message is dnstap.Message. Zero values are left out, except Type.
type Message struct {
	Type           MessageType
	SocketFamily   SocketFamily
	SocketProtocol SocketProtocol
	// QueryAddress is the client's address, 4 or 16 bytes.
	QueryAddress    []byte
	ResponseAddress []byte
	// QueryPort and ResponsePort are sent when not zero.
	QueryPort    uint16
	ResponsePort uint16
	QueryTime    time.Time
	QueryMessage []byte
	// QueryZone is the zone apex in DNS wire format (AUTH_*).
	QueryZone       []byte
	ResponseTime    time.Time
	ResponseMessage []byte
}

// Frame is one dnstap.Dnstap of type MESSAGE.
type Frame struct {
	Identity, Version, Extra []byte
	Message                  Message
}

// AppendFrame appends the encoded frame to b. scratch is reused for the
// nested message and returned grown; pass it back on the next call.
func AppendFrame(b, scratch []byte, f *Frame) (out, scratchOut []byte) {
	scratch = AppendMessage(scratch[:0], &f.Message)
	b = appendBytes(b, fieldIdentity, f.Identity)
	b = appendBytes(b, fieldVersion, f.Version)
	b = appendBytes(b, fieldExtra, f.Extra)
	b = appendTag(b, fieldMessage, wireBytes)
	b = binary.AppendUvarint(b, uint64(len(scratch)))
	b = append(b, scratch...)
	b = appendTag(b, fieldType, wireVarint)
	b = binary.AppendUvarint(b, dnstapTypeMessage)
	return b, scratch
}

// AppendMessage appends the encoded dnstap.Message to b.
func AppendMessage(b []byte, m *Message) []byte {
	b = appendTag(b, fieldMsgType, wireVarint)
	b = binary.AppendUvarint(b, uint64(m.Type))
	b = appendVarint(b, fieldSocketFamily, uint64(m.SocketFamily))
	b = appendVarint(b, fieldSocketProtocol, uint64(m.SocketProtocol))
	b = appendBytes(b, fieldQueryAddress, m.QueryAddress)
	b = appendBytes(b, fieldResponseAddress, m.ResponseAddress)
	b = appendVarint(b, fieldQueryPort, uint64(m.QueryPort))
	b = appendVarint(b, fieldResponsePort, uint64(m.ResponsePort))
	b = appendTime(b, fieldQueryTimeSec, fieldQueryTimeNsec, m.QueryTime)
	b = appendBytes(b, fieldQueryMessage, m.QueryMessage)
	b = appendBytes(b, fieldQueryZone, m.QueryZone)
	b = appendTime(b, fieldResponseTimeSec, fieldResponseTimeNsec, m.ResponseTime)
	b = appendBytes(b, fieldResponseMessage, m.ResponseMessage)
	return b
}

func appendTag(b []byte, field, wire int) []byte {
	return binary.AppendUvarint(b, uint64(field)<<3|uint64(wire)) //nolint:gosec // G115: constant field numbers
}

// appendVarint appends a varint field; zero means absent.
func appendVarint(b []byte, field int, v uint64) []byte {
	if v == 0 {
		return b
	}
	b = appendTag(b, field, wireVarint)
	return binary.AppendUvarint(b, v)
}

// appendBytes appends a length-delimited field; empty means absent.
func appendBytes(b []byte, field int, v []byte) []byte {
	if len(v) == 0 {
		return b
	}
	b = appendTag(b, field, wireBytes)
	b = binary.AppendUvarint(b, uint64(len(v)))
	return append(b, v...)
}

// appendTime appends seconds (uint64) and nanoseconds (fixed32); the zero
// time is absent. A time before 1970 is not representable and is left out.
func appendTime(b []byte, secField, nsecField int, t time.Time) []byte {
	if t.IsZero() || t.Unix() < 0 {
		return b
	}
	b = appendTag(b, secField, wireVarint)
	b = binary.AppendUvarint(b, uint64(t.Unix())) //nolint:gosec // G115: checked non-negative above
	b = appendTag(b, nsecField, wireFixed32)
	return binary.LittleEndian.AppendUint32(b, uint32(t.Nanosecond())) //nolint:gosec // G115: below one billion
}
