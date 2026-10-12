// Package querylog defines the query event, the non-blocking collector the
// log observer writes to, the privacy transform applied once at ingest, the
// live tail, the querylog metrics and the Sink contract every sink
// implements (docs/architecture.md §11.2). The SQLite writer, rollups and
// the read API live in internal/querylog/sqlitelog.
package querylog

import (
	"context"
	"encoding/json"
	"net/netip"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// Event is one answered (or dropped) query. Privacy transforms are applied
// once, by the collector, before any sink or live-tail subscriber sees the
// event; after that the event is shared and must be treated as immutable.
type Event struct {
	// ID is the node-local monotonic id assigned by the collector (the
	// SQLite rowid and the keyset cursor). Zero until the collector stamps
	// it, and zero forever when no id source is configured.
	ID       uint64          `json:"id,omitempty"`
	Time     time.Time       `json:"ts"`
	Latency  time.Duration   `json:"latency"`
	Node     string          `json:"node"`
	Proto    pipeline.Proto  `json:"proto"`
	Client   netip.Addr      `json:"client"`
	Name     string          `json:"name"`
	QType    uint16          `json:"qtype"`
	QClass   uint16          `json:"qclass"`
	Status   pipeline.Status `json:"status"`
	Rcode    int             `json:"rcode"`
	Upstream string          `json:"upstream,omitempty"`
	List     string          `json:"list,omitempty"`
	Rule     string          `json:"rule,omitempty"`
	Answers  int             `json:"answers"`
	Flags    Flags           `json:"flags"`

	// ClientName is the client's identity from the identify step (client
	// id, DHCP/static name) or, under anonymize_client with pseudonyms,
	// the daily pseudonym. Empty when unknown.
	ClientName string `json:"client_name,omitempty"`
	// ClientGroup is the first client group the query was attributed to.
	ClientGroup string `json:"client_group,omitempty"`
	// Policy is the deciding policy, "namespace/name": the
	// highest-priority active policy of the client's groups, or the
	// default policy as the fallback (policy.Decision.Policy). Empty when
	// no policy applied.
	Policy string `json:"policy,omitempty"`
	// Zone is the authoritative or forward zone that matched, if any.
	Zone string `json:"zone,omitempty"`
	// CNAME is the CNAME-chain name that caused a block (blocked_cname).
	CNAME string `json:"cname,omitempty"`
	// ReplyType classifies the answer (ip, cname, nodata, nxdomain, ...).
	ReplyType ReplyType `json:"reply_type"`
	// DNSSEC is the validation state of the answer.
	DNSSEC DNSSECState `json:"dnssec"`
	// EDE is the extended DNS error code (RFC 8914) when HasEDE is set.
	EDE    uint16 `json:"ede,omitempty"`
	HasEDE bool   `json:"has_ede,omitempty"`
	// ECS is the client subnet carried by the query, if any.
	ECS netip.Prefix `json:"ecs,omitzero"`
	// TraceID is the W3C trace id when the query was traced; zero otherwise.
	TraceID [16]byte `json:"-"`
	// Answer is a short summary of the first answer records. Stored only
	// under privacy full (D59).
	Answer string `json:"answer,omitempty"`
}

// Traced reports whether the event carries a trace id.
func (e *Event) Traced() bool { return e.TraceID != [16]byte{} }

// Flags are the boolean facets stored in one column.
type Flags struct {
	RD, DO, TC, AD, CD bool
	Cached, Stale      bool
	// Prefetch marks a cache refresh issued by kudurru itself.
	Prefetch bool
	// ECS marks a query that carried an EDNS client subnet option.
	ECS bool
	// Traceparent marks a query that carried an EDNS TRACEPARENT option.
	Traceparent bool
}

// Flag bits as stored in the query.flags column (docs/architecture.md
// §11.2: RD, DO, TC, AD, CD, cached, stale, prefetch, ecs, traceparent).
const (
	FlagRD uint16 = 1 << iota
	FlagDO
	FlagTC
	FlagAD
	FlagCD
	FlagCached
	FlagStale
	FlagPrefetch
	FlagECS
	FlagTraceparent
)

// Bits packs the flags into the column encoding.
func (f Flags) Bits() uint16 {
	var b uint16
	for _, x := range []struct {
		on  bool
		bit uint16
	}{
		{f.RD, FlagRD}, {f.DO, FlagDO}, {f.TC, FlagTC}, {f.AD, FlagAD}, {f.CD, FlagCD},
		{f.Cached, FlagCached}, {f.Stale, FlagStale}, {f.Prefetch, FlagPrefetch},
		{f.ECS, FlagECS}, {f.Traceparent, FlagTraceparent},
	} {
		if x.on {
			b |= x.bit
		}
	}
	return b
}

// FlagsFromBits unpacks the column encoding.
func FlagsFromBits(b uint16) Flags {
	return Flags{
		RD: b&FlagRD != 0, DO: b&FlagDO != 0, TC: b&FlagTC != 0, AD: b&FlagAD != 0, CD: b&FlagCD != 0,
		Cached: b&FlagCached != 0, Stale: b&FlagStale != 0, Prefetch: b&FlagPrefetch != 0,
		ECS: b&FlagECS != 0, Traceparent: b&FlagTraceparent != 0,
	}
}

// ReplyType classifies the response the client received.
type ReplyType uint8

// Reply types.
const (
	ReplyUnknown  ReplyType = iota
	ReplyIP                 // A/AAAA answer for the question
	ReplyCNAME              // CNAME chain without an address
	ReplyDomain             // PTR or other name answer
	ReplyNoData             // NOERROR, empty answer
	ReplyNXDomain           // NXDOMAIN
	ReplyServfail           // SERVFAIL
	ReplyRefused            // REFUSED
	ReplyNotImp             // NOTIMP
	ReplyOther              // any other rcode
	ReplyBlob               // other record data (TXT, MX, HTTPS, ...)
	ReplyNone               // no response was sent
)

var replyNames = [...]string{"unknown", "ip", "cname", "domain", "nodata", "nxdomain", "servfail", "refused", "notimp", "other", "blob", "none"}

// String returns the API name of the reply type.
func (r ReplyType) String() string {
	if int(r) < len(replyNames) {
		return replyNames[r]
	}
	return "unknown"
}

// DNSSECState is the validation state recorded in the query.dnssec column.
type DNSSECState uint8

// DNSSEC states.
const (
	DNSSECUnknown DNSSECState = iota
	DNSSECSecure
	DNSSECInsecure
	DNSSECBogus
	DNSSECIndeterminate
)

var dnssecNames = [...]string{"unknown", "secure", "insecure", "bogus", "indeterminate"}

// String returns the API name of the state.
func (d DNSSECState) String() string {
	if int(d) < len(dnssecNames) {
		return dnssecNames[d]
	}
	return "unknown"
}

// Collector accepts events without blocking the query path.
type Collector interface {
	// Enqueue returns false when the event was dropped (queue full).
	Enqueue(ev *Event) bool
}

// Sink is any destination for batches of events. The host owns queue
// sizing, drop counters, retry and privacy; sinks only write.
type Sink interface {
	Name() string
	Open(ctx context.Context, cfg json.RawMessage, reg prometheus.Registerer) error
	// Write is called from a dedicated goroutine, never from the query path.
	// The events are shared with other consumers and must not be modified.
	Write(batch []*Event) error
	Flush(ctx context.Context) error
	Close() error
}
