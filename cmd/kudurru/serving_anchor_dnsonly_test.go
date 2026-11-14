package main

import (
	"testing"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// A fenced writer must publish its last-good data as a DNS-only bundle through
// the root anchor owner. Before the fix, the inert token check always ran as a
// writable publication, preparation failed with ErrFenced, and the retired
// writable bundle made every authoritative answer an empty SERVFAIL.
func TestRootAnchorPreparationPublishesFencedDNSOnly(t *testing.T) {
	f := newPrimaryNetworkFixtureWithOutput(t, updateOutputObserver(func(*pipeline.Request, *pipeline.Response, error) {}))
	soa := func(tcp bool) *dns.Msg {
		q := new(dns.Msg)
		q.SetQuestion("example.", dns.TypeSOA)
		m, err := f.exchange(q, tcp)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	for _, tcp := range []bool{false, true} {
		if m := soa(tcp); m.Rcode != dns.RcodeSuccess || !m.Authoritative || len(m.Answer) != 1 {
			t.Fatal("writable fixture did not answer authoritatively", m.Rcode)
		}
	}
	term := f.store.Authority().Term
	term.Epoch++
	term.PrimaryID, term.PromotionID = "other", "later"
	if _, err := f.store.ObserveAuthority(t.Context(), term); err != nil {
		t.Fatal(err)
	}
	if a := f.store.Authority(); a.Mode != replication.Fenced || a.Writable() {
		t.Fatal("fixture did not durably fence the writer", a.Mode)
	}
	f.wait(t, func() bool { return f.publisher.Status().Reason == "dns_only" })
	status := f.publisher.Status()
	if !status.DNSOnly || status.ProtectedReady || !status.Served.Base.Term.Same(f.store.Authority().Installed.Term) {
		t.Fatal("fenced publication was not DNS-only last-good data", status.Reason)
	}
	if _, err := f.publisher.CaptureHTTP(t.Context()); err == nil {
		t.Fatal("DNS-only publication admitted protected HTTP")
	}
	for _, tcp := range []bool{false, true} {
		if m := soa(tcp); m.Rcode != dns.RcodeSuccess || !m.Authoritative || len(m.Answer) != 1 {
			t.Fatal("fenced node did not keep answering its last-good authoritative data", m.Rcode)
		}
	}
}
