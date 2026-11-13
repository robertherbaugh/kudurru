package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dhcp"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

// UniFi inventory device IDs are <site>/<MAC> (ee/integration/unifi).
const (
	inventoryPhoneID    = "11111111-1111-4111-8111-111111111111/00:11:22:33:44:52"
	inventoryTabletID   = "11111111-1111-4111-8111-111111111111/00:11:22:33:44:53"
	inventoryOfficialID = "11111111-1111-4111-8111-111111111111/00:11:22:33:44:54"
)

const (
	inventoryLeasedMAC = "02:00:5e:10:00:20"
	inventoryShortMAC  = "02:00:5e:10:00:21"
)

// DHCP needs a usable unicast subnet, so leased clients are TEST-NET-1
// addresses. Their queries arrive over UDP through the authenticated PROXYv2
// edge path from loopback, which carries the leased source address exactly as
// a trusted dnsdist edge does.
var inventoryEdgeSecret = []byte("inventory-edge-secret-0123456789")

func inventoryEdge(t *testing.T, cfg *config.Config) {
	t.Helper()
	cfg.Listeners.ProxyProtocolFrom = []string{"127.0.0.1/32"}
	cfg.Listeners.ProxyProtocolSecretFile = filepath.Join(t.TempDir(), "proxy-secret")
	if err := os.WriteFile(cfg.Listeners.ProxyProtocolSecretFile, inventoryEdgeSecret, 0o600); err != nil {
		t.Fatal(err)
	}
}

// useLoopbackDHCP binds the serve command's real DHCP service to loopback
// sockets: binding UDP 67 on an interface needs privileges this test lacks.
// Leases, stores, the lease snapshot and the management API are unchanged.
func useLoopbackDHCP(t *testing.T) {
	t.Helper()
	previous := listenDHCPService
	listenDHCPService = func(s *dhcp.Service) error {
		return s.ListenWith(func(dhcp.Config) (net.PacketConn, error) { return net.ListenPacket("udp4", "127.0.0.1:0") })
	}
	t.Cleanup(func() { listenDHCPService = previous })
}

// startInventoryUpstream answers every name with one A record.
func startInventoryUpstream(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	up := &dns.Server{PacketConn: pc, NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		a := new(dns.Msg)
		a.SetReply(q)
		a.RecursionAvailable = true
		a.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 0}, A: net.IPv4(192, 0, 2, 90)}}
		_ = w.WriteMsg(a)
	})}
	done := make(chan error, 1)
	go func() { done <- up.ActivateAndServe() }()
	<-started
	t.Cleanup(func() {
		_ = up.Shutdown()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return pc.LocalAddr().String()
}

// inventoryMACGroups seeds mac matchers for the leased MACs: "leased" blocks
// leased.test.
func inventoryMACGroups(namespace string, zone bool) []store.Object {
	meta := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name, Namespace: namespace} }
	var objects []store.Object
	if zone {
		objects = append(objects, &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "home"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: "example.", Nameservers: []string{"ns.example."}, Policy: public.ZonePolicy{Notify: public.NotifyNone}}})
	}
	for name, macs := range map[string][]string{"leased": {inventoryLeasedMAC, inventoryShortMAC}} {
		var matchers []public.ClientMatcher
		for _, mac := range macs {
			matchers = append(matchers, public.ClientMatcher{MAC: mac})
		}
		objects = append(objects,
			&public.ClientGroup{ObjectMeta: meta(name), Spec: public.ClientGroupSpec{Clients: matchers}},
			&public.BlockList{ObjectMeta: meta(name), Spec: public.BlockListSpec{Sources: []public.ListSource{{Inline: []string{name + ".test"}, Format: "domains"}}}},
			&public.Policy{ObjectMeta: meta(name), Spec: public.PolicySpec{ClientGroupRefs: []string{name}, BlockListRefs: []string{name}}},
		)
	}
	return objects
}

// inventoryAwait polls one authenticated GET until accept is satisfied. 503
// (a serving generation in flight) is retried; other statuses fail.
func inventoryAwait(t *testing.T, c *http.Client, url string, within time.Duration, what string, out any, accept func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	last := 0
	for {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		response, err := c.Do(request)
		if err == nil {
			raw, readErr := io.ReadAll(io.LimitReader(response.Body, 4<<20))
			_ = response.Body.Close()
			last = response.StatusCode
			if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusServiceUnavailable {
				cancel()
				t.Fatalf("%s: GET status %d: %s", what, response.StatusCode, raw)
			}
			if response.StatusCode == http.StatusOK && readErr == nil && json.Unmarshal(raw, out) == nil && accept() {
				cancel()
				return
			}
		}
		cancel()
		if time.Now().After(deadline) {
			t.Fatalf("%s: not observed within %s (last status %d): %+v", what, within, last, out)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// inventoryLogin signs in as admin. A 503 answer (no current writable
// serving generation yet) is retried as clients do; other answers fail.
func inventoryLogin(t *testing.T, c *http.Client, base, password string) api.SessionInfo {
	t.Helper()
	//nolint:gosec // G117: disposable loopback fixture login, never logged.
	body, err := json.Marshal(api.LoginRequest{Username: "admin", Password: password})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/auth/login", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := c.Do(request)
		if err == nil {
			raw, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
			_ = response.Body.Close()
			var session api.SessionInfo
			if response.StatusCode == http.StatusOK && readErr == nil && json.Unmarshal(raw, &session) == nil {
				return session
			}
			if response.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("login status %d", response.StatusCode)
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("login not admitted within 30s")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func inventoryByKey(body api.InventoryBody) map[string]api.InventoryDevice {
	out := map[string]api.InventoryDevice{}
	for _, d := range body.Items {
		key := d.ID
		if d.Kind == api.InventoryKindDHCP {
			key = d.Source + " " + d.ID
		}
		out[key] = d
	}
	return out
}

func inventoryAddressState(d api.InventoryDevice, ip string) string {
	for _, a := range d.Addresses {
		if a.IP == ip {
			return a.State
		}
	}
	return ""
}

// awaitInventoryDNS repeats a UDP query from source (through the trusted edge
// header) until it is blocked (NXDOMAIN) or answered as wanted.
func awaitInventoryDNS(t *testing.T, cfg *config.Config, source, name string, blocked bool) {
	t.Helper()
	awaitInventoryAnswer(t, cfg, source, name, blocked, uniFiHostnameAnswer)
}

// awaitInventoryAnswer is awaitInventoryDNS with the caller's notion of a
// blocked and an allowed answer.
func awaitInventoryAnswer(t *testing.T, cfg *config.Config, source, name string, blocked bool, answer func(*dns.Msg, bool) bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		a, err := uniFiHostnameExchange(t.Context(), cfg, inventoryEdgeSecret, source, "edge-udp", name, true)
		if err == nil && answer(a, blocked) {
			return
		}
		if time.Now().After(deadline) {
			code := -1
			if a != nil {
				code = a.Rcode
			}
			control := -1
			if c, err := uniFiHostnameExchange(t.Context(), cfg, inventoryEdgeSecret, source, "edge-udp", "control.example.", true); err == nil {
				control = c.Rcode
			}
			t.Fatalf("UDP query from %s for %s: rcode=%d transport_error=%t, want blocked=%t (control name rcode=%d)", source, name, code, err != nil, blocked, control)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// D397 on the complete SQLite runtime: the real serve command with a UniFi
// TLS double and the node's DHCP service. GET /inventory lists UniFi devices
// that never queried (and are absent from GET /clients), and DHCP leases with
// their MACs; a lease imported through the API makes a ClientGroup mac
// matcher select the group over UDP; an expired lease stops matching;
// colliding UniFi claims are listed as ambiguous; an expired source is
// flagged stale.
func TestRunServeSQLiteInventoryAPIAndLeaseMACs(t *testing.T) {
	useLoopbackDHCP(t)
	controller := newUniFiHostnameController(t, "unifi-inventory-key")
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.Blocking.DefaultResponse = "nxdomain"
	cfg.DNSSEC.Enabled = false
	cfg.Cache.MaxEntries = 0
	cfg.Querylog.Enabled = true
	cfg.Querylog.Privacy = "full"
	cfg.Querylog.Exclude.SUDN = false // the test names are under special-use .test
	cfg.Upstreams.Groups = map[string]config.UpstreamGroup{"default": {Servers: []string{startInventoryUpstream(t)}, Strategy: "strict", Timeout: config.Duration(2 * time.Second)}}
	inventoryEdge(t, cfg)
	cfg.DHCP = config.DHCP{Enabled: true, Management: "file", Scopes: []config.DHCPScope{{Name: "lan", Interface: "kdr-inventory", ServerIP: "192.0.2.2", Subnet: "192.0.2.0/24",
		RangeStart: "192.0.2.100", RangeEnd: "192.0.2.110", Router: "192.0.2.1", DNSServers: []string{"192.0.2.2"}}}}
	credential := filepath.Join(t.TempDir(), "unifi-key")
	if err := os.WriteFile(credential, []byte(controller.key), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Credentials.AllowedFiles = []string{credential}
	cfg.ObjectsDir = t.TempDir()
	writeUniFiHostnameSource(t, filepath.Join(cfg.ObjectsDir, "integration.yaml"), controller.definition(credential))
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	st, local, _, err := openStore(t.Context(), cfg, v, prometheus.NewRegistry(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	for _, obj := range inventoryMACGroups("default", true) {
		if _, err := st.Apply(t.Context(), obj, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal("seed inventory objects", err)
		}
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	key := trustTestKey(t)(license.Claims{Plan: license.PlanPersonal, Nodes: 3, Features: []license.Feature{license.FeatureIntegrations}})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, key) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("inventory mount shutdown: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("inventory mount shutdown did not join")
		}
	})
	until := time.Now().Add(30 * time.Second)
	for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK {
		select {
		case err := <-done:
			t.Fatalf("inventory mount ended before readiness: %v", err)
		default:
		}
		if time.Now().After(until) {
			t.Fatal("inventory mount never ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{DisableKeepAlives: true}
	t.Cleanup(transport.CloseIdleConnections)
	c := &http.Client{Transport: transport, Jar: jar, Timeout: 10 * time.Second}
	base := "http://" + cfg.Listeners.API.Address + api.BasePath
	tsigMountAwaitRead(t, c, base+"/auth/status", nil)
	session := inventoryLogin(t, c, base, "mounted-test-password-1")

	// UniFi devices are listed from the serving inventory before any of them
	// sends a DNS query; the official API device at 127.0.0.4 owns its address.
	var body api.InventoryBody
	inventoryAwait(t, c, base+"/inventory", 45*time.Second, "UniFi devices listed", &body, func() bool {
		d, ok := inventoryByKey(body)[inventoryOfficialID]
		return ok && len(inventoryByKey(body)) == 3 && inventoryAddressState(d, "127.0.0.4") == "assigned" && !d.Stale && d.Kind == api.InventoryKindSource && len(d.MACs) == 1 && d.ObservedAt != nil
	})
	if body.Node != cfg.Node.Name || body.Generation == 0 {
		t.Fatalf("inventory envelope: node=%q generation=%d", body.Node, body.Generation)
	}
	awaitInventoryDNS(t, cfg, "192.0.2.100", "leased.test.", false)

	expiry := time.Now().Add(time.Hour).Unix()
	// The short lease lives long enough for the import and its own match to
	// be observed first even on a loaded machine.
	short := time.Now().Add(20 * time.Second)
	leases := fmt.Sprintf("%d %s 192.0.2.100 laptop *\n%d %s 192.0.2.101 brief *\n", expiry, inventoryLeasedMAC, short.Unix(), inventoryShortMAC)
	var imported api.DHCPImportResult
	controlMountedRequest(t, c, base, http.MethodPost, "/node/dhcp/v4/scopes/lan/import", session.CSRF, api.DHCPImportRequest{Format: "dnsmasq", Data: leases}, http.StatusOK, &imported)
	if imported.Imported != 2 {
		t.Fatalf("lease import: %+v", imported)
	}
	// The lease MAC selects the mac group over UDP; an unleased address and a
	// UniFi device whose own MAC is in no group stay unaffected.
	awaitInventoryDNS(t, cfg, "192.0.2.101", "leased.test.", true)
	if time.Until(short) <= 0 {
		t.Fatal("short lease expired before its match was asserted; widen its lifetime")
	}
	awaitInventoryDNS(t, cfg, "192.0.2.100", "leased.test.", true)
	awaitInventoryDNS(t, cfg, "192.0.2.102", "leased.test.", false)
	awaitInventoryDNS(t, cfg, "127.0.0.4", "leased.test.", false)
	inventoryAwait(t, c, base+"/inventory?kind=dhcp", 10*time.Second, "DHCP leases listed", &body, func() bool {
		laptop := inventoryByKey(body)["dhcp/lan 192.0.2.100"]
		return len(body.Items) == 2 && laptop.Hostname == "laptop" && len(laptop.MACs) == 1 && laptop.MACs[0] == inventoryLeasedMAC && inventoryAddressState(laptop, "192.0.2.100") == "assigned"
	})

	// The lease expires: the query no longer matches and the lease is flagged
	// (or already removed by the lease janitor, never listed as live).
	time.Sleep(time.Until(short))
	awaitInventoryDNS(t, cfg, "192.0.2.101", "leased.test.", false)
	inventoryAwait(t, c, base+"/inventory?kind=dhcp&stale=false", 10*time.Second, "expired lease not live", &body, func() bool {
		_, live := inventoryByKey(body)["dhcp/lan 192.0.2.101"]
		return !live && len(body.Items) == 1
	})

	// GET /clients is the query-log view: devices that queried, not the
	// UniFi devices at 127.0.0.2 and 127.0.0.3 that never did.
	var clients api.ClientsBody
	inventoryAwait(t, c, base+"/clients", 20*time.Second, "query-log clients", &clients, func() bool {
		seen := map[string]bool{}
		for _, item := range clients.Items {
			seen[item.Key] = true
		}
		return seen["192.0.2.100"] && seen["127.0.0.4"] && !seen["127.0.0.2"] && !seen["127.0.0.3"]
	})

	// Two UniFi clients claim 127.0.0.2 in one poll with the same
	// observation time. The UniFi source passes the collision to the
	// inventory as a tied claim, so both devices list 127.0.0.2 as ambiguous
	// and neither identifies it; the official device keeps its own address.
	controller.phase.Store(uniFiConflict)
	inventoryAwait(t, c, base+"/inventory?kind=inventory", 45*time.Second, "colliding claims flagged", &body, func() bool {
		got := inventoryByKey(body)
		phone, tablet, official := got[inventoryPhoneID], got[inventoryTabletID], got[inventoryOfficialID]
		return len(got) == 3 && len(phone.Addresses) == 1 && len(tablet.Addresses) == 1 &&
			inventoryAddressState(phone, "127.0.0.2") == "ambiguous" && inventoryAddressState(tablet, "127.0.0.2") == "ambiguous" &&
			inventoryAddressState(official, "127.0.0.4") == "assigned"
	})

	// The controller fails: no poll renews the observations, so they turn
	// stale at their original absolute expiry and stay listed as stale.
	controller.phase.Store(uniFiUnavailable)
	inventoryAwait(t, c, base+"/inventory?kind=inventory&stale=true", 60*time.Second, "stale devices flagged", &body, func() bool {
		d, ok := inventoryByKey(body)[inventoryOfficialID]
		return ok && d.Stale && inventoryAddressState(d, "127.0.0.4") == "expired"
	})
	if !strings.HasPrefix(body.Items[0].Source, "integration-") {
		t.Fatalf("inventory source naming: %q", body.Items[0].Source)
	}
}
