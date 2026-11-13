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

// A mounted node applies a zone's own configuration change from the
// publication that carries it on (D395, CORE-OPEN4): a forward zone moved to
// another upstream group, and a primary zone whose query access is
// tightened so that the prober is refused, then loosened again. Probes run
// every 5 ms from the change until 2 s after its effect was first seen; a
// probe sent more than servedSuccessorGrace after that moment must get the
// new behavior (the new upstream's address, REFUSED, the zone's data
// again), never the old one, and no probe gets SERVFAIL. This is the mounted
// counterpart of the internal/servingzones and internal/servingbuild tests
// of the same rules.
func TestRunServeSQLiteZoneChangesApplyFromTheirPublication(t *testing.T) {
	first := zoneChangeUpstream(t, net.IPv4(192, 0, 2, 80))
	second := zoneChangeUpstream(t, net.IPv4(192, 0, 2, 70))
	cfg, _ := mountedTestConfig(t)
	licenseKey := trustTestKey(t)(license.Claims{Plan: license.PlanPersonal, Nodes: 3})
	cfg.Auth.IdentityProviders = nil
	cfg.DNSSEC.Enabled = false
	cfg.Cache.MaxEntries = 0
	cfg.RateLimit.Enabled = false
	cfg.Listeners.Internal.Address = offlineFreeAddress(t, "tcp")
	cfg.Blocking.Enabled = true
	cfg.Upstreams.Groups = map[string]config.UpstreamGroup{
		"default": {Servers: []string{"controlled=" + first}, Strategy: "strict", Timeout: config.Duration(2 * time.Second)},
		"other":   {Servers: []string{"controlled=" + second}, Strategy: "strict", Timeout: config.Duration(2 * time.Second)},
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

	const primaryName, forwardName = "probe.private.example.", "probe.forward.example."
	primary := &public.DNSZone{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "DNSZone"}, ObjectMeta: metav1.ObjectMeta{Name: "change-primary"}, Spec: public.DNSZoneSpec{ZoneName: "private.example.", Type: public.ZoneTypePrimary, Policy: public.ZonePolicy{QueryAccess: public.AccessAllow}}}
	call(http.MethodPost, "/objects/dnszones", primary, http.StatusCreated, nil)
	records := &public.DNSRecordSet{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "DNSRecordSet"}, ObjectMeta: metav1.ObjectMeta{Name: "change-primary", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "change-primary"}, Endpoints: []public.Endpoint{{DNSName: primaryName, RecordType: "A", Targets: []string{"192.0.2.81"}, RecordTTL: 300}}}}
	call(http.MethodPost, "/objects/dnsrecordsets", records, http.StatusCreated, nil)
	forward := &public.DNSZone{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "DNSZone"}, ObjectMeta: metav1.ObjectMeta{Name: "change-forward"}, Spec: public.DNSZoneSpec{ZoneName: "forward.example.", Type: public.ZoneTypeForward, Forward: &public.ForwardSpec{UpstreamGroup: "default"}, Policy: public.ZonePolicy{QueryAccess: public.AccessAllow}}}
	call(http.MethodPost, "/objects/dnszones", forward, http.StatusCreated, nil)
	awaitMountedDNS(t, cfg, primaryName, "192.0.2.81")

	address := cfg.Listeners.DNS[0].Address
	// outcome is what one probe of name got: an A address, an rcode name or
	// "no response".
	outcome := func(name string) string {
		q := new(dns.Msg)
		q.SetQuestion(name, dns.TypeA)
		m, _, err := (&dns.Client{Timeout: 1500 * time.Millisecond}).Exchange(q, address)
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
	for deadline := time.Now().Add(time.Minute); outcome(forwardName) != "192.0.2.80"; {
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
		call(http.MethodGet, path, nil, http.StatusOK, zone)
		mutate := zone.DeepCopy()
		switch what {
		case "forward to other":
			mutate.Spec.Forward.UpstreamGroup = "other"
		case "tighten":
			mutate.Spec.Policy = public.ZonePolicy{QueryAccess: public.AccessUseNetworkACL, QueryACL: []string{"192.0.2.0/24"}}
		case "loosen":
			mutate.Spec.Policy = public.ZonePolicy{QueryAccess: public.AccessAllow}
		}
		call(http.MethodPut, path, mutate, http.StatusOK, nil)
		var seen time.Time
		var bad []string
		silent, late := 0, 0
		for deadline := time.Now().Add(30 * time.Second); seen.IsZero() || time.Since(seen) < 2*time.Second; {
			sent := time.Now()
			got := outcome(name)
			switch {
			case got == want:
				if seen.IsZero() {
					seen = sent
				}
			case got == "no response":
				silent++
			case got == old && (seen.IsZero() || !sent.After(seen.Add(servedSuccessorGrace))):
				if !seen.IsZero() {
					late++
				}
			default:
				bad = append(bad, sent.Format("15:04:05.000")+" "+got)
			}
			if seen.IsZero() && time.Now().After(deadline) {
				t.Fatalf("%s: %s never answered %s after the write (last %s)", what, name, want, got)
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Logf("%s: %s answered %s from %s; %d old answers inside the grace, %d probes unanswered", what, name, want, seen.Format("15:04:05.000"), late, silent)
		if len(bad) > 0 {
			t.Fatalf("%s: %d probes of %s got neither %s nor %s within the grace: %v", what, len(bad), name, want, old, bad[:min(len(bad), 10)])
		}
	}
	change("forward to other", "/objects/dnszones/change-forward", forward, forwardName, "192.0.2.80", "192.0.2.70")
	change("tighten", "/objects/dnszones/change-primary", primary, primaryName, "192.0.2.81", "REFUSED")
	change("loosen", "/objects/dnszones/change-primary", primary, primaryName, "REFUSED", "192.0.2.81")
}
