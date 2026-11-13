package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/license"
)

// A mounted node keeps answering its zones across canonical commits (D395).
// Two direct UDP probes, one for a name of a primary zone and one for a
// name of a forward zone, are sent every 5 ms for the whole run while the
// test writes unrelated objects (a BlockList created and deleted), the
// forward zone's query ACL, the primary zone's query ACL and the primary
// zone's record set. Every probe must get an answer: no SERVFAIL and no
// silence. The primary name must answer one of the addresses its record
// set has had (the old or the new committed data), and after each write
// the latest one.
//
// Before D395 (QUAL-OPEN3 fix-measure03, CORE-COMMIT-GAP measure01) a write
// of a zone's own DNSZone answered that zone SERVFAIL for up to about 0.3 s:
// until the driver had refreshed the zone's lifecycle receipt and, for a
// primary, committed a head of the new configuration, every publication
// served it as committed-unavailable coverage. And any publication swap
// dropped the queries in flight on the replaced publication, about one in a
// few thousand: their answers were computed from the publication that had
// just been replaced and were withheld without a reply.
func TestRunServeSQLiteZonesAnswerAcrossCommits(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	upstream := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		answer := new(dns.Msg)
		answer.SetReply(q)
		answer.RecursionAvailable = true
		answer.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300}, A: net.IPv4(192, 0, 2, 80)}}
		_ = w.WriteMsg(answer)
	})}
	go func() { _ = upstream.ActivateAndServe() }()
	t.Cleanup(func() { _ = upstream.Shutdown() })
	cfg, _ := mountedTestConfig(t)
	licenseKey := trustTestKey(t)(license.Claims{Plan: license.PlanPersonal, Nodes: 3})
	cfg.Auth.IdentityProviders = nil
	cfg.DNSSEC.Enabled = false
	cfg.Cache.MaxEntries = 0
	cfg.RateLimit.Enabled = false
	cfg.Listeners.Internal.Address = offlineFreeAddress(t, "tcp")
	cfg.Blocking.Enabled = true
	cfg.Upstreams.Groups = map[string]config.UpstreamGroup{"default": {Servers: []string{"controlled=" + pc.LocalAddr().String()}, Strategy: "strict", Timeout: config.Duration(2 * time.Second)}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, licenseKey) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error("serve ended", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("serve failed to join shutdown")
		}
	})
	for deadline := time.Now().Add(60 * time.Second); offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK; {
		if time.Now().After(deadline) {
			t.Fatal("the node did not become ready")
		}
		time.Sleep(30 * time.Millisecond)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	base := "http://" + cfg.Listeners.API.Address + api.BasePath
	var login api.SessionInfo
	controlMountedRequest(t, client, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &login)

	const primaryName, forwardName = "probe.private.example.", "probe.forward.example."
	primary := &public.DNSZone{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "DNSZone"}, ObjectMeta: metav1.ObjectMeta{Name: "gap-primary"}, Spec: public.DNSZoneSpec{ZoneName: "private.example.", Type: public.ZoneTypePrimary, Policy: public.ZonePolicy{QueryAccess: public.AccessAllow}}}
	controlMountedRequest(t, client, base, http.MethodPost, "/objects/dnszones", login.CSRF, primary, http.StatusCreated, nil)
	records := &public.DNSRecordSet{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "DNSRecordSet"}, ObjectMeta: metav1.ObjectMeta{Name: "gap-primary", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "gap-primary"}, Endpoints: []public.Endpoint{{DNSName: primaryName, RecordType: "A", Targets: []string{"192.0.2.81"}, RecordTTL: 300}}}}
	controlMountedRequest(t, client, base, http.MethodPost, "/objects/dnsrecordsets", login.CSRF, records, http.StatusCreated, nil)
	forward := &public.DNSZone{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "DNSZone"}, ObjectMeta: metav1.ObjectMeta{Name: "gap-forward"}, Spec: public.DNSZoneSpec{ZoneName: "forward.example.", Type: public.ZoneTypeForward, Forward: &public.ForwardSpec{UpstreamGroup: "default"}, Policy: public.ZonePolicy{QueryAccess: public.AccessAllow}}}
	controlMountedRequest(t, client, base, http.MethodPost, "/objects/dnszones", login.CSRF, forward, http.StatusCreated, nil)
	awaitMountedDNS(t, cfg, primaryName, "192.0.2.81")
	// A forwarded answer is not authoritative, so awaitMountedDNS does not
	// apply to it.
	for deadline := time.Now().Add(time.Minute); ; {
		q := new(dns.Msg)
		q.SetQuestion(forwardName, dns.TypeA)
		m, _, err := (&dns.Client{Timeout: 200 * time.Millisecond}).Exchange(q, cfg.Listeners.DNS[0].Address)
		if err == nil && m.Rcode == dns.RcodeSuccess && len(m.Answer) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the forward zone never answered", err, m)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Let the zones' own follow-up commits (status, lifecycle, head) settle.
	time.Sleep(2 * time.Second)

	var (
		mu        sync.Mutex
		committed = []string{"192.0.2.81"}
		probes    = map[string]int{}
		failures  []string
		kinds     = map[string]int{}
	)
	address := cfg.Listeners.DNS[0].Address
	probe := func(name string) {
		sent := time.Now()
		q := new(dns.Msg)
		q.SetQuestion(name, dns.TypeA)
		m, _, err := (&dns.Client{Timeout: 1500 * time.Millisecond}).Exchange(q, address)
		mu.Lock()
		defer mu.Unlock()
		probes[name]++
		got, kind := "", ""
		switch {
		case err != nil || m == nil:
			got, kind = fmt.Sprintf("no response (%v)", err), "no response"
		case m.Rcode != dns.RcodeSuccess || len(m.Answer) != 1:
			got, kind = fmt.Sprintf("%s with %d answers", dns.RcodeToString[m.Rcode], len(m.Answer)), dns.RcodeToString[m.Rcode]
		default:
			a, ok := m.Answer[0].(*dns.A)
			switch {
			case !ok:
				got = "not an A record"
			case name == forwardName && a.A.String() != "192.0.2.80":
				got = "forwarded " + a.A.String()
			case name == primaryName && !slices.Contains(committed, a.A.String()):
				got = "uncommitted " + a.A.String()
			}
		}
		if got != "" {
			if kind == "" {
				kind = "wrong answer"
			}
			kinds[name+" "+kind]++
			failures = append(failures, fmt.Sprintf("%s at %s: %s", name, sent.Format("15:04:05.000"), got))
		}
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for _, name := range []string{primaryName, forwardName} {
		wg.Go(func() {
			tick := time.NewTicker(5 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-stop:
					return
				case <-tick.C:
				}
				wg.Go(func() { probe(name) })
			}
		})
	}

	settle := 1500 * time.Millisecond
	writes := 0
	write := func(method, path string, body any, want int) {
		t.Helper()
		controlMountedRequest(t, client, base, method, path, login.CSRF, body, want, nil)
		writes++
		time.Sleep(settle)
	}
	const rounds = 4
	for r := range rounds {
		controlMountedRequest(t, client, base, http.MethodGet, "/objects/dnszones/gap-primary", "", nil, http.StatusOK, primary)
		primary.Spec.Policy = public.ZonePolicy{QueryAccess: public.AccessUseNetworkACL, QueryACL: []string{"127.0.0.0/8"}}
		write(http.MethodPut, "/objects/dnszones/gap-primary", primary, http.StatusOK)
		controlMountedRequest(t, client, base, http.MethodGet, "/objects/dnszones/gap-primary", "", nil, http.StatusOK, primary)
		primary.Spec.Policy = public.ZonePolicy{QueryAccess: public.AccessAllow}
		write(http.MethodPut, "/objects/dnszones/gap-primary", primary, http.StatusOK)

		next := fmt.Sprintf("192.0.2.%d", 100+r)
		mu.Lock()
		committed = append(committed, next)
		mu.Unlock()
		controlMountedRequest(t, client, base, http.MethodGet, "/objects/dnsrecordsets/default/gap-primary", "", nil, http.StatusOK, records)
		records.Spec.Endpoints[0].Targets = []string{next}
		write(http.MethodPut, "/objects/dnsrecordsets/default/gap-primary", records, http.StatusOK)
		awaitMountedDNS(t, cfg, primaryName, next)

		controlMountedRequest(t, client, base, http.MethodGet, "/objects/dnszones/gap-forward", "", nil, http.StatusOK, forward)
		forward.Spec.Policy = public.ZonePolicy{QueryAccess: public.AccessUseNetworkACL, QueryACL: []string{"127.0.0.0/8"}}
		write(http.MethodPut, "/objects/dnszones/gap-forward", forward, http.StatusOK)
		controlMountedRequest(t, client, base, http.MethodGet, "/objects/dnszones/gap-forward", "", nil, http.StatusOK, forward)
		forward.Spec.Policy = public.ZonePolicy{QueryAccess: public.AccessAllow}
		write(http.MethodPut, "/objects/dnszones/gap-forward", forward, http.StatusOK)

		list := &public.BlockList{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "BlockList"}, ObjectMeta: metav1.ObjectMeta{Name: "gap-unrelated", Namespace: "default"}, Spec: public.BlockListSpec{Mode: "block", Sources: []public.ListSource{{Name: "inline", Format: "adblock", Inline: []string{"||unrelated.example^"}}}}}
		write(http.MethodPost, "/objects/blocklists", list, http.StatusCreated)
		write(http.MethodDelete, "/objects/blocklists/default/gap-unrelated", nil, http.StatusNoContent)
	}
	close(stop)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	t.Logf("%d writes; %d probes of the primary name, %d of the forward name; %d without a correct answer %v", writes, probes[primaryName], probes[forwardName], len(failures), kinds)
	if probes[primaryName] < 1000 || probes[forwardName] < 1000 {
		t.Fatalf("too few probes: %v", probes)
	}
	// A SERVFAIL or a wrong answer is never a matter of timing. Under the
	// race detector a publication is about ten times slower and a probe can
	// still miss its 1.5 s timeout on shared CPUs, so only there the silent
	// probes are bounded by one per commit; `make test` (hack/go-test.py,
	// NORACE_TESTS) runs this test a second time without the race detector,
	// where every probe must be answered.
	silent := kinds[primaryName+" no response"] + kinds[forwardName+" no response"]
	if raceEnabled && silent == len(failures) && silent <= writes {
		return
	}
	if len(failures) != 0 {
		shown := failures
		if len(shown) > 20 {
			shown = shown[:20]
		}
		t.Fatalf("%d probes across %d commits got no correct answer; first:\n%s", len(failures), writes, strings.Join(shown, "\n"))
	}
}
