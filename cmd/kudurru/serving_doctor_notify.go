package main

import (
	"strings"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
	"github.com/robertherbaugh/kudurru/internal/servingzones"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

// doctorNotify reserves only the health-name diagnostic. It never schedules
// secondary refreshes, sends outbound packets or mutates a primary. The native
// listener signs its response with the provider that verified this request.
func (p *primaryRuntime) doctorNotify(next pipeline.Next) pipeline.Next {
	return func(req *pipeline.Request) (*pipeline.Response, error) {
		if req == nil || req.Msg == nil {
			return nil, pipeline.ErrServingCapture
		}
		m := req.Msg
		if m.Opcode != dns.OpcodeNotify || len(m.Question) == 0 || !strings.EqualFold(m.Question[0].Name, ingress.HealthName) {
			return next(req)
		}
		reply := func(code int) (*pipeline.Response, error) {
			response := pipeline.Reply(req, code, pipeline.StatusSpecialDomain)
			response.Msg.Authoritative = true
			return response, nil
		}
		if m.Response || m.Rcode != dns.RcodeSuccess || len(m.Question) != 1 || m.Question[0].Qtype != dns.TypeSOA || m.Question[0].Qclass != dns.ClassINET || len(m.Answer) != 0 || len(m.Ns) != 0 {
			return reply(dns.RcodeFormatError)
		}
		for _, rr := range m.Extra {
			if rr == nil || rr.Header().Rrtype != dns.TypeTSIG {
				return reply(dns.RcodeFormatError)
			}
		}
		if req.Client.Synthetic || req.Client.Proto != pipeline.ProtoUDP || !req.Client.Addr.IsValid() {
			return reply(dns.RcodeRefused)
		}
		now := time.Now()
		proof, ok := dnsserver.VerifiedNotifyFor(req, now)
		if !ok {
			return reply(dns.RcodeRefused)
		}
		if p == nil || p.publisher == nil || p.driver == nil {
			return reply(dns.RcodeServerFailure)
		}
		writer := p.writer.active.Load()
		if !p.writer.current(writer) {
			return reply(dns.RcodeServerFailure)
		}
		target := serving.TermActivation{Term: writer.fence.Term, Incarnation: writer.fence.Incarnation}
		current := func() bool {
			return p.writer.current(writer) && p.driver.Status().Ready && servingdns.CheckWriter(req, p.publisher, target) == nil
		}
		if !current() {
			return reply(dns.RcodeServerFailure)
		}
		owner, err := servingdns.Component[*servingbuild.RuntimeOwner](req, "runtime")
		if err != nil || owner == nil || owner.Primary() == nil || owner.Zones() == nil {
			return reply(dns.RcodeServerFailure)
		}
		zones, err := servingdns.Component[*servingzones.Prepared](req, "zones")
		if err != nil || zones == nil || owner.Zones() != zones || !owner.Primary().Base().Same(zones.Base()) {
			return reply(dns.RcodeServerFailure)
		}
		identity, keys, keysCurrent := owner.NotifyKeys()
		if identity == "" || keys == nil || keysCurrent == nil || !keysCurrent() {
			return reply(dns.RcodeServerFailure)
		}
		key, found := keys.Identity(proof.Identity().Key)
		if !found || key != proof.Identity() {
			return reply(dns.RcodeRefused)
		}
		objects, err := servingdns.Component[*zonecache.Snapshot](req, "objects")
		if err != nil || !objects.Authoritative() {
			return reply(dns.RcodeServerFailure)
		}
		set := owner.Zones().Zones(now)
		if !set.Synced() {
			return reply(dns.RcodeServerFailure)
		}
		readyAt := func(at time.Time) bool {
			for _, desired := range objects.Objects.Zones() {
				if desired.Spec.Type != public.ZoneTypePrimary && desired.Spec.Type != public.ZoneTypeForward {
					continue
				}
				apex := dns.CanonicalName(desired.Spec.ZoneName)
				route := set.Match(apex)
				if !owner.Primary().Admitted(apex, at) || route == nil || route.Name() != apex || route.Type() != desired.Spec.Type || !route.RoutingAvailableAt(at) {
					return false
				}
			}
			return true
		}
		if !readyAt(now) {
			return reply(dns.RcodeServerFailure)
		}
		actual := time.Now()
		if _, valid := dnsserver.VerifiedNotifyFor(req, actual); !valid || actual.Before(now) || !keysCurrent() || !readyAt(actual) || !current() {
			return reply(dns.RcodeServerFailure)
		}
		response, _ := reply(dns.RcodeSuccess)
		response.Msg.Answer = []dns.RR{&dns.TXT{Hdr: dns.RR_Header{Name: ingress.HealthName, Rrtype: dns.TypeTXT, Class: dns.ClassINET}, Txt: []string{req.Client.Addr.Addr().Unmap().String()}}}
		return response, nil
	}
}
