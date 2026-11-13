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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/license"
)

// zoneChangeUpstream is a loopback upstream that answers every A query with
// address.
func zoneChangeUpstream(t *testing.T, address net.IP) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	upstream := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		answer := new(dns.Msg)
		answer.SetReply(q)
		answer.RecursionAvailable = true
		answer.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300}, A: address}}
		_ = w.WriteMsg(answer)
	})}
	go func() { _ = upstream.ActivateAndServe() }()
	t.Cleanup(func() { _ = upstream.Shutdown() })
	return pc.LocalAddr().String()
}

// zoneChangeNode is one mounted SQLite node of the zone change tests and
// the API session that drives it.
type zoneChangeNode struct {
	t       *testing.T
	cfg     *config.Config
	client  *http.Client
	base    string
	csrf    string
	pending atomic.Int32
}

// startZoneChangeNode runs runServe with cfg (after the tests' common
// settings) until it is ready and logs in.
func startZoneChangeNode(t *testing.T, configure func(*config.Config)) *zoneChangeNode {
	t.Helper()
	cfg, _ := mountedTestConfig(t)
	licenseKey := trustTestKey(t)(license.Claims{Plan: license.PlanPersonal, Nodes: 3})
	cfg.Auth.IdentityProviders = nil
	cfg.DNSSEC.Enabled = false
	cfg.Cache.MaxEntries = 0
	cfg.RateLimit.Enabled = false
	cfg.Listeners.Internal.Address = offlineFreeAddress(t, "tcp")
	if configure != nil {
		configure(cfg)
	}
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
	n := &zoneChangeNode{t: t, cfg: cfg, client: &http.Client{Jar: jar, Timeout: 10 * time.Second}, base: "http://" + cfg.Listeners.API.Address + api.BasePath}
	var login api.SessionInfo
	controlMountedRequest(t, n.client, n.base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &login)
	n.csrf = login.CSRF
	return n
}

// request sends one API request of the test's own setup and writes and
// reports any answer other than want. A 503 generation_pending is a refusal
// before any effect (the newest committed generation is not yet published;
// common under the race detector), and the SQLite command path reports the
// same as "current writable serving generation unavailable" (the D399
// family that CMD-RECAPTURE owns; these retries are to go with the
// zone-group mount tests' when it removes them). The request is asked
// again; a POST asked again after the second may have taken effect, so a
// later 409 "already exists" for it counts as created. What the tests
// assert is DNS, never these requests. It is safe to call from any
// goroutine.
func (n *zoneChangeNode) request(method, path string, body any, want int, out any) error {
	var encoded []byte
	if body != nil {
		var err error
		if encoded, err = json.Marshal(body); err != nil {
			return err
		}
	}
	uncertain := false
	for deadline := time.Now().Add(30 * time.Second); ; {
		req, err := http.NewRequestWithContext(n.t.Context(), method, n.base+path, bytes.NewReader(encoded))
		if err != nil {
			return err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("X-Kudurru-CSRF", n.csrf)
		response, err := n.client.Do(req)
		if err != nil {
			return fmt.Errorf("control request %s %s failed: %w", method, path, err)
		}
		raw, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
		_ = response.Body.Close()
		if err != nil {
			return fmt.Errorf("control response %s %s unreadable: %w", method, path, err)
		}
		if response.StatusCode == want {
			if out != nil {
				if err := json.Unmarshal(raw, out); err != nil {
					return fmt.Errorf("control response %s %s decoding failed: %w", method, path, err)
				}
			}
			return nil
		}
		if uncertain && want == http.StatusCreated && response.StatusCode == http.StatusConflict && bytes.Contains(raw, []byte("already exists")) {
			return nil
		}
		writable := bytes.Contains(raw, []byte("current writable serving generation unavailable"))
		again := response.StatusCode == http.StatusServiceUnavailable && (bytes.Contains(raw, []byte("generation_pending")) || writable)
		if again && writable && method == http.MethodPost {
			uncertain = true
		}
		if !again || time.Now().After(deadline) {
			return fmt.Errorf("control %s %s: status=%d want=%d: %s", method, path, response.StatusCode, want, raw)
		}
		n.pending.Add(1)
		time.Sleep(25 * time.Millisecond)
	}
}

// call is request on the test goroutine.
func (n *zoneChangeNode) call(method, path string, body any, want int, out any) {
	n.t.Helper()
	if err := n.request(method, path, body, want, out); err != nil {
		n.t.Fatal(err)
	}
}

// outcome is what one probe of name got: an A address, an rcode name or
// "no response".
func (n *zoneChangeNode) outcome(name string) string {
	q := new(dns.Msg)
	q.SetQuestion(name, dns.TypeA)
	m, _, err := (&dns.Client{Timeout: 1500 * time.Millisecond}).Exchange(q, n.cfg.Listeners.DNS[0].Address)
	switch {
	case err != nil || m == nil:
		return "no response"
	case m.Rcode != dns.RcodeSuccess:
		return dns.RcodeToString[m.Rcode]
	case len(m.Answer) != 1:
		return fmt.Sprintf("NOERROR with %d answers", len(m.Answer))
	}
	if a, ok := m.Answer[0].(*dns.A); ok {
		return a.A.String()
	}
	return "not an A record"
}

// zoneChangeEffect bounds the time from a zone write's 200 to the first probe
// that sees its effect. The write returns at its commit, before the
// publication that carries it is built and served; that takes well under a
// second without the race detector, and about ten times as long with it.
func zoneChangeEffect() time.Duration {
	if raceEnabled {
		return 30 * time.Second
	}
	return 5 * time.Second
}

// A mounted node applies a zone's own configuration change from the
// publication that carries it on (D395, CORE-OPEN4): a forward zone moved to
// another upstream group, and a primary zone whose query access is
// tightened so that the prober is refused, then loosened again. Probes run
// every 5 ms from the write until 2 s after its effect was first seen. The
// first probe with the new behavior (the new upstream's address, REFUSED, the
// zone's data again) must come within zoneChangeEffect of the write's 200; a
// probe sent more than servedSuccessorGrace after it must never get the old
// behavior; no probe gets SERVFAIL or anything else; and without the race
// detector every probe is answered (with it, at most one per write may miss
// its 1.5 s timeout, as in the commit-gap test). This is the mounted
// counterpart of the internal/servingzones and internal/servingbuild tests
// of the same rules. Old answers between the 200 and the first new one are
// allowed: the write returns at its commit, before its publication, and a
// mounted probe cannot see which publication answered it, so this test does
// not prove that the very first publication carrying a change applies it
// (internal/servingbuild does).
func TestRunServeSQLiteZoneChangesApplyFromTheirPublication(t *testing.T) {
	first := zoneChangeUpstream(t, net.IPv4(192, 0, 2, 80))
	second := zoneChangeUpstream(t, net.IPv4(192, 0, 2, 70))
	n := startZoneChangeNode(t, func(cfg *config.Config) {
		cfg.Blocking.Enabled = true
		cfg.Upstreams.Groups = map[string]config.UpstreamGroup{
			"default": {Servers: []string{"controlled=" + first}, Strategy: "strict", Timeout: config.Duration(2 * time.Second)},
			"other":   {Servers: []string{"controlled=" + second}, Strategy: "strict", Timeout: config.Duration(2 * time.Second)},
		}
	})

	const primaryName, forwardName = "probe.private.example.", "probe.forward.example."
	primary := &public.DNSZone{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "DNSZone"}, ObjectMeta: metav1.ObjectMeta{Name: "change-primary"}, Spec: public.DNSZoneSpec{ZoneName: "private.example.", Type: public.ZoneTypePrimary, Policy: public.ZonePolicy{QueryAccess: public.AccessAllow}}}
	n.call(http.MethodPost, "/objects/dnszones", primary, http.StatusCreated, nil)
	records := &public.DNSRecordSet{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "DNSRecordSet"}, ObjectMeta: metav1.ObjectMeta{Name: "change-primary", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "change-primary"}, Endpoints: []public.Endpoint{{DNSName: primaryName, RecordType: "A", Targets: []string{"192.0.2.81"}, RecordTTL: 300}}}}
	n.call(http.MethodPost, "/objects/dnsrecordsets", records, http.StatusCreated, nil)
	forward := &public.DNSZone{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "DNSZone"}, ObjectMeta: metav1.ObjectMeta{Name: "change-forward"}, Spec: public.DNSZoneSpec{ZoneName: "forward.example.", Type: public.ZoneTypeForward, Forward: &public.ForwardSpec{UpstreamGroup: "default"}, Policy: public.ZonePolicy{QueryAccess: public.AccessAllow}}}
	n.call(http.MethodPost, "/objects/dnszones", forward, http.StatusCreated, nil)
	awaitMountedDNS(t, n.cfg, primaryName, "192.0.2.81")
	for deadline := time.Now().Add(time.Minute); n.outcome(forwardName) != "192.0.2.80"; {
		if time.Now().After(deadline) {
			t.Fatal("the forward zone never answered from its upstream group")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Let the zones' own follow-up commits (status, lifecycle, head) settle.
	time.Sleep(2 * time.Second)

	// change writes the zone and probes name until want has been seen and
	// then for 2 s more. old is the outcome before the change.
	change := func(what, path string, zone *public.DNSZone, name, old, want string) {
		t.Helper()
		n.call(http.MethodGet, path, nil, http.StatusOK, zone)
		mutate := zone.DeepCopy()
		switch what {
		case "forward to other":
			mutate.Spec.Forward.UpstreamGroup = "other"
		case "tighten":
			mutate.Spec.Policy = public.ZonePolicy{QueryAccess: public.AccessUseNetworkACL, QueryACL: []string{"192.0.2.0/24"}}
		case "loosen":
			mutate.Spec.Policy = public.ZonePolicy{QueryAccess: public.AccessAllow}
		}
		n.call(http.MethodPut, path, mutate, http.StatusOK, nil)
		written := time.Now()
		var seen time.Time
		var bad []string
		silent, before, late := 0, 0, 0
		for seen.IsZero() || time.Since(seen) < 2*time.Second {
			sent := time.Now()
			got := n.outcome(name)
			switch {
			case got == want:
				if seen.IsZero() {
					seen = sent
				}
			case got == "no response":
				silent++
			case got == old && seen.IsZero():
				before++
			case got == old && !sent.After(seen.Add(servedSuccessorGrace)):
				late++
			default:
				bad = append(bad, sent.Format("15:04:05.000")+" "+got)
			}
			if seen.IsZero() && time.Since(written) > zoneChangeEffect() {
				t.Fatalf("%s: %s did not answer %s within %s of the write's 200 (last %s)", what, name, want, zoneChangeEffect(), got)
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Logf("%s: %s answered %s %s after the write's 200; %d old answers before it, %d inside the grace after it, %d probes unanswered", what, name, want, seen.Sub(written).Round(time.Millisecond), before, late, silent)
		if len(bad) > 0 {
			t.Fatalf("%s: %d probes of %s got neither %s nor %s within the grace: %v", what, len(bad), name, want, old, bad[:min(len(bad), 10)])
		}
		if silent > 0 && (!raceEnabled || silent > 1) {
			t.Fatalf("%s: %d probes of %s were not answered", what, silent, name)
		}
	}
	change("forward to other", "/objects/dnszones/change-forward", forward, forwardName, "192.0.2.80", "192.0.2.70")
	change("tighten", "/objects/dnszones/change-primary", primary, primaryName, "192.0.2.81", "REFUSED")
	change("loosen", "/objects/dnszones/change-primary", primary, primaryName, "REFUSED", "192.0.2.81")
	if p := n.pending.Load(); p > 0 {
		t.Logf("%d setup requests asked again after 503", p)
	}
}

// A primary parent that answers is edited at the same moment as a primary
// child is created under it (CORE-OPEN4). The parent's DNSZone moves on
// while the child's lifecycle is not yet fenced into it, and the child's
// object name sorts before the parent's, so the driver meets the child
// first. Before the production background guard accepted a boundary
// command against a parent whose source had only moved on, such a pair
// stayed SERVFAIL for good and readiness answered 503 "zone awaiting
// committed publication" (one pair in two on the first runs). Every pair
// must answer both its parent's and its child's data within the bound, and
// the node must be ready at the end.
func TestRunServeSQLiteParentEditedAsChildIsCreated(t *testing.T) {
	n := startZoneChangeNode(t, nil)
	zone := func(name, apex string) *public.DNSZone {
		return &public.DNSZone{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "DNSZone"}, ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: public.DNSZoneSpec{ZoneName: apex, Type: public.ZoneTypePrimary, Policy: public.ZonePolicy{QueryAccess: public.AccessAllow}}}
	}
	records := func(zoneName, owner, target string) *public.DNSRecordSet {
		return &public.DNSRecordSet{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "DNSRecordSet"}, ObjectMeta: metav1.ObjectMeta{Name: zoneName, Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: zoneName}, Endpoints: []public.Endpoint{{DNSName: owner, RecordType: "A", Targets: []string{target}, RecordTTL: 300}}}}
	}
	bound := 30 * time.Second
	if raceEnabled {
		bound = 2 * time.Minute
	}
	const pairs = 4
	for i := range pairs {
		parentName, childName := fmt.Sprintf("p%d", i), fmt.Sprintf("a%d", i)
		parentApex := fmt.Sprintf("p%d.example.", i)
		parentProbe, childProbe := "probe."+parentApex, "probe.c."+parentApex
		n.call(http.MethodPost, "/objects/dnszones", zone(parentName, parentApex), http.StatusCreated, nil)
		n.call(http.MethodPost, "/objects/dnsrecordsets", records(parentName, parentProbe, "192.0.2.81"), http.StatusCreated, nil)
		awaitMountedDNS(t, n.cfg, parentProbe, "192.0.2.81")
		time.Sleep(2 * time.Second) // let the parent's own follow-up commits settle
		var parent public.DNSZone
		n.call(http.MethodGet, "/objects/dnszones/"+parentName, nil, http.StatusOK, &parent)
		parent.Spec.Nameservers = []string{fmt.Sprintf("ns%d.example.net.", i)}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		wg.Go(func() {
			errs[0] = n.request(http.MethodPut, "/objects/dnszones/"+parentName, &parent, http.StatusOK, nil)
		})
		wg.Go(func() {
			errs[1] = n.request(http.MethodPost, "/objects/dnszones", zone(childName, "c."+parentApex), http.StatusCreated, nil)
		})
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		n.call(http.MethodPost, "/objects/dnsrecordsets", records(childName, childProbe, "192.0.2.90"), http.StatusCreated, nil)
		start := time.Now()
		var p, c string
		for {
			p, c = n.outcome(parentProbe), n.outcome(childProbe)
			if p == "192.0.2.81" && c == "192.0.2.90" {
				break
			}
			if time.Since(start) > bound {
				t.Fatalf("pair %d: %s after the child's records: parent=%s child=%s readiness=%d", i, bound, p, c, offlineHTTPStatus(n.cfg.Listeners.Ops.Address, "/healthz/ready"))
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Logf("pair %d: both answer %s after the child's records", i, time.Since(start).Round(100*time.Millisecond))
	}
	for deadline := time.Now().Add(bound); offlineHTTPStatus(n.cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK; {
		if time.Now().After(deadline) {
			t.Fatal("the node is not ready after every pair answered")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
