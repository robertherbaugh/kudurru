package tracing

import (
	"encoding/hex"

	"github.com/miekg/dns"
)

// DefaultOptionCode is the EDNS option code of TRACEPARENT that dnsdist
// (traceparentOptionCode) and PowerDNS Recursor use by default. The draft
// (draft-edns-otel-trace-ids) has no assigned code yet; 65500 is in the
// local/experimental range of RFC 6891 §9.
const DefaultOptionCode uint16 = 65500

// Option code bounds: only the local/experimental range of RFC 6891 §9 is
// configurable, so a misconfigured code can never strip a standard option
// such as client subnet or cookies.
const (
	MinOptionCode uint16 = 65001
	MaxOptionCode uint16 = 65534
)

// FlagSampled is the W3C trace-flags bit that carries the sampling
// decision of the caller.
const FlagSampled byte = 0x01

// optionLen is the length of a version 0 TRACEPARENT option as kudurru
// writes it: VERSION (1), RESERVED (1), TRACE-ID (16), PARENT-ID (8) and
// TRACEFLAGS (1).
const optionLen = 27

// TraceID is a W3C trace id.
type TraceID [16]byte

// IsValid reports whether t is not all zero (W3C: an all-zero id is invalid).
func (t TraceID) IsValid() bool { return t != TraceID{} }

// String is the lowercase hex form used in query-log rows and exemplars.
func (t TraceID) String() string { return hex.EncodeToString(t[:]) }

// SpanID is a W3C span (parent) id.
type SpanID [8]byte

// IsValid reports whether s is not all zero.
func (s SpanID) IsValid() bool { return s != SpanID{} }

// String is the lowercase hex form.
func (s SpanID) String() string { return hex.EncodeToString(s[:]) }

// Remote is the caller's trace context carried by a TRACEPARENT option.
type Remote struct {
	TraceID TraceID
	// SpanID is the caller's span; zero when the option carried only a
	// trace id (an earlier draft revision).
	SpanID SpanID
	// Flags are the W3C trace flags; FlagSampled is the caller's decision.
	// HasFlags is false for an option without the flags octet, which then
	// says nothing about sampling.
	Flags    byte
	HasFlags bool
}

// ParseOption decodes the data of a TRACEPARENT option. The current draft
// defines version 0 as VERSION, RESERVED, TRACE-ID (16), PARENT-ID (8) and
// TRACEFLAGS (1), 27 octets. Earlier revisions of the draft sent the same
// fields without TRACEFLAGS (26 octets) or without PARENT-ID and TRACEFLAGS
// (18 octets); those are read too, so a peer on an older revision still
// joins the trace. Any other length, another version or an all-zero trace
// id is malformed, and the draft says to process the query as if the
// option were absent.
func ParseOption(b []byte) (Remote, bool) {
	var r Remote
	switch len(b) {
	case 18, 26, optionLen:
	default:
		return r, false
	}
	if b[0] != 0 {
		return r, false
	}
	copy(r.TraceID[:], b[2:18])
	if !r.TraceID.IsValid() {
		return Remote{}, false
	}
	if len(b) >= 26 {
		copy(r.SpanID[:], b[18:26])
	}
	if len(b) == optionLen {
		r.Flags, r.HasFlags = b[26], true
	}
	return r, true
}

// AppendOption appends the data of a version 0 TRACEPARENT option.
func AppendOption(dst []byte, trace TraceID, span SpanID, flags byte) []byte {
	dst = append(dst, 0, 0)
	dst = append(dst, trace[:]...)
	dst = append(dst, span[:]...)
	return append(dst, flags)
}

// Extracted is what TakeOption found in a query.
type Extracted struct {
	// Present is true when the query carried at least one option with the
	// configured code.
	Present bool
	// Valid is true when exactly one such option was present and it parsed.
	Valid  bool
	Remote Remote
}

// TakeOption removes every EDNS option with code from m and reports what
// it found. It changes the option slice in place and allocates nothing.
// Two options with the code are malformed: neither is used.
func TakeOption(m *dns.Msg, code uint16) Extracted {
	var ex Extracted
	if m == nil {
		return ex
	}
	opt := m.IsEdns0()
	if opt == nil {
		return ex
	}
	kept := opt.Option[:0]
	count := 0
	for _, o := range opt.Option {
		local, ok := o.(*dns.EDNS0_LOCAL)
		if !ok || local.Code != code {
			kept = append(kept, o)
			continue
		}
		count++
		if count == 1 {
			ex.Remote, ex.Valid = ParseOption(local.Data)
		}
	}
	if count == 0 {
		return ex
	}
	for i := len(kept); i < len(opt.Option); i++ {
		opt.Option[i] = nil
	}
	opt.Option = kept
	ex.Present = true
	if count > 1 {
		ex.Valid, ex.Remote = false, Remote{}
	}
	return ex
}
