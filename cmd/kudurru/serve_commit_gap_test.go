package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
// Three direct UDP probes, one for a name of a primary zone, one for a name
// of a primary child zone under it and one for a name of a forward zone,
// are sent every 5 ms for the whole run while the test writes unrelated
// objects (a BlockList created and deleted), the forward zone's query ACL,
// the primary zone's query ACL and name servers, the child zone's query ACL
// and the primary zone's record set. Every probe must get an answer: no
// SERVFAIL and no silence. The primary name must answer one of the
// addresses its record set has had (the old or the new committed data);
// once a new address has been seen published and servedSuccessorGrace has
// passed, a probe sent after that never gets an older address again.
//
// The child is created right after its parent, while the parent's own
// follow-up commits change its DNSZone. Before D404 that could leave the
// pair SERVFAIL for good ("zone awaiting committed publication"): the
// child's fence required both zones' exact lifecycle receipts, and neither
// could realign its receipt before the child was fenced
// (TestLateChildFencesAParentWhoseSourceAdvanced in sqliteadapter,
// ancestry_receipt_test.go in the driver).
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
	// call sends one API request of the test's own setup and writes. A 503
	// generation_pending is a refusal before any effect (the newest
	// committed generation is not yet published; common under the race
	// detector), and the SQLite command path reports the same as "current
	// writable serving generation unavailable": as in the zone-group mount
	// tests, the request is asked again for the first always, and for the
	// second only when it is safe to repeat (not a POST). What the test
	// asserts is DNS, never these requests.
	pending := 0
	call := func(method, path string, body any, want int, out any) {
		t.Helper()
		var encoded []byte
		if body != nil {
			if encoded, err = json.Marshal(body); err != nil {
				t.Fatal(err)
			}
		}
		for deadline := time.Now().Add(30 * time.Second); ; {
			req, err := http.NewRequestWithContext(t.Context(), method, base+path, bytes.NewReader(encoded))
			if err != nil {
				t.Fatal(err)
			}
			if body != nil {
				req.Header.Set("Content-Type", "application/json")
			}
			req.Header.Set("X-Kudurru-CSRF", login.CSRF)
			response, err := client.Do(req)
			if err != nil {
				t.Fatal("control request failed", method, path, err)
			}
			raw, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
			_ = response.Body.Close()
			if err != nil {
				t.Fatal("control response unreadable", method, path, err)
			}
			if response.StatusCode == want {
				if out != nil {
					if err := json.Unmarshal(raw, out); err != nil {
						t.Fatal("control response decoding failed", method, path, err)
					}
				}
				return
			}
			again := response.StatusCode == http.StatusServiceUnavailable && (bytes.Contains(raw, []byte("generation_pending")) ||
				method != http.MethodPost && bytes.Contains(raw, []byte("current writable serving generation unavailable")))
			if !again || time.Now().After(deadline) {
				t.Fatalf("control %s %s: status=%d want=%d: %s", method, path, response.StatusCode, want, raw)
			}
			pending++
			time.Sleep(25 * time.Millisecond)
		}
	}

	const primaryName, childName, forwardName = "probe.private.example.", "probe.child.private.example.", "probe.forward.example."
	primary := &public.DNSZone{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "DNSZone"}, ObjectMeta: metav1.ObjectMeta{Name: "gap-primary"}, Spec: public.DNSZoneSpec{ZoneName: "private.example.", Type: public.ZoneTypePrimary, Policy: public.ZonePolicy{QueryAccess: public.AccessAllow}}}
	call(http.MethodPost, "/objects/dnszones", primary, http.StatusCreated, nil)
	records := &public.DNSRecordSet{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "DNSRecordSet"}, ObjectMeta: metav1.ObjectMeta{Name: "gap-primary", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "gap-primary"}, Endpoints: []public.Endpoint{{DNSName: primaryName, RecordType: "A", Targets: []string{"192.0.2.81"}, RecordTTL: 300}}}}
	call(http.MethodPost, "/objects/dnsrecordsets", records, http.StatusCreated, nil)
	// A primary child of the probed primary, created with it: its own
	// writes must not take the parent out of service, nor the parent's
	// writes the child. Before D404 this pair could leave one of them
	// SERVFAIL for good (CORE-COMMIT-GAP fix-13, fix-15);
	// TestRunServeSQLitePrimaryParentAndChildZones covers creation in both
	// orders and deletion.
	child := &public.DNSZone{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "DNSZone"}, ObjectMeta: metav1.ObjectMeta{Name: "gap-child"}, Spec: public.DNSZoneSpec{ZoneName: "child.private.example.", Type: public.ZoneTypePrimary, Policy: public.ZonePolicy{QueryAccess: public.AccessAllow}}}
	call(http.MethodPost, "/objects/dnszones", child, http.StatusCreated, nil)
	childRecords := &public.DNSRecordSet{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "DNSRecordSet"}, ObjectMeta: metav1.ObjectMeta{Name: "gap-child", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "gap-child"}, Endpoints: []public.Endpoint{{DNSName: childName, RecordType: "A", Targets: []string{"192.0.2.90"}, RecordTTL: 300}}}}
	call(http.MethodPost, "/objects/dnsrecordsets", childRecords, http.StatusCreated, nil)
	forward := &public.DNSZone{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "DNSZone"}, ObjectMeta: metav1.ObjectMeta{Name: "gap-forward"}, Spec: public.DNSZoneSpec{ZoneName: "forward.example.", Type: public.ZoneTypeForward, Forward: &public.ForwardSpec{UpstreamGroup: "default"}, Policy: public.ZonePolicy{QueryAccess: public.AccessAllow}}}
	call(http.MethodPost, "/objects/dnszones", forward, http.StatusCreated, nil)
	awaitMountedDNS(t, cfg, primaryName, "192.0.2.81")
	awaitMountedDNS(t, cfg, childName, "192.0.2.90")
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
		mu sync.Mutex
		// committed lists every address the primary name's record set has
		// had, oldest first; superseded[i] is when committed[i+1] was seen
		// published, after which committed[i] may only answer a probe sent
		// within servedSuccessorGrace.
		committed  = []string{"192.0.2.81"}
		superseded []time.Time
		probes     = map[string]int{}
		failures   []string
		kinds      = map[string]int{}
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
			case name == childName && a.A.String() != "192.0.2.90":
				got = "child " + a.A.String()
			case name == primaryName && !slices.Contains(committed, a.A.String()):
				got = "uncommitted " + a.A.String()
			case name == primaryName:
				if i := slices.Index(committed, a.A.String()); i < len(superseded) && sent.After(superseded[i].Add(servedSuccessorGrace)) {
					got = fmt.Sprintf("superseded %s (replaced at %s)", a.A.String(), superseded[i].Format("15:04:05.000"))
				}
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
	for _, name := range []string{primaryName, childName, forwardName} {
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
		call(method, path, body, want, nil)
		writes++
		time.Sleep(settle)
	}
	const rounds = 4
	for r := range rounds {
		call(http.MethodGet, "/objects/dnszones/gap-primary", nil, http.StatusOK, primary)
		primary.Spec.Policy = public.ZonePolicy{QueryAccess: public.AccessUseNetworkACL, QueryACL: []string{"127.0.0.0/8"}}
		write(http.MethodPut, "/objects/dnszones/gap-primary", primary, http.StatusOK)
		call(http.MethodGet, "/objects/dnszones/gap-primary", nil, http.StatusOK, primary)
		primary.Spec.Policy = public.ZonePolicy{QueryAccess: public.AccessAllow}
		write(http.MethodPut, "/objects/dnszones/gap-primary", primary, http.StatusOK)
		// A name-server change rewrites the zone's apex: the parent keeps
		// answering its committed head until the driver commits the new one.
		call(http.MethodGet, "/objects/dnszones/gap-primary", nil, http.StatusOK, primary)
		primary.Spec.Nameservers = []string{fmt.Sprintf("ns%d.example.net.", r)}
		write(http.MethodPut, "/objects/dnszones/gap-primary", primary, http.StatusOK)

		// The child's own writes, while the parent and the child are probed.
		call(http.MethodGet, "/objects/dnszones/gap-child", nil, http.StatusOK, child)
		child.Spec.Policy = public.ZonePolicy{QueryAccess: public.AccessUseNetworkACL, QueryACL: []string{"127.0.0.0/8"}}
		write(http.MethodPut, "/objects/dnszones/gap-child", child, http.StatusOK)
		call(http.MethodGet, "/objects/dnszones/gap-child", nil, http.StatusOK, child)
		child.Spec.Policy = public.ZonePolicy{QueryAccess: public.AccessAllow}
		write(http.MethodPut, "/objects/dnszones/gap-child", child, http.StatusOK)

		next := fmt.Sprintf("192.0.2.%d", 100+r)
		mu.Lock()
		committed = append(committed, next)
		mu.Unlock()
		call(http.MethodGet, "/objects/dnsrecordsets/default/gap-primary", nil, http.StatusOK, records)
		records.Spec.Endpoints[0].Targets = []string{next}
		write(http.MethodPut, "/objects/dnsrecordsets/default/gap-primary", records, http.StatusOK)
		awaitMountedDNS(t, cfg, primaryName, next)
		mu.Lock()
		for len(superseded) < len(committed)-1 {
			superseded = append(superseded, time.Now())
		}
		mu.Unlock()

		call(http.MethodGet, "/objects/dnszones/gap-forward", nil, http.StatusOK, forward)
		forward.Spec.Policy = public.ZonePolicy{QueryAccess: public.AccessUseNetworkACL, QueryACL: []string{"127.0.0.0/8"}}
		write(http.MethodPut, "/objects/dnszones/gap-forward", forward, http.StatusOK)
		call(http.MethodGet, "/objects/dnszones/gap-forward", nil, http.StatusOK, forward)
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
	t.Logf("%d writes (%d setup requests asked again after 503); %d probes of the primary name, %d of the child name, %d of the forward name; %d without a correct answer %v", writes, pending, probes[primaryName], probes[childName], probes[forwardName], len(failures), kinds)
	if probes[primaryName] < 1000 || probes[childName] < 1000 || probes[forwardName] < 1000 {
		t.Fatalf("too few probes: %v", probes)
	}
	// A SERVFAIL or a wrong answer is never a matter of timing. Under the
	// race detector a publication is about ten times slower and a probe can
	// still miss its 1.5 s timeout on shared CPUs, so only there the silent
	// probes are bounded by one per commit; `make test` (hack/go-test.py,
	// NORACE_TESTS) runs this test a second time without the race detector,
	// where every probe must be answered.
	silent := kinds[primaryName+" no response"] + kinds[childName+" no response"] + kinds[forwardName+" no response"]
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
