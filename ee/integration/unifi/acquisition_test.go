package unifi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/inventory"
)

func TestOfficialFreshConnectedRowsFailureExpiryAndOmission(t *testing.T) {
	s, f := controller(t)
	h := fixtureLoan()
	p := provisionFixture(t, s, public.UniFiIntegration{}, h)
	now := translationFixture().Observation.ObservedAt
	p.now = func() time.Time { return now }
	first, err := p.Observe(context.Background())
	if err != nil || len(first.Clients) != 1 {
		t.Fatal(first, err)
	}
	original := first.Clients[0]
	ip := original.IPs[0]
	if !original.ObservedAt.Equal(now) || !original.ExpiresAt.Equal(now.Add(3*time.Minute)) {
		t.Fatal("connectedAt mistaken for assignment observation", original)
	}
	holder := inventory.New(func() time.Time { return now })
	if err := holder.Replace(original.Source, 1, first.Clients); err != nil {
		t.Fatal(err)
	}
	// ConnectedAt remains unchanged across two authentic complete polls.
	now = now.Add(time.Minute)
	second, err := p.Observe(context.Background())
	if err != nil || !second.Clients[0].ObservedAt.Equal(now) || !second.Clients[0].ExpiresAt.Equal(now.Add(3*time.Minute)) {
		t.Fatal(second, err)
	}
	if err := holder.Replace(original.Source, 2, second.Clients); err != nil {
		t.Fatal(err)
	}
	snapshot := holder.Snapshot()
	expires := second.Clients[0].ExpiresAt
	f.mu.Lock()
	f.hook = func(w http.ResponseWriter, _ *http.Request, _ int) bool { w.WriteHeader(503); return true }
	f.mu.Unlock()
	now = expires.Add(-time.Nanosecond)
	if _, err := p.Observe(context.Background()); err == nil {
		t.Fatal("failed poll reported success")
	}
	if holder.Snapshot() != snapshot || snapshot.Lookup(ip, now) == nil || snapshot.Lookup(ip, expires) != nil {
		t.Fatal("failure renewed absolute assignment expiry")
	}
	now = expires
	f.mu.Lock()
	f.hook = nil
	f.clients = nil
	f.mu.Unlock()
	omitted, err := p.Observe(context.Background())
	if err != nil || len(omitted.Clients) != 1 || len(omitted.Clients[0].IPs) != 0 || !omitted.Clients[0].ObservedAt.Equal(second.Clients[0].ObservedAt) || !omitted.Clients[0].ExpiresAt.Equal(expires) {
		t.Fatal("omission retained/renewed an address", omitted, err)
	}
	if err := holder.Replace(original.Source, 3, omitted.Clients); err != nil || holder.Snapshot().Lookup(ip, now) != nil {
		t.Fatal(err)
	}
	restarted := provisionFixture(t, s, public.UniFiIntegration{}, fixtureLoan())
	restarted.now = p.now
	empty, err := restarted.Observe(context.Background())
	if err != nil || len(empty.Clients) != 0 {
		t.Fatal("restart invented inventory", empty, err)
	}
}

func TestOfficialMACAndIPMovesReplaceStableIdentity(t *testing.T) {
	s, f := controller(t)
	p := provisionFixture(t, s, public.UniFiIntegration{}, fixtureLoan())
	now := translationFixture().Observation.ObservedAt
	p.now = func() time.Time { return now }
	first, err := p.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	f.mu.Lock()
	f.clients[0].IPAddress = "192.0.2.4"
	f.mu.Unlock()
	moved, err := p.Observe(context.Background())
	if err != nil || moved.Clients[0].ID != first.Clients[0].ID || slices.Equal(moved.Clients[0].IPs, first.Clients[0].IPs) {
		t.Fatal(moved, err)
	}
	now = now.Add(time.Minute)
	f.mu.Lock()
	f.clients[0].MACAddress = "02:00:00:00:00:02"
	f.mu.Unlock()
	swapped, err := p.Observe(context.Background())
	if err != nil || len(swapped.Clients) != 2 {
		t.Fatal(swapped, err)
	}
	holder := inventory.New(func() time.Time { return now })
	if err := holder.Replace(first.Clients[0].Source, 1, swapped.Clients); err != nil {
		t.Fatal(err)
	}
	got := holder.Snapshot().Lookup(netip.MustParseAddr("192.0.2.4"), now)
	if got == nil || slices.Contains(got.MACs, "02:00:00:00:00:01") || holder.Snapshot().Lookup(netip.MustParseAddr("192.0.2.3"), now) != nil {
		t.Fatal("old identity inherited reassigned IP", got)
	}
}

func TestOfficialAcquisitionRejectsPartialMalformedOrDriftingFacts(t *testing.T) {
	modes := []string{"clients_drift", "networks_drift", "site_drift", "version_drift", "duplicate_uuid", "duplicate_mac", "duplicate_json", "missing_pagination", "empty_progress", "count_mismatch", "too_many", "invalid_type", "invalid_mac", "future_connected_at", "invalid_ip", "network_family", "network_identity", "unknown_version", "bad_version", "wrong_site"}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			s, f := controller(t)
			now := translationFixture().Observation.ObservedAt
			f.hook = func(w http.ResponseWriter, r *http.Request, n int) bool {
				if strings.HasSuffix(r.URL.Path, "/clients") {
					switch mode {
					case "clients_drift":
						rows := slices.Clone(f.clients)
						if n == 2 {
							rows[0].IPAddress = "192.0.2.4"
						}
						fixturePage(w, r, rows)
						return true
					case "duplicate_uuid":
						fixturePage(w, r, append(slices.Clone(f.clients), f.clients[0]))
						return true
					case "duplicate_mac":
						rows := append(slices.Clone(f.clients), f.clients[0])
						rows[1].ID = "44444444-4444-4444-8444-444444444444"
						fixturePage(w, r, rows)
						return true
					case "duplicate_json":
						_, _ = io.WriteString(w, `{"offset":0,"offset":0,"limit":200,"count":0,"totalCount":0,"data":[]}`)
						return true
					case "missing_pagination":
						_, _ = io.WriteString(w, `{"data":[]}`)
						return true
					case "empty_progress":
						_, _ = io.WriteString(w, `{"offset":0,"limit":200,"count":0,"totalCount":1,"data":[]}`)
						return true
					case "count_mismatch":
						_, _ = io.WriteString(w, `{"offset":0,"limit":200,"count":1,"totalCount":1,"data":[]}`)
						return true
					case "too_many":
						_, _ = io.WriteString(w, `{"offset":0,"limit":200,"count":0,"totalCount":10001,"data":[]}`)
						return true
					case "invalid_type", "invalid_mac", "future_connected_at", "invalid_ip":
						rows := slices.Clone(f.clients)
						switch mode {
						case "invalid_type":
							rows[0].Type = "UNKNOWN"
						case "invalid_mac":
							rows[0].MACAddress = "ff:ff:ff:ff:ff:ff"
						case "invalid_ip":
							rows[0].IPAddress = "fe80::1%eth0"
						case "future_connected_at":
							at := now.Add(time.Hour)
							rows[0].ConnectedAt = &at
						}
						fixturePage(w, r, rows)
						return true
					}
				}
				if strings.HasSuffix(r.URL.Path, "/networks") && mode == "networks_drift" && n == 2 {
					fixturePage(w, r, []map[string]any{})
					return true
				}
				if strings.HasSuffix(r.URL.Path, "/sites") && mode == "site_drift" && n == 2 {
					fixturePage(w, r, []map[string]string{{"id": siteID, "internalReference": "other"}})
					return true
				}
				if strings.HasSuffix(r.URL.Path, "/info") {
					version := f.version
					switch mode {
					case "version_drift":
						if n == 2 {
							version = "10.6.107"
						}
					case "unknown_version":
						version = "8.9.1"
					case "bad_version":
						version = "10.6.\n106"
					}
					_ = json.NewEncoder(w).Encode(map[string]string{"applicationVersion": version})
					return true
				}
				if strings.HasSuffix(r.URL.Path, "/networks/"+networkID) && (mode == "network_family" || mode == "network_identity") {
					id, ip := networkID, "192.0.2.1"
					if mode == "network_family" {
						ip = "2001:db8::1"
					}
					if mode == "network_identity" {
						id = clientID
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "name": "LAN", "management": "GATEWAY", "enabled": true, "vlanId": 10, "ipv4Configuration": map[string]any{"hostIpAddress": ip, "prefixLength": 24}})
					return true
				}
				return false
			}
			config := public.UniFiIntegration{}
			if mode == "wrong_site" {
				config.Site = "Display name"
			}
			p := provisionFixture(t, s, config, fixtureLoan())
			p.now = func() time.Time { return now }
			snapshot, err := p.Observe(context.Background())
			if err == nil || snapshot.Complete || len(p.previous) != 0 {
				t.Fatal("partial input replaced observation", snapshot, err)
			}
		})
	}
}

func TestOfficialCompletePagingAndPaginationBounds(t *testing.T) {
	s, f := controller(t)
	clients := []clientRow{}
	for i := 0; i < 201; i++ {
		clients = append(clients, clientRow{ID: fmt.Sprintf("%08x-3333-4333-8333-333333333333", i+1), Name: fmt.Sprintf("VPN%d", i), Type: "VPN", IPAddress: fmt.Sprintf("198.51.100.%d", i+1)})
	}
	f.clients = clients
	p := provisionFixture(t, s, public.UniFiIntegration{}, fixtureLoan())
	p.now = func() time.Time { return translationFixture().Observation.ObservedAt }
	snapshot, err := p.Observe(context.Background())
	if err != nil || len(snapshot.Clients) != 201 {
		t.Fatal("paging truncated inventory", snapshot, err)
	}
	f.mu.Lock()
	calls := f.calls["/proxy/network/integration/v1/sites/"+siteID+"/clients"]
	f.mu.Unlock()
	if calls != 4 {
		t.Fatal("did not acquire two full assignment images", calls)
	}
	if _, _, err := p.identity(context.Background(), &pollBudget{pages: maxPages}); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
}

func TestLegacyOptInSupplementCannotRenewHistoryOrReservations(t *testing.T) {
	for _, mode := range []string{"disabled", "404", "metadata", "historical", "fixed_ip", "wrong_ip", "leases", "auth_failure", "bad_envelope", "unsupported_version"} {
		t.Run(mode, func(t *testing.T) {
			s, f := controller(t)
			now := translationFixture().Observation.ObservedAt
			f.hook = func(w http.ResponseWriter, r *http.Request, _ int) bool {
				if !strings.HasPrefix(r.URL.Path, "/proxy/network/api/") && !strings.Contains(r.URL.Path, "/active-leases") {
					return false
				}
				switch mode {
				case "404":
					w.WriteHeader(404)
					return true
				case "auth_failure":
					w.WriteHeader(403)
					return true
				case "bad_envelope":
					_, _ = io.WriteString(w, `{"meta":{"rc":"error"},"data":[]}`)
					return true
				}
				rows := []map[string]any{{"mac": "02:00:00:00:00:01", "ip": "192.0.2.3", "name": "Custom alias", "hostname": "laptop.lan.example", "note": "group=kids", "last_seen": 1, "fixed_ip": "203.0.113.99", "use_fixedip": true, "network_id": "legacy-mongo-id"}}
				if mode == "historical" {
					rows = append(rows, map[string]any{"mac": "02:00:00:00:00:99", "ip": "192.0.2.99", "last_seen": 0, "fixed_ip": "192.0.2.99"})
				}
				if mode == "wrong_ip" {
					rows[0]["ip"] = "192.0.2.4"
				}
				if mode == "leases" {
					rows[0]["hostname"] = "lease-host.lan.example"
					if !strings.Contains(r.URL.Path, "/active-leases") {
						rows = []map[string]any{}
					}
				}
				if strings.Contains(r.URL.Path, "/active-leases") {
					_ = json.NewEncoder(w).Encode(map[string]any{"dhcp_lease_info": rows})
				} else {
					_ = json.NewEncoder(w).Encode(map[string]any{"meta": map[string]string{"rc": "ok"}, "data": rows})
				}
				return true
			}
			if mode == "unsupported_version" {
				f.version = "11.0.1"
			}
			p := provisionFixture(t, s, public.UniFiIntegration{Legacy: mode != "disabled"}, fixtureLoan())
			p.now = func() time.Time { return now }
			snapshot, err := p.Observe(context.Background())
			if mode == "auth_failure" || mode == "bad_envelope" || mode == "unsupported_version" {
				if err == nil || snapshot.Complete {
					t.Fatal(snapshot, err)
				}
				return
			}
			if err != nil || len(snapshot.Clients) != 1 {
				t.Fatal(snapshot, err)
			}
			c := snapshot.Clients[0]
			if !c.ObservedAt.Equal(now) || !c.ExpiresAt.Equal(now.Add(3*time.Minute)) || len(c.IPs) != 1 || c.IPs[0].String() != "192.0.2.3" {
				t.Fatal("legacy fabricated assignment evidence", c)
			}
			if mode == "disabled" || mode == "404" || mode == "wrong_ip" {
				if c.Name != "Laptop" || c.Hostname != "" {
					t.Fatal("unexpected enrichment", c)
				}
			} else if mode == "leases" {
				if c.Hostname != "lease-host.lan.example" {
					t.Fatal(c)
				}
			} else if c.Name != "Custom alias" {
				t.Fatal("legacy metadata not joined", c)
			}
			diag := p.Diagnostics()
			if mode == "404" && len(diag.LegacyUnavailable) != 3 || mode == "historical" && diag.HistoricalWithoutTime != 3 {
				t.Fatal(diag)
			}
			if mode == "disabled" {
				f.mu.Lock()
				defer f.mu.Unlock()
				for path := range f.calls {
					if strings.Contains(path, "/api/") {
						t.Fatal("legacy endpoint contacted while disabled", path)
					}
				}
			}
		})
	}
}

func TestLegacySavedMetadataPrecedenceAndNetworkConflict(t *testing.T) {
	for _, mode := range []string{"precedence", "network_conflict"} {
		t.Run(mode, func(t *testing.T) {
			s, f := controller(t)
			otherID := "44444444-4444-4444-8444-444444444444"
			f.hook = func(w http.ResponseWriter, r *http.Request, _ int) bool {
				if mode == "network_conflict" && strings.HasSuffix(r.URL.Path, "/networks") {
					fixturePage(w, r, []map[string]any{{"id": networkID, "name": "LAN", "vlanId": 10, "enabled": true, "management": "GATEWAY"}, {"id": otherID, "name": "Other", "vlanId": 11, "enabled": true, "management": "GATEWAY"}})
					return true
				}
				if strings.HasSuffix(r.URL.Path, "/networks/"+otherID) {
					_ = json.NewEncoder(w).Encode(map[string]any{"id": otherID, "name": "Other", "vlanId": 11, "enabled": true, "management": "GATEWAY", "ipv4Configuration": map[string]any{"hostIpAddress": "192.0.2.2", "prefixLength": 24}})
					return true
				}
				if !strings.Contains(r.URL.Path, "/api/") {
					return false
				}
				name, netID := "saved-custom", networkID
				if strings.Contains(r.URL.Path, "/stat/sta") {
					name = "station-name"
					if mode == "network_conflict" {
						netID = otherID
					}
				}
				rows := []map[string]any{{"mac": "02:00:00:00:00:01", "ip": "192.0.2.3", "name": name, "hostname": name + ".lan.example", "network_id": netID}}
				if strings.Contains(r.URL.Path, "/active-leases") {
					_ = json.NewEncoder(w).Encode(map[string]any{"dhcp_lease_info": rows})
				} else {
					_ = json.NewEncoder(w).Encode(map[string]any{"meta": map[string]string{"rc": "ok"}, "data": rows})
				}
				return true
			}
			p := provisionFixture(t, s, public.UniFiIntegration{Legacy: true}, fixtureLoan())
			p.now = func() time.Time { return translationFixture().Observation.ObservedAt }
			snapshot, err := p.Observe(context.Background())
			if mode == "network_conflict" {
				if !errors.Is(err, ErrDrift) || snapshot.Complete {
					t.Fatal("conflicting enrichment selected by endpoint order", snapshot, err)
				}
				return
			}
			if err != nil || snapshot.Clients[0].Name != "saved-custom" || snapshot.Clients[0].Hostname != "saved-custom.lan.example" {
				t.Fatal("station overwrote saved alias", snapshot, err)
			}
		})
	}
}
