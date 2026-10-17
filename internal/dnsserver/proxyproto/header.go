// Package proxyproto parses HAProxy PROXY protocol version 2 headers and
// decides whether one may replace the transport peer as the DNS client
// address (dev-docs/design-record.md §3.3, decisions D45 and D56).
//
// The parser is written from the HAProxy specification
// (https://www.haproxy.org/download/3.0/doc/proxy-protocol.txt, section
// 2.2). It never reads past the length the header declares, never accepts a
// header longer than MaxHeaderLen and does not allocate while parsing; TLV
// values alias the input buffer.
//
// Only version 2 (binary) headers are recognised. dnsdist, the edge kudurru
// is designed around, only sends version 2, and the text form of version 1
// cannot carry the shared-secret TLV that authentication relies on.
package proxyproto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
)

// signature is the 12-byte magic every version 2 header starts with.
var signature = [signatureLen]byte{0x0D, 0x0A, 0x0D, 0x0A, 0x00, 0x0D, 0x0A, 0x51, 0x55, 0x49, 0x54, 0x0A}

const (
	signatureLen = 12
	// fixedLen is the signature plus version/command, family/transport and
	// the 16-bit length of the remainder.
	fixedLen = 16

	// MaxHeaderLen bounds the total header length (fixed part, address
	// block and TLVs). The format allows 16+65535 bytes; dnsdist sends well
	// under 128, so anything near the bound is hostile or broken.
	MaxHeaderLen = 2048

	addrLenINET  = 12  // 2 x 4-byte address + 2 x 2-byte port
	addrLenINET6 = 36  // 2 x 16-byte address + 2 x 2-byte port
	addrLenUNIX  = 216 // 2 x 108-byte path
)

// Command is the low nibble of the version/command byte.
type Command uint8

// Commands defined by the specification.
const (
	// CommandLocal marks a connection the proxy opened on its own behalf
	// (health checks); the receiver uses the real endpoints.
	CommandLocal Command = 0x0
	// CommandProxy marks a relayed connection; the address block carries
	// the original endpoints.
	CommandProxy Command = 0x1
)

// Family is the high nibble of the family/transport byte.
type Family uint8

// Address families defined by the specification.
const (
	FamilyUnspec Family = 0x0
	FamilyINET   Family = 0x1
	FamilyINET6  Family = 0x2
	FamilyUNIX   Family = 0x3
)

// Transport is the low nibble of the family/transport byte.
type Transport uint8

// Transport protocols defined by the specification.
const (
	TransportUnspec   Transport = 0x0
	TransportStream   Transport = 0x1
	TransportDatagram Transport = 0x2
)

// TLV types. 0xE0-0xEF are reserved by the specification for private use;
// kudurru uses 0xE0 for the shared secret dnsdist adds with
// SetAdditionalProxyProtocolValueAction.
const (
	TLVTypeALPN      uint8 = 0x01
	TLVTypeAuthority uint8 = 0x02
	TLVTypeCRC32C    uint8 = 0x03
	TLVTypeNoop      uint8 = 0x04
	TLVTypeUniqueID  uint8 = 0x05
	TLVTypeSSL       uint8 = 0x20
	TLVTypeNetNS     uint8 = 0x30
	TLVTypeSecret    uint8 = 0xE0
)

// Parse errors. Every error Parse returns wraps exactly one of them.
var (
	// ErrNoHeader means the input does not start with the v2 signature; the
	// bytes are an ordinary DNS message or stream.
	ErrNoHeader = errors.New("proxyproto: no PROXY protocol v2 signature")
	// ErrTruncated means the input is shorter than the header it declares.
	ErrTruncated = errors.New("proxyproto: truncated header")
	// ErrMalformed means the header violates the specification or exceeds
	// MaxHeaderLen.
	ErrMalformed = errors.New("proxyproto: malformed header")
)

// Header is a parsed version 2 header.
type Header struct {
	Command   Command
	Family    Family
	Transport Transport
	// Source and Destination are set for FamilyINET and FamilyINET6 only.
	Source      netip.AddrPort
	Destination netip.AddrPort
	// RawTLVs is the validated TLV block: zero or more (type, 16-bit
	// big-endian length, value) records. After Parse it aliases the input.
	RawTLVs []byte
}

// HasSignature reports whether b starts with the 12-byte v2 signature.
func HasSignature(b []byte) bool {
	return len(b) >= signatureLen && [signatureLen]byte(b[:signatureLen]) == signature
}

// maybeSignature reports whether b is consistent with the start of a v2
// header: either a prefix of the signature or a buffer that starts with it.
// Stream readers use it to stop reading as soon as the bytes cannot be a
// header.
func maybeSignature(b []byte) bool {
	n := min(len(b), signatureLen)
	for i := range n {
		if b[i] != signature[i] {
			return false
		}
	}
	return true
}

// headerLen validates the fixed part in b (at least fixedLen bytes that
// start with the signature) and returns the total header length it
// declares.
func headerLen(b []byte) (int, error) {
	if ver := b[12] >> 4; ver != 2 {
		return 0, fmt.Errorf("%w: version %d", ErrMalformed, ver)
	}
	if cmd := Command(b[12] & 0x0F); cmd != CommandLocal && cmd != CommandProxy {
		return 0, fmt.Errorf("%w: command %#x", ErrMalformed, uint8(cmd))
	}
	if fam := Family(b[13] >> 4); fam > FamilyUNIX {
		return 0, fmt.Errorf("%w: address family %#x", ErrMalformed, uint8(fam))
	}
	if tr := Transport(b[13] & 0x0F); tr > TransportDatagram {
		return 0, fmt.Errorf("%w: transport %#x", ErrMalformed, uint8(tr))
	}
	total := fixedLen + int(binary.BigEndian.Uint16(b[14:16]))
	if total > MaxHeaderLen {
		return 0, fmt.Errorf("%w: length %d exceeds %d", ErrMalformed, total, MaxHeaderLen)
	}
	return total, nil
}

// Parse parses the v2 header at the start of b and returns it together with
// the number of bytes it occupies; the payload starts at b[n:].
func Parse(b []byte) (h Header, n int, err error) {
	if !maybeSignature(b) {
		return Header{}, 0, ErrNoHeader
	}
	if len(b) < fixedLen {
		return Header{}, 0, ErrTruncated
	}
	total, err := headerLen(b)
	if err != nil {
		return Header{}, 0, err
	}
	if len(b) < total {
		return Header{}, 0, fmt.Errorf("%w: have %d of %d bytes", ErrTruncated, len(b), total)
	}
	h.Command = Command(b[12] & 0x0F)
	h.Family = Family(b[13] >> 4)
	h.Transport = Transport(b[13] & 0x0F)
	body := b[fixedLen:total]

	var addrLen int
	switch h.Family {
	case FamilyINET:
		addrLen = addrLenINET
	case FamilyINET6:
		addrLen = addrLenINET6
	case FamilyUNIX:
		addrLen = addrLenUNIX
	}
	if len(body) < addrLen {
		return Header{}, 0, fmt.Errorf("%w: address block needs %d bytes, have %d", ErrMalformed, addrLen, len(body))
	}
	switch h.Family {
	case FamilyINET:
		h.Source = netip.AddrPortFrom(netip.AddrFrom4([4]byte(body[0:4])), binary.BigEndian.Uint16(body[8:10]))
		h.Destination = netip.AddrPortFrom(netip.AddrFrom4([4]byte(body[4:8])), binary.BigEndian.Uint16(body[10:12]))
	case FamilyINET6:
		h.Source = netip.AddrPortFrom(netip.AddrFrom16([16]byte(body[0:16])), binary.BigEndian.Uint16(body[32:34]))
		h.Destination = netip.AddrPortFrom(netip.AddrFrom16([16]byte(body[16:32])), binary.BigEndian.Uint16(body[34:36]))
	}

	tlvs := body[addrLen:]
	for rest := tlvs; len(rest) > 0; {
		if len(rest) < 3 {
			return Header{}, 0, fmt.Errorf("%w: %d trailing bytes after the last TLV", ErrMalformed, len(rest))
		}
		vl := int(binary.BigEndian.Uint16(rest[1:3]))
		if len(rest) < 3+vl {
			return Header{}, 0, fmt.Errorf("%w: TLV %#x declares %d bytes, have %d", ErrMalformed, rest[0], vl, len(rest)-3)
		}
		rest = rest[3+vl:]
	}
	if len(tlvs) > 0 {
		h.RawTLVs = tlvs
	}
	return h, total, nil
}

// TLV returns the value of the first TLV of type t. RawTLVs must be well
// formed, which Parse guarantees; a malformed block reads as "not found".
func (h *Header) TLV(t uint8) ([]byte, bool) {
	for rest := h.RawTLVs; len(rest) >= 3; {
		vl := int(binary.BigEndian.Uint16(rest[1:3]))
		if len(rest) < 3+vl {
			return nil, false
		}
		if rest[0] == t {
			return rest[3 : 3+vl], true
		}
		rest = rest[3+vl:]
	}
	return nil, false
}

// AppendTLV appends one TLV record to dst and returns the extended slice.
// Values longer than 65535 bytes cannot be encoded and are truncated to
// that length; Append then rejects the header as too long anyway.
func AppendTLV(dst []byte, t uint8, value []byte) []byte {
	if len(value) > 0xFFFF {
		value = value[:0xFFFF]
	}
	dst = append(dst, t)
	dst = binary.BigEndian.AppendUint16(dst, uint16(len(value))) //nolint:gosec // bounded to 0xFFFF above
	return append(dst, value...)
}

// Append encodes h as a version 2 header onto dst. It is what dnsdist does
// on the wire and is used by tests, the fuzz corpus and any kudurru
// component that needs to speak PROXYv2 to a listener.
func (h *Header) Append(dst []byte) ([]byte, error) {
	if h.Command != CommandLocal && h.Command != CommandProxy {
		return dst, fmt.Errorf("%w: command %#x", ErrMalformed, uint8(h.Command))
	}
	if h.Transport > TransportDatagram {
		return dst, fmt.Errorf("%w: transport %#x", ErrMalformed, uint8(h.Transport))
	}
	var addrLen int
	switch h.Family {
	case FamilyUnspec:
	case FamilyINET:
		if !h.Source.Addr().Is4() || !h.Destination.Addr().Is4() {
			return dst, fmt.Errorf("%w: INET header needs IPv4 source and destination", ErrMalformed)
		}
		addrLen = addrLenINET
	case FamilyINET6:
		if !h.Source.Addr().Is6() || !h.Destination.Addr().Is6() {
			return dst, fmt.Errorf("%w: INET6 header needs IPv6 source and destination", ErrMalformed)
		}
		addrLen = addrLenINET6
	case FamilyUNIX:
		addrLen = addrLenUNIX
	default:
		return dst, fmt.Errorf("%w: address family %#x", ErrMalformed, uint8(h.Family))
	}
	total := fixedLen + addrLen + len(h.RawTLVs)
	if total > MaxHeaderLen {
		return dst, fmt.Errorf("%w: length %d exceeds %d", ErrMalformed, total, MaxHeaderLen)
	}
	dst = append(dst, signature[:]...)
	dst = append(dst, 0x20|byte(h.Command), byte(h.Family)<<4|byte(h.Transport))
	dst = binary.BigEndian.AppendUint16(dst, uint16(total-fixedLen)) //nolint:gosec // bounded by MaxHeaderLen above
	switch h.Family {
	case FamilyINET:
		s, d := h.Source.Addr().As4(), h.Destination.Addr().As4()
		dst = append(dst, s[:]...)
		dst = append(dst, d[:]...)
		dst = binary.BigEndian.AppendUint16(dst, h.Source.Port())
		dst = binary.BigEndian.AppendUint16(dst, h.Destination.Port())
	case FamilyINET6:
		s, d := h.Source.Addr().As16(), h.Destination.Addr().As16()
		dst = append(dst, s[:]...)
		dst = append(dst, d[:]...)
		dst = binary.BigEndian.AppendUint16(dst, h.Source.Port())
		dst = binary.BigEndian.AppendUint16(dst, h.Destination.Port())
	case FamilyUNIX:
		dst = append(dst, make([]byte, addrLenUNIX)...)
	}
	return append(dst, h.RawTLVs...), nil
}
