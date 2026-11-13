package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// F28-OPEN6: a primary that enters Conflict (or is fenced) right after the
// committed-status writer wrote its zone's status, and before the driver
// realigned the zone lifecycle's source receipt to the new DNSZone
// resourceVersion, can never realign: a DNS-only node commits nothing. The
// e2e run fix-e2e02-conflict.log showed such a healed conflicted managed
// member answering SERVFAIL for its own zone (`complete serving generation
// unavailable: dns_only`), which a Conflict node holding valid trust must
// never do. This reproduces that durable state on one actual runServe
// database: a status-only write to the DNSZone after the last realignment,
// then an equal-epoch conflict, a physical close and a restart. The node must
// answer its zone's committed names authoritatively on every transport. The
// serving rule that admits a status-only receipt difference is 6780f27d
// ("keep serving a zone across its own status write"); before it the zone
// answered SERVFAIL here indefinitely.
func TestRunServeDirectAnchorsConflictServesAcrossUnrealignedStatusWrite(t *testing.T) {
	cfg, _ := mountedTestConfig(t)
	t.Setenv(license.EnvKey, "")
	t.Setenv(license.EnvKeyFile, "")
	cfg.Auth.IdentityProviders = nil
	cfg.Blocking.Enabled = false
	cfg.DNSSEC.Enabled = false
	cfg.Cache.MaxEntries = 0
	endpoint, seeds, reads := anchorSupervisorServer(t, func(int32, *dns.Msg) {})
	cfg.Upstreams.Recursion.ManagedTopology = "direct"
	cfg.Upstreams.Recursion.AnchorMode = config.RootAnchorsManaged
	cfg.Upstreams.Recursion.DNSSEC = true
	cfg.Upstreams.Recursion.TrustAnchors = seeds
	cfg.Upstreams.Recursion.RootHints = []string{endpoint}
	cfg.Upstreams.Recursion.IPv6 = "off"
	files, roots := tsigMountedCertificate(t)
	tsigMountedTLSListeners(t, cfg, files)
	st, closeStore := tsigMountedStore(t, cfg)
	for _, object := range []store.Object{
		&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "anchor"}, Spec: public.DNSZoneSpec{ZoneName: "anchor.test.", Type: public.ZoneTypePrimary, Nameservers: []string{"ns.anchor.test."}, Policy: public.ZonePolicy{Notify: public.NotifyNone}}},
		&public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "answer", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "anchor"}, Endpoints: []public.Endpoint{{DNSName: "answer.anchor.test.", RecordType: "A", Targets: []string{"192.0.2.77"}}}}},
	} {
		if _, err := st.Apply(t.Context(), object, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	closeStore()
	stop := tsigMountedStart(t, cfg)
	awaitConflictFixtureSettled(t, cfg, "anchor", "answer.anchor.test.", "192.0.2.77")
	stop()
	if reads.Load() != 1 {
		t.Fatal("writer did not bootstrap managed trust exactly once", reads.Load())
	}

	// The durable state the conflicted member was left in: the zone's
	// status (and so its resourceVersion) moved after the driver's last
	// realignment, and the member then observed an equal-epoch conflicting
	// term before it could realign.
	opened, closeOpened := tsigMountedStore(t, cfg)
	zone := &public.DNSZone{}
	if err := opened.Get(t.Context(), store.Key{Kind: "DNSZone", Name: "anchor"}, zone); err != nil {
		t.Fatal(err)
	}
	before := zone.ResourceVersion
	zone.Status.Conditions = append(zone.Status.Conditions, metav1.Condition{Type: "FixtureObserved", Status: metav1.ConditionTrue,
		Reason: "StatusOnlyWrite", Message: "status written after the last lifecycle realignment", LastTransitionTime: metav1.Now()})
	if err := opened.UpdateStatus(t.Context(), zone); err != nil {
		t.Fatal(err)
	}
	if err := opened.Get(t.Context(), store.Key{Kind: "DNSZone", Name: "anchor"}, zone); err != nil || zone.ResourceVersion == before {
		t.Fatal("fixture status write did not move the zone's resourceVersion", err)
	}
	term := opened.Authority().Term
	term.PrimaryID, term.PromotionID = "successor", "promotion-conflict-status"
	_, _ = opened.ObserveAuthority(t.Context(), term) // reports the conflict it records
	if lost := opened.Authority(); lost.Mode != replication.Conflict || lost.Writable() {
		t.Fatal("fixture did not durably reach Conflict", lost.Mode)
	}
	closeOpened()

	stop = tsigMountedStart(t, cfg)
	defer stop()
	tsigMountedAwaitPendingListeners(t, cfg)
	authoritative := func(m *dns.Msg) bool {
		if m == nil || m.Rcode != dns.RcodeSuccess || !m.Authoritative || len(m.Answer) != 1 {
			return false
		}
		a, ok := m.Answer[0].(*dns.A)
		return ok && a.A.String() == "192.0.2.77"
	}
	var last *dns.Msg
	deadline := time.Now().Add(30 * time.Second)
	for {
		query := new(dns.Msg)
		query.SetQuestion("answer.anchor.test.", dns.TypeA)
		m, _, err := (&dns.Client{Timeout: 200 * time.Millisecond}).Exchange(query, cfg.Listeners.DNS[0].Address)
		if err == nil && authoritative(m) {
			break
		}
		if err == nil {
			last = m
		}
		if time.Now().After(deadline) {
			rcode := "none"
			if last != nil {
				rcode = dns.RcodeToString[last.Rcode]
			}
			t.Fatal("conflicted managed node did not answer its zone across an unrealigned status write; last rcode", rcode, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// From the first authoritative answer on, every transport answers it,
	// and keeps answering it for two seconds (no SERVFAIL, no relapse).
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		for _, protocol := range []string{"udp", "tcp", "dot", "doh", "doq", "doh3"} {
			query := new(dns.Msg)
			query.SetQuestion("answer.anchor.test.", dns.TypeA)
			query.Id = 0 // DoQ requires the DNS message ID to be zero.
			raw, err := query.Pack()
			if err != nil {
				t.Fatal(err)
			}
			m := new(dns.Msg)
			if err := m.Unpack(tsigMountedExchange(t, cfg, roots, protocol, raw)); err != nil {
				t.Fatal(err)
			}
			if !authoritative(m) {
				t.Fatal("conflicted managed node stopped answering its zone", protocol, dns.RcodeToString[m.Rcode])
			}
		}
	}
}

// awaitConflictFixtureSettled is awaitMountedPrimarySettled with its zone
// reads taken as observations: a read answered 503 while a publication is
// pending (a sign-in or read across the zone's own status write) is read
// again instead of failing the fixture. It returns once the zone's Ready
// status is current and stays the same across one more DNS answer, so the
// driver's realignment has been committed before the node stops.
func awaitConflictFixtureSettled(t *testing.T, cfg *config.Config, zoneObject, name, address string) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := &http.Client{Jar: jar, Transport: transport, Timeout: 5 * time.Second}
	base := "http://" + cfg.Listeners.API.Address + api.BasePath
	awaitMountedDNS(t, cfg, name, address)
	deadline := time.Now().Add(time.Minute)
	for {
		if conflictFixtureLogin(client, base) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("mounted primary never accepted a sign-in")
		}
		time.Sleep(50 * time.Millisecond)
	}
	read := func() (string, bool) {
		response, err := client.Get(base + "/objects/dnszones/" + zoneObject)
		if err != nil {
			return "", false
		}
		defer func() { _ = response.Body.Close() }()
		body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		var zone public.DNSZone
		if err != nil || response.StatusCode != http.StatusOK || json.Unmarshal(body, &zone) != nil {
			return "", false
		}
		ready := meta.FindStatusCondition(zone.Status.Conditions, public.ConditionReady)
		return zone.ResourceVersion, ready != nil && ready.Status == metav1.ConditionTrue && ready.ObservedGeneration == zone.Generation
	}
	for {
		before, ready := read()
		if ready {
			awaitMountedDNS(t, cfg, name, address)
			if after, still := read(); still && after == before {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("mounted primary zone status never settled", zoneObject)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// conflictFixtureLogin signs the fixture administrator in and reports success.
func conflictFixtureLogin(client *http.Client, base string) bool {
	raw, err := json.Marshal(api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}) // #nosec G101 -- disposable fixture credential
	if err != nil {
		return false
	}
	response, err := client.Post(base+"/auth/login", "application/json", bytes.NewReader(raw))
	if err != nil {
		return false
	}
	_ = response.Body.Close()
	return response.StatusCode == http.StatusOK
}
