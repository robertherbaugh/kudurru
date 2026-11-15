package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// One actual runServe database is bootstrapped as a managed/direct writer, then
// durably fenced by a higher term and later put in an equal-epoch conflict,
// with a physical close, reopen and restart for each role. Before D369 startup
// admission refused both roles ("root anchor policy unavailable or changed"),
// so such a node could not restart at all. It must start, serve its last-good
// data on every transport with protected HTTP closed, and keep only deny-only
// supervision: exactly one independent root acquisition per start, and no
// change to the accepted trust history, the canonical image or the authority.
func TestRunServeDirectAnchorsFencedAndConflictRestart(t *testing.T) {
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
	awaitMountedPrimarySettled(t, cfg, "anchor", "answer.anchor.test.", "192.0.2.77")
	stop()
	if reads.Load() != 1 {
		t.Fatal("writer did not bootstrap managed trust exactly once", reads.Load())
	}
	answer := func(protocol string) *dns.Msg {
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
		return m
	}
	lastGood := func(m *dns.Msg) bool {
		if m == nil || m.Rcode != dns.RcodeSuccess || !m.Authoritative || len(m.Answer) != 1 {
			return false
		}
		a, ok := m.Answer[0].(*dns.A)
		return ok && a.A.String() == "192.0.2.77"
	}
	want := int32(1)
	for _, role := range []replication.Mode{replication.Fenced, replication.Conflict} {
		t.Logf("managed anchor DNS-only restart role=%s", role)
		opened, closeOpened := tsigMountedStore(t, cfg)
		term := opened.Authority().Term
		term.PrimaryID, term.PromotionID = "successor", "promotion-"+string(role)
		if role == replication.Fenced {
			term.Epoch++
		}
		// Fenced: a higher authenticated term. Conflict: a different term in
		// the epoch this member already observed.
		if _, err := opened.ObserveAuthority(t.Context(), term); err != nil && role == replication.Fenced {
			t.Fatal(err)
		}
		lost := opened.Authority()
		if lost.Mode != role || lost.Writable() {
			t.Fatal("fixture did not durably reach the DNS-only role", role, lost.Mode)
		}
		before := anchorFollowerImage(t.Context(), t, opened)
		owner, err := accepted.DecodeRows(before.Payload.Runtime, before.Term.ClusterID)
		if err != nil || owner.Inspect().Phase != accepted.Managed {
			t.Fatal("fixture lost accepted managed history", err)
		}
		closeOpened()
		stop := tsigMountedStart(t, cfg)
		// Operational listeners, serving not protected-ready, HTTP auth 503.
		tsigMountedAwaitPendingListeners(t, cfg)
		// The first DNS-only publication follows listener readiness. Wait with
		// fresh tolerant queries; every assertion below is a strict exchange.
		deadline := time.Now().Add(time.Minute)
		for {
			query := new(dns.Msg)
			query.SetQuestion("answer.anchor.test.", dns.TypeA)
			m, _, err := (&dns.Client{Timeout: 200 * time.Millisecond}).Exchange(query, cfg.Listeners.DNS[0].Address)
			if err == nil && lastGood(m) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("restarted DNS-only managed node never served its last-good data", role, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
		for _, protocol := range []string{"udp", "tcp", "dot", "doh", "doq", "doh3"} {
			if m := answer(protocol); !lastGood(m) {
				t.Fatal("restarted DNS-only managed node refused last-good data", role, protocol, dns.RcodeToString[m.Rcode])
			}
		}
		want++
		for reads.Load() != want {
			if time.Now().After(deadline) {
				t.Fatal("restarted DNS-only node did not keep exactly one independent supervision poll", role, reads.Load(), want)
			}
			time.Sleep(20 * time.Millisecond)
		}
		// Past the supervisor's one-second recheck: its cooldown holds.
		time.Sleep(1500 * time.Millisecond)
		if ready := offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready"); ready == http.StatusOK {
			t.Fatal("DNS-only managed node reported protected readiness", role)
		}
		if status := offlineHTTPStatusWithTimeout(cfg.Listeners.API.Address, "/api/v1/auth/status", 3*time.Second); status != http.StatusServiceUnavailable {
			t.Fatal("DNS-only managed node opened protected HTTP", role, status)
		}
		if m := answer("tcp"); !lastGood(m) {
			t.Fatal("deny-only supervision interrupted last-good DNS", role, dns.RcodeToString[m.Rcode])
		}
		stop()
		if reads.Load() != want {
			t.Fatal("DNS-only node reacquired root evidence or polled as a writer", role, reads.Load(), want)
		}
		reopened, closeReopened := tsigMountedStore(t, cfg)
		after := reopened.Authority()
		if after.Mode != lost.Mode || !after.Term.Same(lost.Term) || !after.Installed.Same(lost.Installed) || (after.Conflict == nil) != (lost.Conflict == nil) {
			t.Fatal("DNS-only restart changed the durable authority", role, after.Mode)
		}
		image := anchorFollowerImage(t.Context(), t, reopened)
		current, err := accepted.DecodeRows(image.Payload.Runtime, image.Term.ClusterID)
		if err != nil || current.Version() != owner.Version() || !anchorFollowerSameImage(before, image) {
			t.Fatal("DNS-only restart changed accepted trust history or canonical data", role, err)
		}
		closeReopened()
	}
}

// "Last-good DNS" on a recursive resolver includes DNSSEC validation. A fenced
// managed node must keep producing AD-validated native recursion from its last
// accepted managed owner after a physical restart, on every transport, while
// its only root acquisition as a member is the deny-only supervisor's single
// TCP poll. The private root needs a real non-loopback interface, exactly as
// in TestRunServeDirectAnchorsActualRecursiveDNSKEYAndRestart.
func TestRunServeDirectAnchorsFencedRecursiveValidation(t *testing.T) {
	cfg, _ := mountedTestConfig(t)
	t.Setenv(license.EnvKey, "")
	t.Setenv(license.EnvKeyFile, "")
	cfg.Auth.IdentityProviders = nil
	cfg.Blocking.Enabled = false
	cfg.DNSSEC.Enabled = false
	cfg.Cache.MaxEntries = 0 // require the native resolver path, not outer raw hits
	endpoint, seeds, tcpKeys, udpKeys := anchorRecursiveServer(t)
	cfg.Upstreams.Groups = map[string]config.UpstreamGroup{"default": {Mode: "recursive", Strategy: "parallel_best"}}
	cfg.Upstreams.Recursion.ManagedTopology = "direct"
	cfg.Upstreams.Recursion.AnchorMode = config.RootAnchorsManaged
	cfg.Upstreams.Recursion.DNSSEC = true
	cfg.Upstreams.Recursion.TrustAnchors = seeds
	cfg.Upstreams.Recursion.RootHints = []string{endpoint}
	cfg.Upstreams.Recursion.IPv6 = "off"
	files, roots := tsigMountedCertificate(t)
	tsigMountedTLSListeners(t, cfg, files)
	validated := func(protocol string) (bool, int) {
		q := new(dns.Msg)
		q.SetQuestion(".", dns.TypeDNSKEY)
		q.SetEdns0(1232, true)
		q.Id = 0
		raw, err := q.Pack()
		if err != nil {
			t.Fatal(err)
		}
		answer := new(dns.Msg)
		if err := answer.Unpack(tsigMountedExchange(t, cfg, roots, protocol, raw)); err != nil {
			t.Fatal(err)
		}
		if answer.Rcode != dns.RcodeSuccess || !answer.AuthenticatedData || len(answer.Answer) != 2 {
			return false, answer.Rcode
		}
		var key *dns.DNSKEY
		var sig *dns.RRSIG
		for _, rr := range answer.Answer {
			switch value := rr.(type) {
			case *dns.DNSKEY:
				key = value
			case *dns.RRSIG:
				sig = value
			}
		}
		if key == nil || sig == nil || sig.Verify(key, []dns.RR{key}) != nil {
			return false, answer.Rcode
		}
		ds := key.ToDS(dns.SHA256)
		ds.Hdr.Ttl = 60
		return ds.String() == seeds[0], answer.Rcode
	}
	stop := tsigMountedStart(t, cfg)
	deadline := time.Now().Add(30 * time.Second)
	for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatal("managed recursive writer did not become ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ok, rcode := validated("udp"); !ok {
		t.Fatal("writable managed node did not validate native recursion", dns.RcodeToString[rcode])
	}
	stop()
	if tcpKeys.Load() != 1 {
		t.Fatal("writer did not bootstrap managed trust exactly once", tcpKeys.Load())
	}
	opened, closeOpened := tsigMountedStore(t, cfg)
	term := opened.Authority().Term
	term.Epoch++
	term.PrimaryID, term.PromotionID = "successor", "later"
	if _, err := opened.ObserveAuthority(t.Context(), term); err != nil {
		t.Fatal(err)
	}
	if opened.Authority().Mode != replication.Fenced {
		t.Fatal("fixture did not durably fence the writer")
	}
	before := anchorFollowerImage(t.Context(), t, opened)
	owner, err := accepted.DecodeRows(before.Payload.Runtime, before.Term.ClusterID)
	if err != nil || owner.Inspect().Phase != accepted.Managed {
		t.Fatal("fixture lost accepted managed history", err)
	}
	closeOpened()
	priorUDP := udpKeys.Load()
	stop = tsigMountedStart(t, cfg)
	tsigMountedAwaitPendingListeners(t, cfg)
	deadline = time.Now().Add(time.Minute)
	for {
		q := new(dns.Msg)
		q.SetQuestion(".", dns.TypeDNSKEY)
		q.SetEdns0(1232, true)
		m, _, err := (&dns.Client{Timeout: time.Second}).Exchange(q, cfg.Listeners.DNS[0].Address)
		if err == nil && m.Rcode == dns.RcodeSuccess && m.AuthenticatedData {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("restarted fenced managed node never validated native recursion", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, protocol := range []string{"udp", "tcp", "dot", "doh", "doq", "doh3"} {
		if ok, rcode := validated(protocol); !ok {
			t.Fatal("fenced managed node lost validated native recursion", protocol, dns.RcodeToString[rcode])
		}
	}
	for tcpKeys.Load() != 2 {
		if time.Now().After(deadline) {
			t.Fatal("fenced node did not keep exactly one independent supervision poll", tcpKeys.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	if tcpKeys.Load() != 2 || udpKeys.Load() <= priorUDP {
		t.Fatal("fenced node polled as a writer or answered without fresh native authority", tcpKeys.Load(), udpKeys.Load(), priorUDP)
	}
	reopened, closeReopened := tsigMountedStore(t, cfg)
	image := anchorFollowerImage(t.Context(), t, reopened)
	current, err := accepted.DecodeRows(image.Payload.Runtime, image.Term.ClusterID)
	if err != nil || current.Version() != owner.Version() || !anchorFollowerSameImage(before, image) || reopened.Authority().Mode != replication.Fenced {
		t.Fatal("fenced recursion or supervision changed accepted trust history, data or role", err)
	}
	closeReopened()
}
