package main

import (
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

// A TSIG refusal is answered by the transport before any pipeline work. It
// must reach the client on every transport while the node is between a
// canonical commit and its next publication, as an ordinary answer does
// (D378); before the fix about one refusal in four sent right after a signed
// UPDATE was withheld and the client timed out (non-race; the dnsserver test
// TestCapturedTSIGRefusalBindsOutput is the deterministic proof). Each UPDATE
// is a new request and is never retried; only its commit matters here, so
// NOERROR, the explicit SERVFAIL of an unobserved publication and a withheld
// acknowledgment are all accepted, while any refusal fails the test.
func TestRunServeTSIGRefusalAnsweredBetweenCommitAndPublication(t *testing.T) {
	t.Setenv(license.EnvKey, "")
	t.Setenv(license.EnvKeyFile, "")
	cfg, secret := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.Blocking.Enabled = false
	cfg.DNSSEC.Enabled = false
	cfg.Cache.MaxEntries = 0
	files, roots := tsigMountedCertificate(t)
	tsigMountedTLSListeners(t, cfg, files)
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	st, local, _, err := openStore(t.Context(), cfg, v, prometheus.NewRegistry(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	for _, obj := range []store.Object{
		&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "home"}, Spec: public.DNSZoneSpec{
			ZoneName: "example.", Type: public.ZoneTypePrimary, Nameservers: []string{"ns.example."},
			Policy: public.ZonePolicy{Notify: public.NotifyNone, Update: public.AccessAllow, UpdatePolicies: []public.UpdatePolicy{{TSIGKeyRef: "updates", Name: "*.example.", Types: []string{"A"}}}},
		}},
		&public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "seed", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "home"}, Endpoints: []public.Endpoint{{DNSName: "seed.example.", RecordType: "A", Targets: []string{"192.0.2.42"}}}}},
	} {
		if _, err := st.Apply(t.Context(), obj, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	if local.Close() != nil || st.Close() != nil {
		t.Fatal("close seeded store")
	}
	stop := tsigMountedStart(t, cfg)
	defer stop()
	awaitMountedDNS(t, cfg, "seed.example.", "192.0.2.42")
	address := cfg.Listeners.DNS[0].Address
	update := &dns.Client{Net: "tcp", Timeout: 5 * time.Second, TsigSecret: map[string]string{"updates.": secret}}
	// A well-formed secret that is not the configured one: BADSIG.
	wrong := "d3Jvbmctc2VjcmV0LXdyb25nLXNlY3JldC13cm9uZw=="
	protocols := []string{"udp", "tcp", "dot", "doh", "doq", "doh3"}
	committed := 0
	for i := range 6 * len(protocols) {
		protocol := protocols[i%len(protocols)]
		answer, _, err := update.Exchange(primaryUpdateMessage("example.", fmt.Sprintf("dyn%d.example.", i), true), address)
		rcode := -1 // no answer: an acknowledgment withheld across a later commit (D313)
		if err == nil {
			if answer == nil || answer.IsTsig() == nil || answer.Rcode != dns.RcodeSuccess && answer.Rcode != dns.RcodeServerFailure {
				t.Fatalf("signed UPDATE %d was refused", i)
			}
			rcode = answer.Rcode
		}
		if rcode == dns.RcodeSuccess {
			committed++
		}
		query := new(dns.Msg)
		query.SetQuestion("seed.example.", dns.TypeA)
		query.Id = 0
		query.SetTsig("updates.", dns.HmacSHA256, 300, time.Now().Unix())
		wire, _, err := dns.TsigGenerate(query, wrong, "", false)
		if err != nil {
			t.Fatal("sign refused request")
		}
		t.Logf("refusal after UPDATE %d (rcode %d): protocol=%s", i, rcode, protocol)
		refusal := new(dns.Msg)
		if refusal.Unpack(tsigMountedExchange(t, cfg, roots, protocol, wire)) != nil || refusal.Rcode != dns.RcodeNotAuth || refusal.IsTsig() == nil || refusal.IsTsig().Error != dns.RcodeBadSig || refusal.IsTsig().MAC != "" || len(refusal.Answer) != 0 {
			t.Fatalf("protocol %s: refusal after UPDATE %d is not an unsigned BADSIG", protocol, i)
		}
	}
	if committed == 0 {
		t.Fatal("no UPDATE committed")
	}
}
