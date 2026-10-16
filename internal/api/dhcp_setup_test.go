package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/api/apitest"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dhcp"
	"github.com/robertherbaugh/kudurru/internal/dhcpsetup"
)

const dhcpSetupPath = BasePath + dhcpBase + "/setup"

func testDHCPSetupScope() DHCPSetupScope {
	return DHCPSetupScope{Name: "lan", Interface: "eth0", ServerIP: "192.0.2.2", Subnet: "192.0.2.0/24",
		TrustedRelays: []DHCPTrustedRelay{}, RangeStart: "192.0.2.100", RangeEnd: "192.0.2.110",
		Router: "192.0.2.1", DNSServers: []string{"192.0.2.2"}, DomainName: "home.arpa",
		LeaseDuration: "12h", DNSTTL: "30s", Reservations: []DHCPReservation{}}
}

func newDHCPSetupHarness(t *testing.T, host config.DHCP, locked []string, mods ...func(*Options)) (*harness, *dhcpsetup.Manager, string) {
	t.Helper()
	dir := t.TempDir()
	manager, err := dhcpsetup.Open(dir, host, locked)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	setup := []func(*Options){func(o *Options) { o.DHCPSetup = manager; o.Config.DHCP = manager.Effective() }}
	return newHarness(t, append(setup, mods...)...), manager, dir
}

func getDHCPSetup(t *testing.T, h *harness) DHCPSetup {
	t.Helper()
	var setup DHCPSetup
	r := h.do("GET", dhcpSetupPath, "admin", nil).want(200)
	r.json(&setup)
	if setup.ETag == "" || setup.ETag != r.Header().Get("ETag") {
		t.Fatalf("setup ETag mismatch: %+v", setup)
	}
	return setup
}

func TestDHCPSetupAvailabilityAndPermissions(t *testing.T) {
	h := newHarness(t)
	setup := getDHCPSetup(t, h)
	if setup.Node != "node-a" || setup.Available || setup.UnavailableReason == "" || setup.Enabled || setup.Scopes == nil {
		t.Fatalf("unavailable setup: %+v", setup)
	}
	body := DHCPSetupRequest{Scopes: []DHCPSetupScope{}, ScopeRevisions: map[string]string{}}
	h.do("PUT", dhcpSetupPath, "admin", body, "If-Match", setup.ETag).want(503)
	h, _, _ = newDHCPSetupHarness(t, config.DHCP{}, nil)
	setup = getDHCPSetup(t, h)
	if !setup.Available || setup.ManagedBy != "api" || setup.Revision != "1" || setup.RestartRequired || setup.Enabled || len(setup.Scopes) != 0 {
		t.Fatalf("empty setup: %+v", setup)
	}
	for _, user := range []string{"viewer", "auditor", "operator", "admin"} {
		h.do("GET", dhcpSetupPath, user, nil).want(200)
		h.do("GET", BasePath+dhcpBase+"/interfaces", user, nil).want(200)
		if user != "admin" {
			h.do("PUT", dhcpSetupPath, user, body, "If-Match", setup.ETag).want(403)
		}
	}
	h.do("GET", dhcpSetupPath, "", nil).want(401)
	h.do("GET", BasePath+dhcpBase+"/interfaces", "", nil).want(401)
}

func TestDHCPSetupSaveIsStagedAndDurable(t *testing.T) {
	h, manager, dir := newDHCPSetupHarness(t, config.DHCP{}, nil)
	initial := getDHCPSetup(t, h)
	body := DHCPSetupRequest{Enabled: true, Scopes: []DHCPSetupScope{testDHCPSetupScope()}, ScopeRevisions: map[string]string{"lan": "0"}}
	h.do("PUT", dhcpSetupPath, "admin", body).want(400)
	for _, tag := range []string{"*", dhcpSetupETag("node-b", 1), dhcpETag("node-a", "setup", 1)} {
		h.do("PUT", dhcpSetupPath, "admin", body, "If-Match", tag).want(412)
	}
	var saved DHCPSetup
	h.do("PUT", dhcpSetupPath, "admin", body, "If-Match", initial.ETag).want(200).json(&saved)
	if saved.Node != "node-a" || !saved.Enabled || !saved.RestartRequired || saved.ManagedBy != "api" || saved.Revision == initial.Revision || len(saved.Scopes) != 1 {
		t.Fatalf("saved setup: %+v", saved)
	}
	if manager.Effective().Enabled || len(manager.Effective().Scopes) != 0 || h.srv.opts.DHCP != nil {
		t.Fatal("setup save activated the running configuration")
	}
	var status DHCPStatus
	h.do("GET", BasePath+dhcpBase+"/status", "admin", nil).want(200).json(&status)
	if status.Enabled || status.Available {
		t.Fatalf("pending setup reported active: %+v", status)
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file.Name(), ".db") {
			t.Fatalf("setup save created a lease database: %s", file.Name())
		}
	}
	h.do("PUT", dhcpSetupPath, "admin", body, "If-Match", initial.ETag).want(412)
	var unchanged DHCPSetup
	h.do("PUT", dhcpSetupPath, "admin", body, "If-Match", saved.ETag).want(200).json(&unchanged)
	if unchanged.Revision != saved.Revision {
		t.Fatal("identical setup changed revision")
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := dhcpsetup.Open(dir, config.DHCP{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if state := reopened.State(); !state.Enabled || state.RestartRequired || len(state.Scopes) != 1 || state.Scopes[0].Management != "api" {
		t.Fatalf("setup lost after restart: %+v", state)
	}
	var success, failure bool
	for _, event := range h.audit.Events() {
		if event.Action == "dhcp.setup" && event.Node == "node-a" && event.Actor.Name == "admin" {
			success = success || event.Result == "success"
			failure = failure || event.Result == "error"
		}
	}
	if !success || !failure {
		t.Fatal("setup changes were not audited")
	}
}

func TestDHCPSetupAdoptionEnvironmentAndRuntimeFailure(t *testing.T) {
	scope, err := testDHCPSetupScope().config()
	if err != nil {
		t.Fatal(err)
	}
	scope.Management = "file"
	host := config.DHCP{Enabled: true, Scopes: []config.DHCPScope{scope}}
	h, _, _ := newDHCPSetupHarness(t, host, nil)
	setup := getDHCPSetup(t, h)
	body := DHCPSetupRequest{Enabled: true, Scopes: setup.Scopes, ScopeRevisions: setup.ScopeRevisions}
	r := h.do("PUT", dhcpSetupPath, "admin", body, "If-Match", setup.ETag).want(409)
	if setup.ManagedBy != "file" || r.errKey() != KeyManagedBy {
		t.Fatalf("file adoption guard: %+v %s", setup, r.Body.String())
	}
	body.Adopt = true
	h.do("PUT", dhcpSetupPath, "admin", body, "If-Match", setup.ETag).want(200).json(&setup)
	if setup.ManagedBy != "api" || !setup.RestartRequired {
		t.Fatalf("adoption did not require restart: %+v", setup)
	}
	h, _, _ = newDHCPSetupHarness(t, host, []string{"dhcp.enabled"})
	setup = getDHCPSetup(t, h)
	r = h.do("PUT", dhcpSetupPath, "admin", body, "If-Match", setup.ETag).want(409)
	if !setup.EnvironmentLocked || r.errKey() != KeyEnvLocked {
		t.Fatalf("environment takeover: %+v %s", setup, r.Body.String())
	}
	h, _, _ = newDHCPSetupHarness(t, config.DHCP{}, nil, func(o *Options) {
		o.DHCPSetupError = "interface eth0: address is not assigned"
		o.Config.DHCP.Enabled = true
	})
	setup = getDHCPSetup(t, h)
	if !setup.Available || !setup.RestartRequired || setup.RuntimeError == "" || setup.UnavailableReason != "" {
		t.Fatalf("failed runtime cannot be repaired: %+v", setup)
	}
	var status DHCPStatus
	h.do("GET", BasePath+dhcpBase+"/status", "admin", nil).want(200).json(&status)
	if status.Enabled || status.Available {
		t.Fatalf("failed runtime reported active: %+v", status)
	}
	h.do("PUT", dhcpSetupPath, "admin", body, "If-Match", setup.ETag).want(200).json(&setup)
	if setup.RuntimeError == "" || !setup.RestartRequired {
		t.Fatal("saving setup concealed an uncorrected runtime failure")
	}
	h, _, _ = newDHCPSetupHarness(t, config.DHCP{Interface: "eth0"}, nil)
	setup = getDHCPSetup(t, h)
	if setup.Enabled || setup.ManagedBy != "file" || len(setup.Scopes) != 1 {
		t.Fatalf("disabled incomplete host setup is not readable: %+v", setup)
	}
	h.do("PUT", dhcpSetupPath, "admin", body, "If-Match", setup.ETag).want(200)
}

func TestDHCPSetupRejectsIncompleteAndInvalidScopes(t *testing.T) {
	h, manager, _ := newDHCPSetupHarness(t, config.DHCP{}, nil)
	initial := getDHCPSetup(t, h)
	for _, tc := range []struct {
		name string
		edit func(*DHCPSetupRequest)
	}{
		{"enabled empty", func(r *DHCPSetupRequest) { r.Enabled = true; r.Scopes = []DHCPSetupScope{} }},
		{"disabled incomplete", func(r *DHCPSetupRequest) { r.Scopes[0].Interface = "" }},
		{"invalid subnet", func(r *DHCPSetupRequest) { r.Scopes[0].Subnet = "192.0.2.2/24" }},
		{"zero lease", func(r *DHCPSetupRequest) { r.Scopes[0].LeaseDuration = "0s" }},
		{"zero TTL", func(r *DHCPSetupRequest) { r.Scopes[0].DNSTTL = "0s" }},
		{"fractional TTL", func(r *DHCPSetupRequest) { r.Scopes[0].DNSTTL = "1.5s" }},
		{"invalid duration", func(r *DHCPSetupRequest) { r.Scopes[0].LeaseDuration = "tomorrow" }},
		{"null scope revisions", func(r *DHCPSetupRequest) { r.ScopeRevisions = nil }},
		{"negative scope revision", func(r *DHCPSetupRequest) { r.ScopeRevisions["lan"] = "-1" }},
		{"overflowed scope revision", func(r *DHCPSetupRequest) { r.ScopeRevisions["lan"] = "18446744073709551616" }},
		{"invalid reservation", func(r *DHCPSetupRequest) {
			r.Scopes[0].Reservations = []DHCPReservation{{MAC: "02:00:00:00:00:01", IP: "192.0.3.10"}}
		}},
		{"duplicate scope", func(r *DHCPSetupRequest) { r.Scopes = append(r.Scopes, r.Scopes[0]) }},
		{"scope count", func(r *DHCPSetupRequest) {
			for len(r.Scopes) < 257 {
				r.Scopes = append(r.Scopes, testDHCPSetupScope())
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := DHCPSetupRequest{Scopes: []DHCPSetupScope{testDHCPSetupScope()}, ScopeRevisions: map[string]string{"lan": "0"}}
			tc.edit(&body)
			h.do("PUT", dhcpSetupPath, "admin", body, "If-Match", initial.ETag).want(400)
			if got := manager.State(); got.Revision != 1 || len(got.Scopes) != 0 {
				t.Fatalf("invalid save changed setup: %+v", got)
			}
		})
	}
	body := DHCPSetupRequest{Scopes: []DHCPSetupScope{testDHCPSetupScope()}, ScopeRevisions: map[string]string{"lan": "0"}}
	raw, _ := json.Marshal(body)
	var unknown map[string]any
	_ = json.Unmarshal(raw, &unknown)
	unknown["management"] = "file"
	h.do("PUT", dhcpSetupPath, "admin", unknown, "If-Match", initial.ETag).want(400)
}

func TestDHCPSetupDoesNotChangeLiveScope(t *testing.T) {
	manager, err := dhcpsetup.Open(t.TempDir(), config.DHCP{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	h, store := dhcpAPIHarness(t, "api", func(o *Options) { o.DHCPSetup = manager })
	lease := dhcpAPIBind(t, store, 1)
	live := store.Configuration()
	setup := getDHCPSetup(t, h)
	scope := testDHCPSetupScope()
	scope.Name, scope.Interface = "default", "replacement0"
	h.do("PUT", dhcpSetupPath, "admin", DHCPSetupRequest{Enabled: true, Scopes: []DHCPSetupScope{scope}, ScopeRevisions: map[string]string{"default": "0"}}, "If-Match", setup.ETag).want(200)
	if !reflect.DeepEqual(live, store.Configuration()) {
		t.Fatal("staged topology changed the active allocator")
	}
	leases, err := store.List(context.Background())
	if err != nil || len(leases) != 1 || leases[0].Address != lease.Address || leases[0].ExpiresAt != lease.ExpiresAt {
		t.Fatalf("staging lost live lease: %+v %v", leases, err)
	}
}

func TestDHCPSetupCSRFAndLicense(t *testing.T) {
	h, _, _ := newDHCPSetupHarness(t, config.DHCP{}, nil)
	setup := getDHCPSetup(t, h)
	body := DHCPSetupRequest{Scopes: []DHCPSetupScope{}, ScopeRevisions: map[string]string{}}
	r := h.req("PUT", dhcpSetupPath, "", body, "If-Match", setup.ETag)
	r.AddCookie(&http.Cookie{Name: apitest.CookieName, Value: "admin", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	h.serve(r).want(403)
	r = h.req("PUT", dhcpSetupPath, "", body, "If-Match", setup.ETag, apitest.HeaderCSRF, apitest.CSRFToken)
	r.AddCookie(&http.Cookie{Name: apitest.CookieName, Value: "admin", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	h.serve(r).want(200)
	h, _, _ = newDHCPSetupHarness(t, config.DHCP{}, nil, func(o *Options) { o.License = licenseManager(t, 0); o.Membership = peers(2) })
	setup = getDHCPSetup(t, h)
	h.do("PUT", dhcpSetupPath, "admin", body, "If-Match", setup.ETag).want(402)
	h.do("GET", dhcpSetupPath, "viewer", nil).want(200)
}

func TestDiscoverDHCPInterfaces(t *testing.T) {
	interfaces := func() ([]net.Interface, error) {
		return []net.Interface{{Name: "lo", Flags: net.FlagLoopback | net.FlagUp}, {Name: "eth0", Flags: net.FlagBroadcast}}, nil
	}
	addresses := func(iface net.Interface) ([]net.Addr, error) {
		if iface.Name == "lo" {
			return []net.Addr{&net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)}}, nil
		}
		return []net.Addr{&net.IPNet{IP: net.ParseIP("2001:db8::1"), Mask: net.CIDRMask(64, 128)},
			&net.IPNet{IP: net.ParseIP("192.0.2.3"), Mask: net.CIDRMask(24, 32)},
			&net.IPNet{IP: net.ParseIP("192.0.2.3"), Mask: net.CIDRMask(24, 32)}}, nil
	}
	got, err := discoverDHCPInterfaces(interfaces, addresses)
	if err != nil || len(got) != 2 || got[0].Name != "eth0" || got[0].Up || !got[0].Broadcast || !reflect.DeepEqual(got[0].Addresses, []string{"192.0.2.3"}) || !got[1].Loopback || !got[1].Up {
		t.Fatalf("interface inventory: %+v %v", got, err)
	}
	failed := errors.New("inventory unavailable")
	if _, err := discoverDHCPInterfaces(func() ([]net.Interface, error) { return nil, failed }, addresses); !errors.Is(err, failed) {
		t.Fatalf("list failure ignored: %v", err)
	}
	if _, err := discoverDHCPInterfaces(interfaces, func(net.Interface) ([]net.Addr, error) { return nil, failed }); !errors.Is(err, failed) {
		t.Fatalf("address failure ignored: %v", err)
	}
}

func TestDHCPSetupRelayReservationRoundTrip(t *testing.T) {
	h, manager, _ := newDHCPSetupHarness(t, config.DHCP{}, nil)
	setup := getDHCPSetup(t, h)
	scope := testDHCPSetupScope()
	scope.RelayOnly, scope.RegisterDNS = true, true
	scope.Subnet, scope.Router = "198.51.100.0/24", "198.51.100.1"
	scope.RangeStart, scope.RangeEnd = "198.51.100.100", "198.51.100.110"
	scope.TrustedRelays = []DHCPTrustedRelay{{SourceIP: "192.0.2.1", GatewayIP: "198.51.100.1"}}
	scope.Reservations = []DHCPReservation{{MAC: "02:00:00:00:00:01", IP: "198.51.100.50", Hostname: "printer"}}
	body := DHCPSetupRequest{Scopes: []DHCPSetupScope{scope}, ScopeRevisions: map[string]string{"lan": "0"}}
	h.do("PUT", dhcpSetupPath, "admin", body, "If-Match", setup.ETag).want(200).json(&setup)
	scope.LeaseDuration = "12h0m0s"
	if !reflect.DeepEqual(setup.Scopes, []DHCPSetupScope{scope}) || manager.State().Scopes[0].Management != "api" {
		t.Fatalf("scope fields changed in setup conversion: %+v", setup.Scopes)
	}
}

func TestDHCPSetupConcurrentWriters(t *testing.T) {
	h, manager, _ := newDHCPSetupHarness(t, config.DHCP{}, nil)
	setup := getDHCPSetup(t, h)
	firstScope, secondScope := testDHCPSetupScope(), testDHCPSetupScope()
	firstScope.DomainName, secondScope.DomainName = "first.home.arpa", "second.home.arpa"
	requests := []*http.Request{
		h.req("PUT", dhcpSetupPath, "admin", DHCPSetupRequest{Scopes: []DHCPSetupScope{firstScope}, ScopeRevisions: map[string]string{"lan": "0"}}, "If-Match", setup.ETag),
		h.req("PUT", dhcpSetupPath, "admin", DHCPSetupRequest{Scopes: []DHCPSetupScope{secondScope}, ScopeRevisions: map[string]string{"lan": "0"}}, "If-Match", setup.ETag),
	}
	codes := make(chan int, 2)
	for _, request := range requests {
		go func() { codes <- h.serve(request).Code }()
	}
	first, second := <-codes, <-codes
	if (first != 200 || second != 412) && (first != 412 || second != 200) {
		t.Fatalf("concurrent setup writes: %d %d", first, second)
	}
	if got := manager.State(); got.Revision != 2 || len(got.Scopes) != 1 {
		t.Fatalf("unexpected concurrent save state: %+v", got)
	}
}

func TestDHCPSetupLiveOverlayRevisionAndRestartApply(t *testing.T) {
	ctx := context.Background()
	h, manager, dir := newDHCPSetupHarness(t, config.DHCP{}, nil)
	setup := getDHCPSetup(t, h)
	body := DHCPSetupRequest{Enabled: true, Scopes: []DHCPSetupScope{testDHCPSetupScope()}, ScopeRevisions: map[string]string{"lan": "0"}}
	h.do("PUT", dhcpSetupPath, "admin", body, "If-Match", setup.ETag).want(200)
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	manager, err := dhcpsetup.Open(dir, config.DHCP{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	configs, err := manager.Effective().Configs()
	if err != nil {
		t.Fatal(err)
	}
	plan := manager.Startup()[0]
	dbPath := filepath.Join(dir, "dhcp-lan.db")
	store, err := dhcp.OpenLeasesForSetup(dbPath, configs[0], plan.Token, plan.ExpectedRevision)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	h = newHarness(t, func(o *Options) { o.DHCPSetup = manager; o.Config.DHCP = manager.Effective() })
	updateDomain := func(domain string) {
		t.Helper()
		state := store.Configuration()
		opts, err := dhcpOptions(state.Config).runtime()
		if err != nil {
			t.Fatal(err)
		}
		opts.DomainName = domain
		if err := store.UpdateOptions(ctx, state.Revision, opts); err != nil {
			t.Fatal(err)
		}
	}
	updateDomain("live.home.arpa")
	setup = getDHCPSetup(t, h)
	if len(setup.PendingScopes) != 0 || setup.Scopes[0].DomainName != "live.home.arpa" || setup.ScopeRevisions["lan"] != strconv.FormatUint(store.Configuration().Revision, 10) {
		t.Fatalf("setup did not overlay live DB settings: %+v", setup)
	}
	draft := DHCPSetupRequest{Enabled: true, Scopes: setup.Scopes, ScopeRevisions: setup.ScopeRevisions}
	draft.Scopes[0].Interface = "replacement0"
	draft.Scopes[0].DomainName = "pending.home.arpa"
	updateDomain("concurrent.home.arpa")
	r := h.do("PUT", dhcpSetupPath, "admin", draft, "If-Match", setup.ETag).want(412)
	if r.errKey() != KeyPrecondition || manager.State().Revision != 2 || store.Configuration().Config.DomainName != "concurrent.home.arpa" {
		t.Fatalf("stale live revision changed state: %s", r.Body.String())
	}
	refreshed := getDHCPSetup(t, h)
	draft.ScopeRevisions = refreshed.ScopeRevisions
	h.do("PUT", dhcpSetupPath, "admin", draft, "If-Match", refreshed.ETag).want(200).json(&setup)
	if !reflect.DeepEqual(setup.PendingScopes, []string{"lan"}) || setup.Scopes[0].DomainName != "pending.home.arpa" || !setup.RestartRequired {
		t.Fatalf("desired edit was not staged: %+v", setup)
	}
	updateDomain("newer.live.home.arpa")
	refreshed = getDHCPSetup(t, h)
	if refreshed.Scopes[0].DomainName != "pending.home.arpa" || !reflect.DeepEqual(refreshed.PendingScopes, []string{"lan"}) {
		t.Fatalf("pending desired options were replaced by live DB values: %+v", refreshed)
	}
	h.do("PUT", dhcpSetupPath, "admin", draft, "If-Match", setup.ETag).want(412)
	draft.ScopeRevisions = refreshed.ScopeRevisions
	h.do("PUT", dhcpSetupPath, "admin", draft, "If-Match", refreshed.ETag).want(200)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := dhcpsetup.Open(dir, config.DHCP{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	configs, err = restarted.Effective().Configs()
	if err != nil {
		t.Fatal(err)
	}
	plan = restarted.Startup()[0]
	applied, err := dhcp.OpenLeasesForSetup(dbPath, configs[0], plan.Token, plan.ExpectedRevision)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = applied.Close() })
	if got := applied.Configuration().Config; got.DomainName != "pending.home.arpa" || got.Interface != "replacement0" {
		t.Fatalf("restart ignored saved setup: %+v", got)
	}
	h = newHarness(t, func(o *Options) { o.DHCPSetup = restarted; o.Config.DHCP = restarted.Effective() })
	setup = getDHCPSetup(t, h)
	if len(setup.PendingScopes) != 0 || setup.RestartRequired {
		t.Fatalf("applied setup still reported pending: %+v", setup)
	}
}
