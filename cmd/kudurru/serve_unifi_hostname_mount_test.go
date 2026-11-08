package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/inbound"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/dnsserver/proxyproto"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// uniFiHostnameController serves the real official and opt-in legacy wire
// contracts. It never replaces the provider, host, source transaction or DNS
// inventory owner. Each complete poll captures a coherent fixture phase; only a
// subsequent authenticated observation may change serving identity.
type uniFiHostnameController struct {
	server *httptest.Server
	phase  atomic.Int32
	calls  atomic.Int32
	key    string
	mu     sync.Mutex
	active bool
	cut    int32
	hold   *uniFiHostnamePollGate
}

type uniFiHostnamePollGate struct {
	phase            int32
	entered, release chan struct{}
	once             sync.Once
}

func (g *uniFiHostnamePollGate) resume() { g.once.Do(func() { close(g.release) }) }

// Queue a real configured poll before opening the DNS request's bounded window.
// Only the last authenticated HTTP response is held; source admission and
// complete publication still run through the production owners afterward.
func (c *uniFiHostnameController) holdPhase(phase int32) *uniFiHostnamePollGate {
	g := &uniFiHostnamePollGate{phase: phase, entered: make(chan struct{}), release: make(chan struct{})}
	c.mu.Lock()
	c.hold = g
	c.phase.Store(phase)
	c.mu.Unlock()
	return g
}

const (
	uniFiNames = iota
	uniFiConflict
	uniFiOmitted
	uniFiRenamed
	uniFiUnavailable
)

func newUniFiHostnameController(t *testing.T, key string) *uniFiHostnameController {
	t.Helper()
	c := &uniFiHostnameController{key: key}
	c.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.calls.Add(1)
		if r.Method != http.MethodGet || r.Header.Get("X-API-Key") != c.key {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if !c.active {
			c.cut, c.active = c.phase.Load(), true
		}
		phase := c.cut
		if phase == uniFiUnavailable {
			c.active = false
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		page := func(rows []map[string]any) {
			_ = json.NewEncoder(w).Encode(map[string]any{"offset": 0, "limit": 200, "count": len(rows), "totalCount": len(rows), "data": rows})
		}
		const base = "/proxy/network/integration/v1"
		const site = "11111111-1111-4111-8111-111111111111"
		const network = "22222222-2222-4222-8222-222222222222"
		switch r.URL.Path {
		case base + "/info":
			_ = json.NewEncoder(w).Encode(map[string]any{"applicationVersion": "10.6.106"})
		case base + "/sites":
			page([]map[string]any{{"id": site, "internalReference": "default"}})
		case base + "/sites/" + site + "/networks":
			page([]map[string]any{{"id": network, "name": "LAN", "management": "GATEWAY", "enabled": true, "vlanId": 10}})
		case base + "/sites/" + site + "/networks/" + network:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": network, "name": "LAN", "management": "GATEWAY", "enabled": true, "vlanId": 10, "ipv4Configuration": map[string]any{"hostIpAddress": "127.0.0.1", "prefixLength": 8}})
		case base + "/sites/" + site + "/clients":
			rows := []map[string]any{}
			if phase != uniFiOmitted {
				other := "127.0.0.3"
				if phase == uniFiConflict {
					other = "127.0.0.2"
				}
				for i, ip := range []string{"127.0.0.2", other} {
					id, mac := "33333333-3333-4333-8333-333333333333", "00:11:22:33:44:52"
					if i == 1 {
						id, mac = "44444444-4444-4444-8444-444444444444", "00:11:22:33:44:53"
					}
					rows = append(rows, map[string]any{"id": id, "name": "Shared Label", "type": "WIRED", "ipAddress": ip, "macAddress": mac})
				}
				// This valid official name has no legacy row: it must identify
				// through the official API even with enrichment unavailable.
				rows = append(rows, map[string]any{"id": "55555555-5555-4555-8555-555555555555", "name": "official.unifi.lan", "type": "WIRED", "ipAddress": "127.0.0.4", "macAddress": "00:11:22:33:44:54"})
			}
			page(rows)
		case "/proxy/network/api/s/default/rest/user", "/proxy/network/api/s/default/stat/sta", "/proxy/network/v2/api/site/default/active-leases":
			rows := []map[string]any{}
			if phase != uniFiOmitted {
				a, b := "Phone.UNIFI.lan.", "tablet.unifi.lan"
				if phase == uniFiRenamed {
					a, b = "safe.other.lan", "safe.other.lan"
				}
				rows = []map[string]any{{"mac": "00:11:22:33:44:52", "hostname": a}, {"mac": "00:11:22:33:44:53", "hostname": b}}
			}
			if r.URL.Path == "/proxy/network/v2/api/site/default/active-leases" {
				if g := c.hold; g != nil && g.phase == phase {
					close(g.entered)
					select {
					case <-g.release:
					case <-r.Context().Done():
						c.active, c.hold = false, nil
						return
					}
					c.hold = nil
				}
				c.active = false
				_ = json.NewEncoder(w).Encode(map[string]any{"dhcp_lease_info": rows})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]any{"meta": map[string]any{"rc": "ok"}, "data": rows})
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	c.server.Config.ErrorLog = log.New(io.Discard, "", 0)
	c.server.StartTLS()
	t.Cleanup(c.server.Close)
	return c
}

func (c *uniFiHostnameController) definition(path string) *public.Integration {
	sum := sha256.Sum256(c.server.Certificate().Raw)
	return &public.Integration{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "Integration"}, ObjectMeta: metav1.ObjectMeta{Name: "hostname", Namespace: "default"}, Spec: public.IntegrationSpec{Type: "unifi", Credentials: map[string]public.CredentialReference{"apiKey": {File: path}}, UniFi: &public.UniFiIntegration{Endpoint: c.server.URL, TLSFingerprint: "sha256:" + hex.EncodeToString(sum[:]), Legacy: true, Poll: metav1.Duration{Duration: 5 * time.Second}, Inventory: &public.UniFiInventory{ZoneRef: public.ObjectRef{Name: "home"}, NameTemplate: "{{ .Name | slug }}", ReverseZones: "disabled"}}}}
}

// This starts the real serve command assembly: licensed HTTPS acquisition,
// guarded durable source admission, complete publication, policy selection,
// response cache, native UDP/TCP and authenticated edge PROXYv2 decoding.
func TestRunServeUniFiHostnamePolicyLifecycle(t *testing.T) {
	runServeUniFiHostnamePolicyLifecycle(t, true)
}

// Repeat the held original-publication boundary without first spending the
// assignment's lifetime on the independent collision/expiry/restoration matrix.
func TestRunServeUniFiHostnameHeldPublicationDeadline(t *testing.T) {
	runServeUniFiHostnamePolicyLifecycle(t, false)
}

func runServeUniFiHostnamePolicyLifecycle(t *testing.T, fullLifecycle bool) {
	t.Helper()
	controller := newUniFiHostnameController(t, "unifi-fixture-one")
	successor := newUniFiHostnameController(t, "unifi-fixture-two")
	successor.phase.Store(uniFiRenamed)
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.Blocking.DefaultResponse = "nxdomain"
	cfg.DNSSEC.Enabled = false
	cfg.Cache.MaxEntries = 256
	secret := bytes.Repeat([]byte{0x63}, 32)
	cfg.Listeners.ProxyProtocolFrom = []string{"127.0.0.1/32"}
	cfg.Listeners.ProxyProtocolSecretFile = filepath.Join(t.TempDir(), "proxy-secret")
	if err := os.WriteFile(cfg.Listeners.ProxyProtocolSecretFile, secret, 0600); err != nil {
		t.Fatal("write fixture proxy secret")
	}
	paths := []string{filepath.Join(t.TempDir(), "first"), filepath.Join(t.TempDir(), "second")}
	for i, key := range []string{controller.key, successor.key} {
		if err := os.WriteFile(paths[i], []byte(key), 0600); err != nil {
			t.Fatal("write fixture controller credential")
		}
	}
	cfg.Credentials.AllowedFiles = paths

	// Hold one real upstream CNAME response while the original inventory owner
	// is withdrawn and replaced. The response-side alias decision must retain
	// the first request's name/membership, even after fresh DNS no longer has it.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var unhold sync.Once
	resume := func() { unhold.Do(func() { close(release) }) }
	defer resume()
	started := make(chan struct{})
	up := &dns.Server{PacketConn: pc, NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		a := new(dns.Msg)
		a.SetReply(q)
		a.RecursionAvailable = true
		name := q.Question[0].Name
		if name == "held.alias.test." {
			once.Do(func() { close(entered) })
			select {
			case <-release:
			case <-time.After(10 * time.Second):
			}
			a.Answer = append(a.Answer, &dns.CNAME{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 0}, Target: "exact.test."})
			name = "exact.test."
		}
		a.Answer = append(a.Answer, &dns.A{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 0}, A: net.IPv4(192, 0, 2, 90)})
		_ = w.WriteMsg(a)
	})}
	upDone := make(chan error, 1)
	go func() { upDone <- up.ActivateAndServe() }()
	<-started
	t.Cleanup(func() {
		resume()
		_ = up.Shutdown()
		if err := <-upDone; err != nil {
			t.Error(err)
		}
	})
	cfg.Upstreams.Groups = map[string]config.UpstreamGroup{"default": {Servers: []string{pc.LocalAddr().String()}, Strategy: "strict", Timeout: config.Duration(5 * time.Second)}}
	cfg.ObjectsDir = t.TempDir()
	sourceFile := filepath.Join(cfg.ObjectsDir, "integration.yaml")
	writeUniFiHostnameSource(t, sourceFile, controller.definition(paths[0]))
	seedUniFiHostnameMount(t, cfg)
	sign := trustTestKey(t)
	key := sign(license.Claims{Plan: license.PlanPersonal, Nodes: 3, Features: []license.Feature{license.FeatureIntegrations}})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, key) }()
	t.Cleanup(func() {
		resume()
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error("UniFi mount shutdown failed")
			}
		case <-time.After(15 * time.Second):
			t.Error("UniFi mount shutdown did not join")
		}
	})
	until := time.Now().Add(20 * time.Second)
	for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK {
		select {
		case err := <-done:
			t.Fatalf("UniFi mount ended before readiness: failed=%t", err != nil)
		default:
		}
		if time.Now().After(until) {
			t.Fatal("UniFi mount never ready")
		}
		time.Sleep(20 * time.Millisecond)
	}

	edgeAddress := startUniFiHostnameEdge(t, cfg, secret)
	edgeConfig := *cfg
	edgeConfig.Listeners.DNS = append([]config.DNSListener(nil), cfg.Listeners.DNS...)
	edgeConfig.Listeners.DNS[0].Address = edgeAddress
	transports := []string{"udp", "tcp", "edge-udp", "edge-tcp"}
	if edgeAddress != "" {
		transports = append(transports, "dnsdist-udp", "dnsdist-tcp")
	}
	exchange := func(source, transport, name string, authenticated bool) (*dns.Msg, error) {
		if transport == "dnsdist-udp" {
			return uniFiHostnameExchange(t.Context(), &edgeConfig, nil, source, "udp", name, true)
		}
		if transport == "dnsdist-tcp" {
			return uniFiHostnameExchange(t.Context(), &edgeConfig, nil, source, "tcp", name, true)
		}
		return uniFiHostnameExchange(t.Context(), cfg, secret, source, transport, name, authenticated)
	}
	// A verdict may withdraw before its source operation acknowledges the full
	// primary publication. The next independent acquisition starts after that
	// acknowledgment plus Poll. Bound observations by Poll + the existing host
	// operation limit, without extending any request or source deadline.
	observationBound := controller.definition(paths[0]).Spec.UniFi.Poll.Duration + 30*time.Second
	want := func(source, transport, name string, blocked bool) {
		t.Helper()
		until := time.Now().Add(observationBound)
		for {
			a, err := exchange(source, transport, name, true)
			if err == nil && uniFiHostnameAnswer(a, blocked) {
				return
			}
			if time.Now().After(until) {
				t.Fatalf("UniFi decision source=%s transport=%s name=%s blocked=%t error=%t", source, transport, name, blocked, err != nil)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	var heldDeadline time.Time
	awaitTransport := func(source, transport, name string, blocked bool) *dns.Msg {
		t.Helper()
		until := time.Now().Add(observationBound)
		if !heldDeadline.IsZero() && heldDeadline.Before(until) {
			until = heldDeadline
		}
		ctx, cancel := context.WithDeadline(t.Context(), until)
		defer cancel()
		for {
			a, err := uniFiHostnameExchange(ctx, cfg, secret, source, transport, name, true)
			if err == nil && uniFiHostnameAnswer(a, blocked) {
				return a
			}
			if time.Now().After(until) {
				t.Fatalf("UniFi identity did not converge source=%s name=%s blocked=%t phase=%d attempts=%d successor_attempts=%d", source, name, blocked, controller.phase.Load(), controller.calls.Load(), successor.calls.Load())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	await := func(source, name string, blocked bool) *dns.Msg {
		t.Helper()
		return awaitTransport(source, "udp", name, blocked)
	}
	phase := func(next int32) {
		t.Helper()
		controller.phase.Store(next)
		until := time.Now().Add(observationBound)
		if !heldDeadline.IsZero() && heldDeadline.Before(until) {
			until = heldDeadline
		}
		for {
			doc := uniFiHostnameDocument(t, cfg)
			present, renamed, phone := 0, 0, false
			for _, client := range doc.Snapshot.Clients {
				present += len(client.IPs)
				phone = phone || len(client.IPs) > 0 && strings.EqualFold(strings.TrimSuffix(client.Hostname, "."), "phone.unifi.lan")
				if len(client.IPs) > 0 && client.Hostname == "safe.other.lan" {
					renamed++
				}
			}
			if next == uniFiConflict && present == 1 || next == uniFiOmitted && present == 0 || next == uniFiNames && present == 3 && phone || next == uniFiRenamed && present == 3 && renamed == 2 {
				return
			}
			if time.Now().After(until) {
				t.Fatalf("controller phase=%d was not durably accepted; attempts=%d", next, controller.calls.Load())
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	await("127.0.0.2", "exact.test.", true)
	await("127.0.0.3", "glob.test.", true)
	// Wait for the scheduled acquisition before starting the original DNS
	// deadline. Waiting for file reconciliation or the next Poll while holding
	// DNS could legitimately consume that entire five-second request lifetime.
	poll := controller.holdPhase(uniFiRenamed)
	defer poll.resume()
	select {
	case <-poll.entered:
	case <-time.After(observationBound):
		t.Fatal("renamed UniFi poll did not reach authenticated response gate")
	}
	// Establish the policy actually served immediately before this request;
	// reaching the next poll alone does not prove the old inventory is live.
	before, err := exchange("127.0.0.2", "tcp", "exact.test.", true)
	if err != nil || !uniFiHostnameAnswer(before, true) {
		code := -1
		if before != nil {
			code = before.Rcode
		}
		t.Fatalf("original hostname policy unavailable at held request capture: rcode=%d transport_error=%t assignment_remaining=%s", code, err != nil, time.Until(uniFiHostnameDeadline(t, cfg, "shared-label.example.")).Round(time.Millisecond))
	}
	heldAt := time.Now()
	heldDeadline = heldAt.Add(dnsserver.DefaultQueryTimeout)
	held := make(chan *dns.Msg, 1)
	go func() {
		a, _ := exchange("127.0.0.2", "tcp", "held.alias.test.", true)
		held <- a
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("held alias did not reach actual upstream")
	}
	// Complete that original real acquisition and observe its durable renamed
	// source and fresh policy before releasing the held upstream response.
	poll.resume()
	phase(uniFiRenamed)
	await("127.0.0.2", "exact.test.", false)
	resume()
	select {
	case a := <-held:
		if !uniFiHostnameAnswer(a, true) || time.Since(heldAt) >= dnsserver.DefaultQueryTimeout {
			code := -1
			if a != nil {
				code = a.Rcode
			}
			t.Fatalf("held CNAME response lost original policy: rcode=%d elapsed=%s", code, time.Since(heldAt).Round(time.Millisecond))
		}
		t.Logf("held original hostname policy retained across accepted inventory change in %s (original deadline %s)", time.Since(heldAt).Round(time.Millisecond), dnsserver.DefaultQueryTimeout)
	case <-time.After(5 * time.Second):
		t.Fatal("held alias did not complete")
	}
	heldDeadline = time.Time{}
	// Independently qualify endpoint/allowlisted-credential replacement through
	// the production file loader. Restore Phone first so an earlier renamed
	// observation cannot impersonate the replacement provider's publication.
	phase(uniFiNames)
	await("127.0.0.2", "exact.test.", true)

	for _, transport := range transports {
		want("127.0.0.2", transport, "exact.test.", true)
		want("127.0.0.2", transport, "glob.test.", false) // Exact hostname tier beats nameGlob.
		want("127.0.0.3", transport, "glob.test.", true)
		want("127.0.0.3", transport, "exact.test.", false)
		want("127.0.0.4", transport, "exact.test.", false)
		want("127.0.0.4", transport, "glob.test.", true)
	}
	if _, err := exchange("127.0.0.2", "edge-udp", "exact.test.", false); err == nil {
		t.Fatal("wrong edge credential admitted inventory identity")
	}
	if controller.calls.Load() < 13 {
		t.Fatal("real authenticated complete UniFi acquisition was not exercised")
	}

	if fullLifecycle {
		allocations := map[string]string{}
		for _, name := range []string{"shared-label.example.", "shared-label-2.example."} {
			a := awaitTransport("127.0.0.4", "tcp", name, false)
			if !uniFiHostnameAnswer(a, false) || !a.Authoritative {
				t.Fatal("allocated source name missing from mounted authority")
			}
			allocations[name] = a.Answer[0].(*dns.A).A.String()
		}
		if allocations["shared-label.example."] == allocations["shared-label-2.example."] {
			t.Fatal("colliding display names collapsed two assignments")
		}

		phase(uniFiConflict)
		await("127.0.0.2", "exact.test.", false)
		await("127.0.0.3", "glob.test.", false)
		await("127.0.0.4", "glob.test.", true)
		phase(uniFiNames)
		await("127.0.0.2", "exact.test.", true)
		await("127.0.0.3", "glob.test.", true)
		phase(uniFiOmitted)
		await("127.0.0.2", "exact.test.", false)
		await("127.0.0.3", "glob.test.", false)
		phase(uniFiNames)
		await("127.0.0.2", "exact.test.", true)
		// Capture the real persisted assignment deadline before failing the source.
		// A temporary policy withdrawal alone is not evidence of expiry.
		expiry := uniFiHostnameDeadline(t, cfg, "shared-label.example.")
		controller.phase.Store(uniFiUnavailable)
		if remaining := time.Until(expiry); remaining <= 0 || remaining > 3*controller.definition(paths[0]).Spec.UniFi.Poll.Duration {
			t.Fatal("persisted original assignment has an invalid remaining lifetime")
		}
		timer := time.NewTimer(time.Until(expiry))
		select {
		case <-timer.C:
		case <-t.Context().Done():
			timer.Stop()
			t.Fatal("assignment expiry observation canceled")
		}
		await("127.0.0.2", "exact.test.", false)
		await("127.0.0.3", "glob.test.", false)
		if fresh := uniFiHostnameDeadline(t, cfg, "shared-label.example."); !fresh.Equal(expiry) {
			t.Fatal("failed polls changed the original persisted assignment deadline")
		}
		for _, transport := range transports {
			want("127.0.0.2", transport, "exact.test.", false)
			want("127.0.0.4", transport, "glob.test.", false)
		}
		// At the exact deadline an expired primary image refuses with SERVFAIL
		// until its replacement is published. Both that refusal and the settled
		// NXDOMAIN/NODATA must contain no retained source answer.
		for name := range allocations {
			a, err := exchange("127.0.0.4", "tcp", name, true)
			if err != nil || a == nil || len(a.Answer) != 0 || a.Rcode != dns.RcodeNameError && a.Rcode != dns.RcodeSuccess && a.Rcode != dns.RcodeServerFailure {
				code, answers := -1, -1
				if a != nil {
					code, answers = a.Rcode, len(a.Answer)
				}
				t.Fatalf("expired source record query: rcode=%d answers=%d transport_error=%t", code, answers, err != nil)
			}
		}
		phase(uniFiNames)
		await("127.0.0.2", "exact.test.", true)
		for name, ip := range allocations {
			// Inspect the exact successful TCP observation. A second request
			// would acquire a different cut during continuous source polling.
			a := awaitTransport("127.0.0.4", "tcp", name, false)
			if !uniFiHostnameAnswer(a, false) || a.Answer[0].(*dns.A).A.String() != ip {
				t.Fatal("restoration changed stable name allocation")
			}
		}
	}

	writeUniFiHostnameSource(t, sourceFile, successor.definition(paths[1]))
	// Fresh requests must acquire only the successor hostname, never the old
	// source identity or a fallback to an earlier live claim for this address.
	await("127.0.0.2", "successor.test.", true)
	for _, transport := range transports {
		want("127.0.0.2", transport, "exact.test.", false)
		want("127.0.0.3", transport, "successor.test.", true)
	}
	if successor.calls.Load() < 13 {
		t.Fatal("replacement endpoint/credential was not acquired")
	}
}

func writeUniFiHostnameSource(t *testing.T, path string, definition *public.Integration) {
	t.Helper()
	raw, err := json.Marshal(definition)
	if err != nil {
		t.Fatal("encode UniFi file source")
	}
	// Atomic file replacement is the same production fsnotify/load path used
	// for operator source rotation; no unsupported generic Integration API.
	staged := path + ".new"
	if err := os.WriteFile(staged, raw, 0600); err != nil {
		t.Fatal("stage UniFi file source")
	}
	if err := os.Rename(staged, path); err != nil {
		t.Fatal("publish UniFi file source")
	}
}

func seedUniFiHostnameMount(t *testing.T, cfg *config.Config) {
	t.Helper()
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	st, local, _, err := openStore(t.Context(), cfg, v, prometheus.NewRegistry(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close(); _ = st.Close() })
	meta := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name, Namespace: "default"} }
	objects := []store.Object{
		&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "home"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: "example.", Nameservers: []string{"ns.example."}, Policy: public.ZonePolicy{Notify: public.NotifyNone}}},
	}
	for _, matcher := range []struct {
		name     string
		selector public.ClientMatcher
	}{
		{"exact", public.ClientMatcher{Hostname: "phone.unifi.lan"}},
		{"glob", public.ClientMatcher{NameGlob: "*.unifi.lan"}},
		{"successor", public.ClientMatcher{Hostname: "safe.other.lan"}},
	} {
		objects = append(objects,
			&public.ClientGroup{ObjectMeta: meta(matcher.name), Spec: public.ClientGroupSpec{Clients: []public.ClientMatcher{matcher.selector}}},
			&public.BlockList{ObjectMeta: meta(matcher.name), Spec: public.BlockListSpec{Sources: []public.ListSource{{Inline: []string{matcher.name + ".test"}, Format: "domains"}}}},
			&public.Policy{ObjectMeta: meta(matcher.name), Spec: public.PolicySpec{ClientGroupRefs: []string{matcher.name}, BlockListRefs: []string{matcher.name}}},
		)
	}
	for _, obj := range objects {
		if _, err := st.Apply(t.Context(), obj, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal("seed UniFi objects", err)
		}
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

func uniFiHostnameAnswer(a *dns.Msg, blocked bool) bool {
	if a == nil {
		return false
	}
	if blocked {
		return a.Rcode == dns.RcodeNameError && len(a.Answer) == 0
	}
	return a.Rcode == dns.RcodeSuccess && len(a.Answer) == 1 && a.Answer[0].Header().Rrtype == dns.TypeA
}

func uniFiHostnameExchange(ctx context.Context, cfg *config.Config, secret []byte, source, transport, name string, authenticated bool) (*dns.Msg, error) {
	edge := transport == "edge-udp" || transport == "edge-tcp"
	tcp := transport == "tcp" || transport == "edge-tcp"
	network := "udp"
	dialer := &net.Dialer{Timeout: time.Second, LocalAddr: &net.UDPAddr{IP: net.ParseIP(source)}}
	if tcp {
		network, dialer.LocalAddr = "tcp", &net.TCPAddr{IP: net.ParseIP(source)}
	}
	q := new(dns.Msg)
	q.SetQuestion(name, dns.TypeA)
	if !edge {
		// miekg/dns also applies Client.Dialer.Timeout to response reads.
		// Bound connection setup independently so held queries can complete
		// inside the server's unchanged original five-second request deadline.
		raw, err := dialer.DialContext(ctx, network, cfg.Listeners.DNS[0].Address)
		if err != nil {
			return nil, err
		}
		defer func() { _ = raw.Close() }()
		client := &dns.Client{Net: network, ReadTimeout: dnsserver.DefaultQueryTimeout + time.Second, WriteTimeout: time.Second}
		a, _, err := client.ExchangeWithConnContext(ctx, q, &dns.Conn{Conn: raw})
		return a, err
	}
	raw, err := net.DialTimeout(network, cfg.Listeners.DNS[0].Address, time.Second)
	if err != nil {
		return nil, err
	}
	defer func() { _ = raw.Close() }()
	_ = raw.SetDeadline(time.Now().Add(time.Second))
	token := secret
	if !authenticated {
		token = bytes.Repeat([]byte{0x64}, 32)
	}
	kind := proxyproto.TransportDatagram
	if tcp {
		kind = proxyproto.TransportStream
	}
	h := proxyproto.Header{Command: proxyproto.CommandProxy, Family: proxyproto.FamilyINET, Transport: kind, Source: netip.AddrPortFrom(netip.MustParseAddr(source), 5353), Destination: netip.MustParseAddrPort(cfg.Listeners.DNS[0].Address), RawTLVs: proxyproto.AppendTLV(nil, proxyproto.TLVTypeSecret, token)}
	header, err := h.Append(nil)
	if err != nil {
		return nil, err
	}
	if tcp {
		if _, err := raw.Write(header); err != nil {
			return nil, err
		}
		co := &dns.Conn{Conn: raw}
		if err := co.WriteMsg(q); err != nil {
			return nil, err
		}
		return co.ReadMsg()
	}
	wire, err := q.Pack()
	if err != nil {
		return nil, err
	}
	if _, err := raw.Write(append(header, wire...)); err != nil {
		return nil, err
	}
	buf := make([]byte, 4096)
	n, err := raw.Read(buf)
	if err != nil {
		return nil, err
	}
	a := new(dns.Msg)
	return a, a.Unpack(buf[:n])
}

// Decode a single read-only SQLite SELECT snapshot of the actual source head
// and immutable chunks. No serving/store callback or provider is substituted.
func uniFiHostnameDocument(t *testing.T, cfg *config.Config) inbound.Document {
	t.Helper()
	dsn := (&url.URL{Scheme: "file", Path: filepath.Join(cfg.Node.DataDir, sqlite.FileName), RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal("open original source observation")
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error("close original source observation")
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	rows, err := db.QueryContext(ctx, "SELECT kind,data FROM objects WHERE kind IN ('IntegrationSourceHead','IntegrationSourceChunk') LIMIT 16")
	if err != nil {
		t.Fatal("read original source observation")
	}
	defer func() { _ = rows.Close() }()
	image := inbound.SourceImage{}
	chunks := map[string]*kinds.IntegrationSourceChunk{}
	for rows.Next() {
		var kind string
		var raw []byte
		if rows.Scan(&kind, &raw) != nil {
			t.Fatal("read original source carrier")
		}
		if kind == kinds.KindIntegrationSourceHead {
			if image.Head != nil {
				t.Fatal("unexpected source owner in isolated fixture")
			}
			image.Head = new(kinds.IntegrationSourceHead)
			if json.Unmarshal(raw, image.Head) != nil {
				t.Fatal("decode original source head")
			}
		} else {
			c := new(kinds.IntegrationSourceChunk)
			if json.Unmarshal(raw, c) != nil {
				t.Fatal("decode original source chunk")
			}
			chunks[c.Name] = c
		}
	}
	if rows.Err() != nil || image.Head == nil {
		t.Fatal("complete original source observation")
	}
	for _, ref := range image.Head.Spec.Chunks {
		image.Chunks = append(image.Chunks, chunks[ref.Name])
	}
	doc, err := inbound.DecodeSource(image)
	if err != nil {
		t.Fatal("validate original source snapshot")
	}
	return doc
}

func uniFiHostnameDeadline(t *testing.T, cfg *config.Config, name string) time.Time {
	t.Helper()
	doc := uniFiHostnameDocument(t, cfg)
	for _, rs := range doc.Snapshot.Desired.RecordSets {
		for _, e := range rs.Spec.Endpoints {
			if e.DNSName == name && e.Kudurru != nil && e.Kudurru.ExpiresAt != nil {
				return e.Kudurru.ExpiresAt.Time
			}
		}
	}
	t.Fatal("original source deadline absent")
	return time.Time{}
}
