package main

import (
	"strings"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
)

// doctorNotify attests only to the source of this authenticated UDP packet. It
// neither grants Kubernetes write authority nor claims current zone readiness.
// The credential source is the same immutable, complete startup inventory used
// by all native listeners; a pending or refused source cannot use a file fallback.
func (r *kubeTSIGStartup) doctorNotify(next pipeline.Next) pipeline.Next {
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
		keys := r.keys()
		if keys == nil || req.Ctx.Err() != nil || !now.Before(req.Deadline) {
			return reply(dns.RcodeServerFailure)
		}
		identity, found := keys.Identity(proof.Identity().Key)
		if !found || identity != proof.Identity() {
			return reply(dns.RcodeRefused)
		}
		actual := time.Now()
		if _, valid := dnsserver.VerifiedNotifyFor(req, actual); !valid || actual.Before(now) || !actual.Before(req.Deadline) || req.Ctx.Err() != nil || r.keys() != keys {
			return reply(dns.RcodeServerFailure)
		}
		response, _ := reply(dns.RcodeSuccess)
		response.Msg.Answer = []dns.RR{&dns.TXT{Hdr: dns.RR_Header{Name: ingress.HealthName, Rrtype: dns.TypeTXT, Class: dns.ClassINET}, Txt: []string{req.Client.Addr.Addr().Unmap().String()}}}
		return response, nil
	}
}
