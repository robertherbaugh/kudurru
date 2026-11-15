package dnstap

import (
	"errors"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

// The decoder of the dnstap listener (D372): the two messages of
// dnstap.proto read from the protocol buffers wire format by hand, with the
// standard wire parser only. Unknown fields are skipped, a field with the
// wrong wire type or a truncated field is an error.

// Message types the listener reads besides the ones the sink sends.
const (
	ResolverQuery    MessageType = 3
	ResolverResponse MessageType = 4
)

// ErrMalformedMessage is returned for a payload that is not a dnstap.Dnstap
// of type MESSAGE.
var ErrMalformedMessage = errors.New("dnstap: malformed message")

// DecodeFrame decodes one dnstap.Dnstap. The byte slices of the result
// alias payload.
func DecodeFrame(payload []byte, f *Frame) error {
	*f = Frame{}
	var message []byte
	haveMessage, typ := false, uint64(0)
	err := walk(payload, func(num protowire.Number, wt protowire.Type, v []byte, n uint64) error {
		switch num {
		case fieldIdentity, fieldVersion, fieldExtra, fieldMessage:
			if wt != protowire.BytesType {
				return ErrMalformedMessage
			}
			switch num {
			case fieldIdentity:
				f.Identity = v
			case fieldVersion:
				f.Version = v
			case fieldExtra:
				f.Extra = v
			default:
				message, haveMessage = v, true
			}
		case fieldType:
			if wt != protowire.VarintType {
				return ErrMalformedMessage
			}
			typ = n
		}
		return nil
	})
	if err != nil {
		return err
	}
	if typ != dnstapTypeMessage || !haveMessage {
		return ErrMalformedMessage
	}
	return decodeMessage(message, &f.Message)
}

func decodeMessage(b []byte, m *Message) error {
	var qSec, qNsec, rSec, rNsec uint64
	haveType := false
	err := walk(b, func(num protowire.Number, wt protowire.Type, v []byte, n uint64) error {
		want := protowire.VarintType
		switch num {
		case fieldQueryAddress, fieldResponseAddress, fieldQueryMessage, fieldQueryZone, fieldResponseMessage:
			want = protowire.BytesType
		case fieldQueryTimeNsec, fieldResponseTimeNsec:
			want = protowire.Fixed32Type
		case fieldMsgType, fieldSocketFamily, fieldSocketProtocol, fieldQueryPort, fieldResponsePort, fieldQueryTimeSec, fieldResponseTimeSec:
		default:
			return nil // a field this listener does not read
		}
		if wt != want {
			return ErrMalformedMessage
		}
		switch num {
		case fieldMsgType:
			m.Type, haveType = MessageType(n), true //nolint:gosec // G115: an enum; unknown values are refused later
		case fieldSocketFamily:
			m.SocketFamily = SocketFamily(n) //nolint:gosec // G115: an enum
		case fieldSocketProtocol:
			m.SocketProtocol = SocketProtocol(n) //nolint:gosec // G115: an enum
		case fieldQueryAddress:
			m.QueryAddress = v
		case fieldResponseAddress:
			m.ResponseAddress = v
		case fieldQueryPort, fieldResponsePort:
			if n > 0xFFFF {
				return ErrMalformedMessage
			}
			if num == fieldQueryPort {
				m.QueryPort = uint16(n)
			} else {
				m.ResponsePort = uint16(n)
			}
		case fieldQueryTimeSec:
			qSec = n
		case fieldQueryTimeNsec:
			qNsec = n
		case fieldQueryMessage:
			m.QueryMessage = v
		case fieldQueryZone:
			m.QueryZone = v
		case fieldResponseTimeSec:
			rSec = n
		case fieldResponseTimeNsec:
			rNsec = n
		case fieldResponseMessage:
			m.ResponseMessage = v
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !haveType {
		return ErrMalformedMessage
	}
	m.QueryTime, m.ResponseTime = decodeTime(qSec, qNsec), decodeTime(rSec, rNsec)
	return nil
}

// maxUnixSeconds keeps a decoded time within what time.Time and the query
// log represent (year 9999).
const maxUnixSeconds = 253402300799

// decodeTime returns the zero time for an absent or impossible time.
func decodeTime(sec, nsec uint64) time.Time {
	if sec == 0 || sec > maxUnixSeconds || nsec >= 1e9 {
		return time.Time{}
	}
	return time.Unix(int64(sec), int64(nsec)) //nolint:gosec // G115: bounded above
}

// walk calls fn for every field of b. v is the value of a length-delimited
// field, n the value of a varint or fixed field.
func walk(b []byte, fn func(num protowire.Number, wt protowire.Type, v []byte, n uint64) error) error {
	for len(b) > 0 {
		num, wt, tagLen := protowire.ConsumeTag(b)
		if tagLen < 0 {
			return ErrMalformedMessage
		}
		b = b[tagLen:]
		var (
			v      []byte
			n      uint64
			length int
		)
		switch wt {
		case protowire.VarintType:
			n, length = protowire.ConsumeVarint(b)
		case protowire.Fixed32Type:
			var x uint32
			x, length = protowire.ConsumeFixed32(b)
			n = uint64(x)
		case protowire.Fixed64Type:
			n, length = protowire.ConsumeFixed64(b)
		case protowire.BytesType:
			v, length = protowire.ConsumeBytes(b)
		default:
			length = protowire.ConsumeFieldValue(num, wt, b)
		}
		if length < 0 {
			return ErrMalformedMessage
		}
		b = b[length:]
		if err := fn(num, wt, v, n); err != nil {
			return err
		}
	}
	return nil
}
