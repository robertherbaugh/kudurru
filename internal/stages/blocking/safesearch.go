package blocking

import (
	"strings"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/listdata/catalogue"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
)

// CatalogueSource supplies the blocked-services and safe-search catalogue
// in service; catalogue.Holder implements it.
type CatalogueSource interface {
	Current() *catalogue.Catalogue
}

func (s *Stage) catalogue() *catalogue.Catalogue {
	if s.opts.Catalogue == nil {
		return catalogue.Embedded()
	}
	return s.opts.Catalogue.Current()
}

// safeSearch answers a search engine's host name with its safe-search
// target (D60): a CNAME from the question to the target, followed by the
// target's own records of the question type, resolved through the rest of
// the chain (cache and upstream, with the client's upstream group), so the
// answer looks like the one the vendor's documented DNS rewrite gives. An
// engine documented with addresses instead of a target is answered with
// those addresses directly (NODATA for a type without one). The TTL of
// the synthesised records is the block TTL, so switching safe search off
// takes effect as quickly as an unblock.
func (s *Stage) safeSearch(req *pipeline.Request, d *policy.Decision, rw catalogue.Rewrite, next pipeline.Next) (*pipeline.Response, error) {
	q := req.Msg.Question[0]
	live := s.live.Load()
	ttl := d.BlockTTL
	if ttl <= 0 {
		ttl = live.blockTTL
	}
	sec := ttlSeconds(ttl)
	m := new(dns.Msg)
	m.SetReply(req.Msg)
	m.RecursionAvailable = true
	out := &pipeline.Response{Msg: m, Status: pipeline.StatusBlockedSafeSearch, List: rw.List()}
	if rw.Target == "" {
		hdr := dns.RR_Header{Name: q.Name, Rrtype: q.Qtype, Class: q.Qclass, Ttl: sec}
		var rendered []string
		for _, a := range rw.Addrs {
			rendered = append(rendered, a.String())
			switch {
			case q.Qtype == dns.TypeA && a.Is4():
				m.Answer = append(m.Answer, &dns.A{Hdr: hdr, A: a.AsSlice()})
			case q.Qtype == dns.TypeAAAA && a.Is6():
				m.Answer = append(m.Answer, &dns.AAAA{Hdr: hdr, AAAA: a.AsSlice()})
			}
		}
		out.Rule = strings.Join(rendered, " ")
	} else {
		target := dns.Fqdn(rw.Target)
		out.Rule = "CNAME " + target
		m.Answer = []dns.RR{&dns.CNAME{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeCNAME, Class: q.Qclass, Ttl: sec}, Target: target}}
		if q.Qtype != dns.TypeCNAME {
			sub := *req
			sub.Msg = req.Msg.Copy()
			sub.Msg.Question[0].Name = target
			resp, err := next(&sub)
			if err != nil {
				return resp, err
			}
			if resp == nil || resp.Msg == nil {
				return nil, pipeline.ErrNoAnswer
			}
			m.Rcode = resp.Msg.Rcode
			for _, rr := range resp.Msg.Answer {
				m.Answer = append(m.Answer, dns.Copy(rr))
			}
			if len(resp.Msg.Answer) == 0 {
				for _, rr := range resp.Msg.Ns {
					m.Ns = append(m.Ns, dns.Copy(rr))
				}
			}
			out.Upstream = resp.Upstream
		}
	}
	if opt := req.Msg.IsEdns0(); opt != nil {
		m.SetEdns0(ednsUDPSize, opt.Do())
		ro := m.IsEdns0()
		ro.Option = append(ro.Option, &dns.EDNS0_EDE{InfoCode: dns.ExtendedErrorCodeForgedAnswer, ExtraText: rw.List()})
	}
	s.metrics.decision(rewriteSafeSearch, groupLabel(req))
	return out, nil
}
