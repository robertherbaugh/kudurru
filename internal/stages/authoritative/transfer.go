package authoritative

import (
	"errors"
	"slices"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

// Zone transfers out (dev-docs/design-record.md §7.1 step 6, §13.1 M2):
//
//   - AXFR (RFC 5936) only over stream transports (TCP, DoT); over UDP it
//     is REFUSED. The answer is the zone in canonical order with the SOA
//     first and last, chunked into messages of at most 64 KiB, streamed by
//     the transport through pipeline.Response.Transfer.
//   - IXFR (RFC 1995) without a journal (v0.2): a secondary whose serial
//     is current gets the single SOA; otherwise the whole zone in AXFR
//     form (§4), or, over UDP, the single SOA so it retries over TCP (§2).
//   - spec.policy.zoneTransfer, transferACL and transferTSIGKeyRefs are
//     enforced by zone.Zone.TransferAllowed against the client address
//     and the verified TSIG key (pipeline.Client.TSIGKey). A refusal is
//     REFUSED with Status refused_acl.
//   - While the zone cache is not open (unsynced or stale) nothing is
//     transferred: SERVFAIL servfail_unsynced.

// Transfer metric results (kudurru_zone_transfers_total{result}).
const (
	TransferSuccess     = "success"
	TransferUpToDate    = "uptodate"
	TransferTCPRequired = "tcp_required"
	TransferRefused     = "refused"
	TransferUnsynced    = "unsynced"
	TransferFormErr     = "formerr"
	TransferAborted     = "aborted"
)

// Errors of the Transfer capability.
var (
	// ErrZoneNotFound: no zone with exactly that name is loaded.
	ErrZoneNotFound = errors.New("authoritative: no such zone")
	// ErrNotTransferable: a forward zone or a partial zone with fallback.
	ErrNotTransferable = errors.New("authoritative: zone is not transferable")
	// ErrUnsynced: the zone cache cannot vouch for its data (D80).
	ErrUnsynced = errors.New("authoritative: zone cache is not synced")
)

// Metrics are the stage's Prometheus instruments.
type Metrics struct {
	// Transfers counts zone transfers by zone, direction ("out"; "in" is
	// for secondary zones in v0.2) and result.
	Transfers *prometheus.CounterVec
}

// NewMetrics registers the stage metrics with reg (nil leaves them
// unregistered, for tests).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Transfers: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_zone_transfers_total",
			Help: "Zone transfers (AXFR/IXFR) by zone, direction and result (success, uptodate, tcp_required, refused, unsynced, formerr, aborted).",
		}, []string{"zone", "direction", "result"}),
	}
	if reg != nil {
		reg.MustRegister(m.Transfers)
	}
	return m
}

func (s *Stage) countTransfer(z *zone.Zone, result string) {
	if s.Metrics != nil {
		s.Metrics.Transfers.WithLabelValues(z.Name(), "out", result).Inc()
	}
}

func (s *Stage) chunkBytes() int {
	if s.ChunkBytes > 0 {
		return s.ChunkBytes
	}
	return zone.DefaultChunkBytes
}

// streams reports whether the transport can carry a multi-message answer.
func streams(p pipeline.Proto) bool { return p == pipeline.ProtoTCP || p == pipeline.ProtoDoT }

// transfer answers AXFR and IXFR for zone z. The query ACL has already
// passed.
func (s *Stage) transfer(req *pipeline.Request, gate zone.Gate, z *zone.Zone) *pipeline.Response {
	q := req.Msg.Question[0]
	if gate != zone.GateOpen {
		s.countTransfer(z, TransferUnsynced)
		return s.unsynced(req, gate)
	}
	if !z.TransferAllowed(clientAddr(req), req.Client.TSIGKey) {
		s.countTransfer(z, TransferRefused)
		return s.refused(req, z)
	}
	stream := streams(req.Client.Proto)
	if q.Qtype == dns.TypeIXFR {
		serial, ok := ixfrSerial(req.Msg, z)
		if !ok {
			// RFC 1995 §3: the query carries the secondary's SOA in the
			// authority section.
			s.countTransfer(z, TransferFormErr)
			r := pipeline.Reply(req, dns.RcodeFormatError, pipeline.StatusAuthoritative)
			r.Upstream = upstreamName(z)
			r.Origin = pipeline.OriginFailure
			s.finish(req, r.Msg)
			return r
		}
		if z.UpToDate(serial) || !stream {
			result := TransferUpToDate
			if !z.UpToDate(serial) {
				result = TransferTCPRequired
			}
			s.countTransfer(z, result)
			return s.soaOnly(req, z)
		}
	} else if !stream {
		// AXFR is defined over stream transports only (RFC 5936 §4.2).
		s.countTransfer(z, TransferRefused)
		return s.refused(req, z)
	}
	return s.axfr(req, z)
}

// refused is the REFUSED answer for a transfer the policy or transport
// does not allow.
func (s *Stage) refused(req *pipeline.Request, z *zone.Zone) *pipeline.Response {
	r := pipeline.Reply(req, dns.RcodeRefused, pipeline.StatusRefusedACL)
	r.Upstream = upstreamName(z)
	r.Origin = pipeline.OriginFailure
	s.finish(req, r.Msg)
	return r
}

// soaOnly answers an IXFR with the current SOA alone: the secondary is up
// to date, or must retry over TCP (RFC 1995 §2, §4).
func (s *Stage) soaOnly(req *pipeline.Request, z *zone.Zone) *pipeline.Response {
	m := s.xfrMsg(req, []dns.RR{z.SOA()})
	return &pipeline.Response{Msg: m, Status: pipeline.StatusAuthoritative, Upstream: upstreamName(z), Origin: pipeline.OriginAuthoritative, Security: pipeline.Security{State: pipeline.ValidationUnknown}}
}

// axfr streams the whole zone. Msg is the first message; Transfer yields
// every message, the first one included, and counts the transfer once the
// last one was written (or the client went away).
func (s *Stage) axfr(req *pipeline.Request, z *zone.Zone) *pipeline.Response {
	chunks := z.AXFR(s.chunkBytes())
	var first *dns.Msg
	for c := range chunks {
		first = s.xfrMsg(req, c)
		break
	}
	resp := &pipeline.Response{Msg: first, Status: pipeline.StatusAuthoritative, Upstream: upstreamName(z), Origin: pipeline.OriginAuthoritative, Security: pipeline.Security{State: pipeline.ValidationUnknown}}
	resp.Transfer = func(yield func(*dns.Msg) bool) {
		i := 0
		for c := range chunks {
			m := first
			if i > 0 {
				m = s.xfrMsg(req, c)
			}
			i++
			if !yield(m) {
				s.countTransfer(z, TransferAborted)
				return
			}
		}
		s.countTransfer(z, TransferSuccess)
	}
	return resp
}

// xfrMsg builds one transfer message: AA set, the question echoed (RFC
// 5936 §2.2.1 allows it in every message), compressed, with the query's
// OPT echoed.
func (s *Stage) xfrMsg(req *pipeline.Request, rrs []dns.RR) *dns.Msg {
	m := new(dns.Msg)
	m.SetReply(req.Msg)
	m.Authoritative = true
	m.Compress = true
	m.Answer = rrs
	s.finish(req, m)
	return m
}

// ixfrSerial returns the serial of the secondary's SOA in the authority
// section of an IXFR query.
func ixfrSerial(m *dns.Msg, z *zone.Zone) (uint32, bool) {
	for _, rr := range m.Ns {
		if soa, ok := rr.(*dns.SOA); ok && dns.CanonicalName(soa.Hdr.Name) == z.Name() {
			return soa.Serial, true
		}
	}
	return 0, false
}

// Transfer implements TransferProvider: the records of the named zone for
// an outgoing transfer, in chunks of at most ChunkBytes, SOA first and
// last. serial is the requester's current serial (0 for AXFR): when it is
// current the only chunk is the SOA (RFC 1995 §2). Policy is the caller's
// job; the stage's own AXFR/IXFR answers enforce zoneTransfer before
// calling into the same code. The channel is buffered with every chunk and
// closed, so a consumer that stops early leaks nothing.
func (s *Stage) Transfer(name string, serial uint32) (<-chan []dns.RR, error) {
	// Once captured serving is wired, request-free capability calls cannot
	// select a coherent cut. Call TransferCaptured with the retained request.
	if s.Capture != nil {
		return nil, pipeline.ErrServingCapture
	}
	return s.transferChunks(s.zones(), name, serial)
}

// TransferCaptured reads only the typed zone.Set supplied by Stage.Capture.
// Chunk resources are borrowed from req.Serving; callers retain/clone that
// lease through consumption. IXFR SOA and AXFR fallback use this same set.
func (s *Stage) TransferCaptured(req *pipeline.Request, name string, serial uint32) (<-chan []dns.RR, error) {
	if req == nil || req.Serving == nil || s.Capture == nil {
		return nil, pipeline.ErrServingCapture
	}
	return s.transferChunks(s.Capture(req), name, serial)
}

func (s *Stage) transferChunks(set zone.Set, name string, serial uint32) (<-chan []dns.RR, error) {
	if set == nil || set.Gate() != zone.GateOpen {
		return nil, ErrUnsynced
	}
	z := set.Match(name)
	if z == nil || z.Name() != dns.CanonicalName(name) {
		return nil, ErrZoneNotFound
	}
	if (z.Type() != v1alpha1.ZoneTypePrimary && z.Type() != v1alpha1.ZoneTypeSecondary) || z.HasFallback() {
		return nil, ErrNotTransferable
	}
	if !z.AvailableAt(s.now()) {
		return nil, ErrUnsynced
	}
	var chunks [][]dns.RR
	if serial != 0 && z.UpToDate(serial) {
		chunks = [][]dns.RR{{z.SOA()}}
	} else {
		chunks = slices.Collect(z.AXFR(s.chunkBytes()))
	}
	ch := make(chan []dns.RR, len(chunks))
	for _, c := range chunks {
		ch <- c
	}
	close(ch)
	return ch, nil
}
