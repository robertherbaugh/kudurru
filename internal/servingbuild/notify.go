package servingbuild

import (
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/secondary"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
	"github.com/robertherbaugh/kudurru/internal/servingzones"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

// CapturedNotify schedules secondary refresh hints using only the original
// request's complete serving lease. Unavailable secondary copies can refresh;
// primary lifecycle coverage cannot authorize a hint. The native receiver still
// validates proof/source identity and the listener owns response signing.
func CapturedNotify(receiver *secondary.NotifyReceiver, now func() time.Time) pipeline.Next {
	if now == nil {
		now = time.Now
	}
	return func(req *pipeline.Request) (*pipeline.Response, error) {
		if req == nil || req.Msg == nil {
			return nil, pipeline.ErrServingCapture
		}
		reply := func(code int) (*pipeline.Response, error) {
			response := pipeline.Reply(req, code, pipeline.StatusAuthoritative)
			response.Msg.Authoritative = true
			return response, nil
		}
		if !capturedNotifyMessage(req.Msg) {
			return reply(dns.RcodeFormatError)
		}
		if req.Client.Synthetic || (req.Client.Proto != pipeline.ProtoUDP && req.Client.Proto != pipeline.ProtoTCP && req.Client.Proto != pipeline.ProtoDoT) {
			return reply(dns.RcodeRefused)
		}
		if receiver == nil || receiver.Manager == nil || req.Ctx == nil || req.Ctx.Err() != nil {
			return reply(dns.RcodeServerFailure)
		}
		objects, err := servingdns.Component[*zonecache.Snapshot](req, "objects")
		if err != nil || !objects.Authoritative() {
			return reply(dns.RcodeServerFailure)
		}
		runtime, err := servingdns.Component[*RuntimeOwner](req, "runtime")
		if err != nil {
			return reply(dns.RcodeServerFailure)
		}
		zones, err := servingdns.Component[*servingzones.Prepared](req, "zones")
		if err != nil || runtime.Zones() != zones || runtime.Primary() == nil || !zones.Base().Same(runtime.Primary().Base()) {
			return reply(dns.RcodeServerFailure)
		}
		identity, keys, current := runtime.NotifyKeys()
		if identity == "" || len(identity) > 512 || keys == nil || current == nil || !current() {
			return reply(dns.RcodeServerFailure)
		}
		set := zones.Zones(now())
		if !set.Synced() {
			return reply(dns.RcodeServerFailure)
		}
		apex := dns.CanonicalName(req.Msg.Question[0].Name)
		route := set.Match(apex)
		if route == nil || route.Name() != apex || route.Type() != public.ZoneTypeSecondary {
			return reply(dns.RcodeNotAuth)
		}
		if req.Ctx.Err() != nil || !current() {
			return reply(dns.RcodeServerFailure)
		}
		return receiver.HandleCaptured(req, objects, keys)
	}
}

// Keep RFC 1996 framing rejection ahead of lease selection. HandleCaptured
// repeats this validation before authorization; neither path trusts hint data.
func capturedNotifyMessage(m *dns.Msg) bool {
	if m.Response || m.Opcode != dns.OpcodeNotify || m.Rcode != dns.RcodeSuccess || len(m.Question) != 1 || len(m.Ns) != 0 || len(m.Answer) > 1 {
		return false
	}
	q := m.Question[0]
	if q.Qclass != dns.ClassINET || q.Qtype != dns.TypeSOA || !dns.IsFqdn(q.Name) {
		return false
	}
	if _, valid := dns.IsDomainName(q.Name); !valid {
		return false
	}
	if len(m.Answer) == 1 {
		soa, ok := m.Answer[0].(*dns.SOA)
		if !ok || soa == nil || soa.Hdr.Class != dns.ClassINET || dns.CanonicalName(soa.Hdr.Name) != dns.CanonicalName(q.Name) {
			return false
		}
	}
	for _, rr := range m.Extra {
		if rr == nil || (rr.Header().Rrtype != dns.TypeOPT && rr.Header().Rrtype != dns.TypeTSIG) {
			return false
		}
	}
	return true
}
