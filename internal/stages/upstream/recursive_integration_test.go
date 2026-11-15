package upstream_test

import (
	"context"
	"crypto"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/recursor"
	cachestage "github.com/robertherbaugh/kudurru/internal/stages/cache"
	dnssecstage "github.com/robertherbaugh/kudurru/internal/stages/dnssec"
	"github.com/robertherbaugh/kudurru/internal/stages/postprocess"
	upstreamstage "github.com/robertherbaugh/kudurru/internal/stages/upstream"
	"github.com/robertherbaugh/kudurru/internal/upstream"
)

type signedRoot struct {
	key     *dns.DNSKEY
	signer  crypto.Signer
	expires time.Time
}

func (s signedRoot) Exchange(_ context.Context, q *dns.Msg, _ netip.AddrPort, _ bool) (*dns.Msg, error) {
	m := new(dns.Msg)
	m.SetReply(q)
	m.Authoritative = true
	switch q.Question[0].Qtype {
	case dns.TypeDNSKEY:
		m.Answer = []dns.RR{s.key}
	case dns.TypeA:
		m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: strings.ToLower(q.Question[0].Name), Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.IPv4(192, 0, 2, 7)}}
	case dns.TypeNS:
		m.Answer = []dns.RR{&dns.NS{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeNS, Class: dns.ClassINET, Ttl: 60}, Ns: "ns.root."}}
		m.Extra = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: "ns.root.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.IPv4(192, 0, 2, 53)}}
	}
	if len(m.Answer) > 0 {
		inception, expiration := time.Now().Add(-time.Hour).Unix(), s.expires.Unix()
		const maxTimestamp = 1<<32 - 1
		if inception < 0 || inception > maxTimestamp || expiration < 0 || expiration > maxTimestamp {
			return nil, errors.New("test signature time is outside the DNSSEC timestamp range")
		}
		sig := &dns.RRSIG{Hdr: dns.RR_Header{Name: m.Answer[0].Header().Name, Rrtype: dns.TypeRRSIG, Class: dns.ClassINET, Ttl: 60}, Algorithm: s.key.Algorithm, KeyTag: s.key.KeyTag(), SignerName: ".", Inception: uint32(inception), Expiration: uint32(expiration)}
		if err := sig.Sign(s.signer, m.Answer); err != nil {
			return nil, err
		}
		m.Answer = append(m.Answer, sig)
	}
	return m, nil
}

func TestRecursiveValidationSurvivesRawCacheAndFinalization(t *testing.T) {
	key := &dns.DNSKEY{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: 60}, Flags: 257, Protocol: 3, Algorithm: dns.ECDSAP256SHA256}
	secret, err := key.Generate(256)
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(30 * time.Second).Truncate(time.Second)
	cfg := recursor.DefaultConfig()
	cfg.RootHints = []string{"192.0.2.53:53"}
	cfg.TrustAnchors = []string{key.ToDS(dns.SHA256).String()}
	cfg.QNAMEMinimization = false
	cfg.IPv6 = recursor.IPv6Off
	rec, err := recursor.New(cfg, recursor.Options{Exchanger: signedRoot{key, secret.(crypto.Signer), expires}})
	if err != nil {
		t.Fatal(err)
	}
	set, err := upstream.New(upstream.Config{Groups: map[string]upstream.GroupConfig{"default": {Recursive: true}}, Recursion: cfg}, upstream.Options{Recursor: rec})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = set.Close() }()
	up := upstreamstage.NewStage(set)
	// Forwarded validation deliberately has different anchors. It must not
	// override native recursive validation or change the original CD request.
	defaults, err := config.FromFlat(config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	validator, err := defaults.DNSSEC.Validator(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = up.SetValidation(validator, true); err != nil {
		t.Fatal(err)
	}
	cc := cachestage.New(cache.New(cache.OptionsFrom(defaults.Cache, nil)), cachestage.SettingsFrom(defaults.Cache), up)
	defer cc.Wait()
	chain := pipeline.MustNew(&postprocess.Stage{}, cc, dnssecstage.New(up), up)
	ask := func(do, ad, cd bool) *pipeline.Response {
		t.Helper()
		q := new(dns.Msg)
		q.SetQuestion("www.example.com.", dns.TypeA)
		q.SetEdns0(1232, do)
		q.AuthenticatedData = ad
		q.CheckingDisabled = cd
		r, err := chain.Serve(&pipeline.Request{Ctx: t.Context(), Msg: q})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(r.Close)
		return r
	}
	for _, flags := range []struct{ do, ad, cd bool }{{true, false, false}, {false, true, false}, {false, false, false}, {true, false, true}} {
		for n := 0; n < 2; n++ {
			r := ask(flags.do, flags.ad, flags.cd)
			if r.Msg.Rcode != dns.RcodeSuccess || r.Msg.AuthenticatedData != (!flags.cd && (flags.do || flags.ad)) {
				t.Fatalf("flags %+v: rcode=%d AD=%v", flags, r.Msg.Rcode, r.Msg.AuthenticatedData)
			}
			if n == 1 && r.Status != pipeline.StatusCached {
				t.Fatalf("second response not cached: %v", r.Status)
			}
			if !flags.cd && (r.Security.State != pipeline.ValidationSecure || r.Security.ValidUntil.After(expires) || r.Security.ValidUntil.IsZero()) {
				t.Fatalf("lost bounded native validation: %+v", r.Security)
			}
			if !flags.do {
				for _, rr := range r.Msg.Answer {
					if rr.Header().Rrtype == dns.TypeRRSIG {
						t.Fatal("proof leaked to non-DO client")
					}
				}
			}
		}
	}
}
