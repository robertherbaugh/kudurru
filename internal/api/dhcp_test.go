package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api/apitest"
	"github.com/robertherbaugh/kudurru/internal/dhcp"
)

func dhcpAPIHarness(t *testing.T, management string, mods ...func(*Options)) (*harness, *dhcp.LeaseStore) {
	t.Helper()
	cfg := dhcp.Config{Name: "default", Management: management, Interface: "eth0", ServerIP: netip.MustParseAddr("192.0.2.2"), Subnet: netip.MustParsePrefix("192.0.2.0/24"), RangeStart: netip.MustParseAddr("192.0.2.100"), RangeEnd: netip.MustParseAddr("192.0.2.110"), Router: netip.MustParseAddr("192.0.2.1"), DNSServers: []netip.Addr{netip.MustParseAddr("192.0.2.2")}, DomainName: "home.arpa", LeaseDuration: 12 * time.Hour, DNSTTL: 30 * time.Second}
	store, err := dhcp.OpenLeases(filepath.Join(t.TempDir(), "dhcp.db"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	service, err := dhcp.NewService([]dhcp.Config{cfg}, map[string]*dhcp.LeaseStore{"default": store}, nil)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close(); _ = store.Close() })
	setup := []func(*Options){func(o *Options) { o.DHCP = service; o.Config.DHCP.Enabled = true }}
	h := newHarness(t, append(setup, mods...)...)
	return h, store
}

func dhcpAPIConfig(t *testing.T, h *harness) DHCPConfig {
	t.Helper()
	var cfg DHCPConfig
	r := h.do(http.MethodGet, BasePath+dhcpBase+"/scopes/default/config", "admin", nil).want(http.StatusOK)
	r.json(&cfg)
	if cfg.ETag == "" || cfg.ETag != r.Header().Get("ETag") {
		t.Fatalf("ETag mismatch: %+v %s", cfg, r.Header().Get("ETag"))
	}
	return cfg
}

func dhcpAPIBind(t *testing.T, store *dhcp.LeaseStore, n int) dhcp.Lease {
	t.Helper()
	client := dhcp.Client{MAC: fmt.Sprintf("02:00:00:00:00:%02x", n), Hostname: fmt.Sprintf("client-%d", n)}
	offered, err := store.Offer(context.Background(), client, netip.Addr{})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := store.Acknowledge(context.Background(), client, offered.Address, dhcp.Selecting)
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func TestDHCPStatusDisabledAndPermissions(t *testing.T) {
	h := newHarness(t)
	var status DHCPStatus
	h.do("GET", BasePath+dhcpBase+"/status", "viewer", nil).want(200).json(&status)
	if status.Node != "node-a" || status.Enabled || status.Available || status.Scopes == nil || len(status.Scopes) != 0 {
		t.Fatalf("disabled status: %+v", status)
	}
	h.do("GET", BasePath+dhcpBase+"/scopes/default/config", "admin", nil).want(503)
	h, store := dhcpAPIHarness(t, "api")
	dhcpAPIBind(t, store, 1)
	root := BasePath + dhcpBase + "/scopes/default"
	for _, user := range []string{"viewer", "auditor", "operator", "admin"} {
		h.do("GET", root+"/config", user, nil).want(200)
		h.do("GET", root+"/reservations", user, nil).want(200)
		leaseCode := 200
		if user == "viewer" {
			leaseCode = 403
		}
		h.do("GET", root+"/leases", user, nil).want(leaseCode)
		h.do("GET", root+"/export", user, nil).want(leaseCode)
		if user != "admin" {
			h.do("PUT", root+"/config", user, dhcpAPIConfig(t, h).Options).want(403)
			h.do("PUT", root+"/reservations/02:00:00:00:00:02", user, map[string]any{"ip": "192.0.2.102"}).want(403)
			h.do("DELETE", root+"/reservations/02:00:00:00:00:02", user, nil).want(403)
			h.do("POST", root+"/import", user, DHCPImportRequest{Format: "dnsmasq", Data: ""}).want(403)
		}
	}
	h.do("GET", root+"/leases", "", nil).want(401)
	h.do("GET", BasePath+dhcpBase+"/scopes/missing/config", "admin", nil).want(404)
	h.do("DELETE", root+"/leases/192.0.2.100", "admin", nil).want(404)
	h.do("GET", BasePath+dhcpBase+"/status", "viewer", nil).want(200).json(&status)
	if !status.Enabled || !status.Available || len(status.Scopes) != 1 || status.Scopes[0].Name != "default" {
		t.Fatalf("active status: %+v", status)
	}
}

func TestDHCPOptionsRevisionAndLiveLeaseSafety(t *testing.T) {
	h, store := dhcpAPIHarness(t, "api")
	lease := dhcpAPIBind(t, store, 1)
	path := BasePath + dhcpBase + "/scopes/default/config"
	cfg := dhcpAPIConfig(t, h)
	h.do("PUT", path, "admin", cfg.Options).want(400)
	h.do("PUT", path, "admin", cfg.Options, "If-Match", "*").want(412)
	h.do("PUT", path, "admin", cfg.Options, "If-Match", dhcpETag("node-b", "default", store.Configuration().Revision)).want(412)
	opts := cfg.Options
	opts.DomainName = "new.home.arpa"
	opts.LeaseDuration = "1h"
	opts.RegisterDNS = true
	var changed DHCPConfig
	h.do("PUT", path, "admin", opts, "If-Match", cfg.ETag).want(200).json(&changed)
	if changed.Options.DomainName != opts.DomainName || changed.Revision == cfg.Revision || changed.ETag == cfg.ETag {
		t.Fatalf("options not applied: %+v", changed)
	}
	h.do("PUT", path, "admin", opts, "If-Match", cfg.ETag).want(412)
	opts.RangeStart = lease.Address.Next().String()
	r := h.do("PUT", path, "admin", opts, "If-Match", changed.ETag).want(409)
	if r.errKey() != KeyConflict {
		t.Fatalf("live conflict: %s", r.Body.String())
	}
	after := dhcpAPIConfig(t, h)
	if after.Revision != changed.Revision || after.Options.RangeStart != cfg.Options.RangeStart {
		t.Fatal("failed mutation changed configuration")
	}
	leases, err := store.List(context.Background())
	if err != nil || len(leases) != 1 || leases[0].ExpiresAt != lease.ExpiresAt {
		t.Fatalf("options lost live lease: %+v %v", leases, err)
	}
	opts = changed.Options
	opts.DNSTTL = "tomorrow"
	h.do("PUT", path, "admin", opts, "If-Match", changed.ETag).want(400)
	opts = changed.Options
	opts.DNSTTL = "0s"
	h.do("PUT", path, "admin", opts, "If-Match", changed.ETag).want(400)
	opts.DNSTTL = "1.5s"
	h.do("PUT", path, "admin", opts, "If-Match", changed.ETag).want(400)
	raw, _ := json.Marshal(changed.Options)
	var unknown map[string]any
	_ = json.Unmarshal(raw, &unknown)
	unknown["subnet"] = "198.51.100.0/24"
	h.do("PUT", path, "admin", unknown, "If-Match", changed.ETag).want(400)
	var successful, failed bool
	for _, ev := range h.audit.Events() {
		if ev.Action == "dhcp.write" && ev.Node == "node-a" && ev.Actor.Name == "admin" {
			successful = successful || ev.Result == "success"
			failed = failed || ev.Result == "error"
			if ev.Result == "error" && string(ev.Before) != string(ev.After) {
				t.Fatalf("failed mutation audit changed configuration: before %s after %s", ev.Before, ev.After)
			}
		}
	}
	if !successful || !failed {
		t.Fatal("DHCP management mutations were not audited")
	}
}

func TestDHCPReservationSafetyOwnershipAndLocks(t *testing.T) {
	h, store := dhcpAPIHarness(t, "api")
	lease := dhcpAPIBind(t, store, 1)
	root := BasePath + dhcpBase + "/scopes/default"
	path := root + "/reservations/02-00-00-00-00-02"
	cfg := dhcpAPIConfig(t, h)
	h.do("PUT", path, "admin", map[string]any{"ip": lease.Address.String()}, "If-Match", cfg.ETag).want(409)
	var changed DHCPConfig
	h.do("PUT", path, "admin", map[string]any{"ip": "192.0.2.102", "hostname": "printer"}, "If-Match", cfg.ETag).want(200).json(&changed)
	if len(changed.Reservations) != 1 || changed.Reservations[0].MAC != "02:00:00:00:00:02" {
		t.Fatalf("reservation not canonical: %+v", changed)
	}
	h.do("DELETE", path, "admin", nil, "If-Match", cfg.ETag).want(412)
	h.do("DELETE", path, "admin", nil, "If-Match", changed.ETag).want(200).json(&changed)
	if len(changed.Reservations) != 0 {
		t.Fatal("reservation not removed")
	}
	h.do("PUT", root+"/reservations/not-a-mac", "admin", map[string]any{"ip": "192.0.2.102"}, "If-Match", changed.ETag).want(400)
	h.srv.cfg.EnvLocked = []string{"dhcp.leaseDuration", "dhcp.reservations"}
	cfg = dhcpAPIConfig(t, h)
	if len(cfg.EnvLocked) != 2 {
		t.Fatalf("environment locks: %+v", cfg.EnvLocked)
	}
	options := cfg.Options
	options.LeaseDuration = "1h"
	r := h.do("PUT", root+"/config", "admin", options, "If-Match", cfg.ETag).want(409)
	if r.errKey() != KeyEnvLocked {
		t.Fatal(r.Body.String())
	}
	h.do("PUT", path, "admin", map[string]any{"ip": "192.0.2.102"}, "If-Match", cfg.ETag).want(409)
	options = cfg.Options
	options.DomainName = "changed.home.arpa"
	h.do("PUT", root+"/config", "admin", options, "If-Match", cfg.ETag).want(200)
	h, _ = dhcpAPIHarness(t, "file")
	cfg = dhcpAPIConfig(t, h)
	r = h.do("PUT", root+"/config", "admin", cfg.Options, "If-Match", cfg.ETag).want(409)
	if r.errKey() != KeyManagedBy {
		t.Fatal(r.Body.String())
	}
	h.do("PUT", path, "admin", map[string]any{"ip": "192.0.2.102"}, "If-Match", cfg.ETag).want(409)
	h.do("DELETE", path, "admin", nil, "If-Match", cfg.ETag).want(409)
}

func TestDHCPLeasePaginationAndImportExport(t *testing.T) {
	h, store := dhcpAPIHarness(t, "api")
	first := dhcpAPIBind(t, store, 1)
	dhcpAPIBind(t, store, 2)
	root := BasePath + dhcpBase + "/scopes/default"
	var page DHCPLeases
	h.do("GET", root+"/leases?limit=1", "auditor", nil).want(200).json(&page)
	if page.Node != "node-a" || len(page.Items) != 1 || page.Items[0].Address != first.Address.String() || page.Continue == "" {
		t.Fatalf("first page: %+v", page)
	}
	cursor := page.Continue
	page = DHCPLeases{}
	h.do("GET", root+"/leases?limit=1&continue="+cursor, "operator", nil).want(200).json(&page)
	if len(page.Items) != 1 || page.Items[0].Address == first.Address.String() || page.Continue != "" {
		t.Fatalf("next page: %+v", page)
	}
	h.do("GET", root+"/leases?state=offered&continue="+cursor, "admin", nil).want(400)
	h.do("GET", root+"/leases?continue=invalid!", "admin", nil).want(400)
	h.do("GET", root+"/leases?q=client-2", "admin", nil).want(200).json(&page)
	if len(page.Items) != 1 || page.Items[0].Hostname != "client-2" {
		t.Fatalf("lease filter: %+v", page)
	}
	var exported DHCPExport
	h.do("GET", root+"/export", "auditor", nil).want(200).json(&exported)
	if len(exported.Document.Leases) != 2 || exported.Document.Version != 1 {
		t.Fatalf("export: %+v", exported)
	}
	raw, _ := json.Marshal(exported.Document)
	var result DHCPImportResult
	h.do("POST", root+"/import", "admin", DHCPImportRequest{Format: "native", Data: string(raw)}).want(200).json(&result)
	if result.Unchanged != 2 || result.Imported != 0 {
		t.Fatalf("idempotent import: %+v", result)
	}
	expiry := time.Now().Add(time.Hour).Unix()
	// A valid first row must roll back when the second conflicts with a live owner.
	data := fmt.Sprintf("%d 02:00:00:00:00:03 192.0.2.105 imported *\n%d 02:00:00:00:00:04 %s conflicting *\n", expiry, expiry, first.Address)
	r := h.do("POST", root+"/import", "admin", DHCPImportRequest{Format: "dnsmasq", Data: data})
	if r.Code != 400 && r.Code != 409 {
		t.Fatalf("conflicting import: %d %s", r.Code, r.Body.String())
	}
	leases, err := store.List(context.Background())
	if err != nil || len(leases) != 2 {
		t.Fatalf("import was not atomic: %+v %v", leases, err)
	}
	h.do("POST", root+"/import", "admin", DHCPImportRequest{Format: "native", Data: "{}"}).want(400)
	h.do("POST", root+"/import", "admin", DHCPImportRequest{Format: "dnsmasq", Data: strings.Repeat("x", dhcp.MaxImportBytes+1)}).want(413)
	// File ownership restricts configuration edits, not runtime lease import.
	h, _ = dhcpAPIHarness(t, "file")
	data = fmt.Sprintf("%d 02:00:00:00:00:03 192.0.2.105 imported *\n", expiry)
	h.do("POST", root+"/import", "admin", DHCPImportRequest{Format: "dnsmasq", Data: data}).want(200).json(&result)
	if result.Imported != 1 {
		t.Fatalf("file-owned runtime import: %+v", result)
	}
}

func TestDHCPCookieMutationRequiresCSRF(t *testing.T) {
	h, _ := dhcpAPIHarness(t, "api")
	cfg := dhcpAPIConfig(t, h)
	path := BasePath + dhcpBase + "/scopes/default/config"
	request := h.req("PUT", path, "", cfg.Options, "If-Match", cfg.ETag)
	request.AddCookie(&http.Cookie{Name: apitest.CookieName, Value: "admin", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	h.serve(request).want(403)
	request = h.req("PUT", path, "", cfg.Options, "If-Match", cfg.ETag, apitest.HeaderCSRF, apitest.CSRFToken)
	request.AddCookie(&http.Cookie{Name: apitest.CookieName, Value: "admin", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	h.serve(request).want(200)
}

func TestDHCPManagementHonorsLicenseGate(t *testing.T) {
	h, _ := dhcpAPIHarness(t, "api", func(o *Options) { o.License = licenseManager(t, 0); o.Membership = peers(2) })
	cfg := dhcpAPIConfig(t, h)
	root := BasePath + dhcpBase + "/scopes/default"
	h.do("PUT", root+"/config", "admin", cfg.Options, "If-Match", cfg.ETag).want(402)
	h.do("POST", root+"/import", "admin", DHCPImportRequest{Format: "native", Data: "{}"}).want(402)
	h.do("GET", root+"/config", "viewer", nil).want(200)
	h.do("GET", root+"/leases", "auditor", nil).want(200)
}

func TestDHCPConcurrentWritersUseOneRevision(t *testing.T) {
	h, store := dhcpAPIHarness(t, "api")
	cfg := dhcpAPIConfig(t, h)
	before := store.Configuration().Revision
	a, b := cfg.Options, cfg.Options
	a.DomainName = "first.home.arpa"
	b.DomainName = "second.home.arpa"
	path := BasePath + dhcpBase + "/scopes/default/config"
	requests := []*http.Request{h.req("PUT", path, "admin", a, "If-Match", cfg.ETag), h.req("PUT", path, "admin", b, "If-Match", cfg.ETag)}
	codes := make(chan int, 2)
	for _, request := range requests {
		go func() { codes <- h.serve(request).Code }()
	}
	first, second := <-codes, <-codes
	if (first != 200 || second != 412) && (first != 412 || second != 200) {
		t.Fatalf("concurrent writes: %d %d", first, second)
	}
	if state := store.Configuration(); state.Revision != before+1 {
		t.Fatalf("unexpected revision: %+v", state)
	}
}
