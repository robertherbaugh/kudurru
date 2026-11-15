package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/inventory"
)

func inventoryDeclaration() declaration {
	d := actionDeclaration()
	d.Capabilities = []string{"clients"}
	d.Manifest.Capabilities = []integration.Capability{integration.CapClientSource}
	return d
}

func inventoryWire(at time.Time) (integration.Client, integration.Network) {
	n := integration.Network{Name: "lan", VLAN: 10, CIDR: "192.0.2.0/24", Domain: "example.test"}
	c := integration.Client{ID: "device-1", IPs: []string{"192.0.2.7"}, MACs: []string{"02:00:00:00:00:01"}, Name: "Device", Hostname: "device.example.test", Network: n,
		FixedIP: "192.0.2.99", Tags: []string{"workstation"}, LastSeen: at}
	return c, n
}

func TestInventoryActualTLSCompletePollNeverRenewsLastSeenOrGrantsFixedIP(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	c, n := inventoryWire(at)
	var clientsCalls, networkCalls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", integrationMedia)
		switch r.URL.Path {
		case "/provider/":
			_ = json.NewEncoder(w).Encode(inventoryDeclaration())
		case "/provider/clients":
			clientsCalls.Add(1)
			_ = json.NewEncoder(w).Encode([]integration.Client{c})
		case "/provider/networks":
			networkCalls.Add(1)
			_ = json.NewEncoder(w).Encode([]integration.Network{n})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	p, _ := lifecycleProvider(t, server, &lifecycleHost{}, nil)
	now := at
	p.now = func() time.Time { return now }
	if _, err := p.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	first, err := p.Observe(t.Context())
	if err != nil || !first.Complete || first.Desired != nil || len(first.Clients) != 1 {
		t.Fatalf("first complete observation: %v", err)
	}
	original := first.Clients[0]
	if original.Source != host.SourceID(p.owner) || !original.ObservedAt.Equal(at) || !original.ExpiresAt.Equal(at.Add(inventoryLifetime)) || len(original.IPs) != 1 || original.IPs[0].String() != "192.0.2.7" {
		t.Fatal("poll manufactured identity, time or desired address authority")
	}
	first.Clients[0].IPs[0] = netip.MustParseAddr("192.0.2.100")
	now = at.Add(4 * time.Minute)
	second, err := p.Observe(t.Context())
	if err != nil || !second.Complete || len(second.Clients) != 1 {
		t.Fatalf("repeat old observation: %v", err)
	}
	if !second.Clients[0].ObservedAt.Equal(at) || !second.Clients[0].ExpiresAt.Equal(original.ExpiresAt) || second.Clients[0].IPs[0].String() != "192.0.2.7" {
		t.Fatal("repeat renewed old device or aliased previous return")
	}
	holder := inventory.New(func() time.Time { return now })
	if err := holder.Replace(host.SourceID(p.owner), 1, second.Clients); err != nil {
		t.Fatal(err)
	}
	if holder.Snapshot().Lookup(netip.MustParseAddr("192.0.2.7"), now) != nil || holder.Snapshot().Lookup(netip.MustParseAddr("192.0.2.99"), at) != nil {
		t.Fatal("stale or desired-only address gained live identity")
	}
	if clientsCalls.Load() != 2 || networkCalls.Load() != 2 {
		t.Fatal("polls did not use exactly two complete-list reads each")
	}
}

func TestInventoryActualTLSIncompleteOrAmbiguousWholeCandidateRefuses(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	base, network := inventoryWire(at)
	for _, mode := range []string{"null-client", "null-network", "null-array", "duplicate-id", "duplicate-network", "network-mismatch", "future", "missing-time", "ancient", "bad-ip", "multicast-mac", "credential-echo", "pagination", "partial-status", "network-failure"} {
		t.Run(mode, func(t *testing.T) {
			c, n := base, network
			clients := []*integration.Client{&c}
			networks := []*integration.Network{&n}
			switch mode {
			case "null-client":
				clients = []*integration.Client{nil}
			case "null-network":
				networks = []*integration.Network{nil}
			case "null-array":
				clients = nil
			case "duplicate-id":
				clients = append(clients, &c)
			case "duplicate-network":
				networks = append(networks, &n)
			case "network-mismatch":
				n.VLAN++
			case "future":
				c.LastSeen = at.Add(time.Second)
			case "missing-time":
				c.LastSeen = time.Time{}
			case "ancient":
				c.LastSeen = at.Add(-25 * time.Hour)
			case "bad-ip":
				c.IPs = []string{"not-an-address"}
			case "multicast-mac":
				c.MACs = []string{"01:00:00:00:00:01"}
			case "credential-echo":
				c.Attributes = map[string]string{"note": "explicit-test-token"}
			}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", integrationMedia)
				switch r.URL.Path {
				case "/provider/":
					_ = json.NewEncoder(w).Encode(inventoryDeclaration())
				case "/provider/clients":
					if mode == "pagination" {
						w.Header().Set("Link", "</clients?page=2>; rel=next")
					}
					if mode == "partial-status" {
						w.WriteHeader(http.StatusPartialContent)
					}
					_ = json.NewEncoder(w).Encode(clients)
				case "/provider/networks":
					if mode == "network-failure" {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					_ = json.NewEncoder(w).Encode(networks)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			p, _ := lifecycleProvider(t, server, &lifecycleHost{}, nil)
			p.now = func() time.Time { return at }
			if _, err := p.Check(t.Context()); err != nil {
				t.Fatal(err)
			}
			out, err := p.Observe(t.Context())
			if err == nil || out.Complete || len(out.Clients) != 0 || out.Desired != nil {
				t.Fatal("partial or invalid peer produced a usable source candidate")
			}
		})
	}
}

func TestInventoryOriginalAuthorityCanceledBetweenListsDoesNotFetchSuccessor(t *testing.T) {
	at := time.Now().UTC()
	c, _ := inventoryWire(at)
	var calls, charges atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", integrationMedia)
		if r.URL.Path == "/provider/" {
			_ = json.NewEncoder(w).Encode(inventoryDeclaration())
			return
		}
		_ = json.NewEncoder(w).Encode([]integration.Client{c})
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h := &lifecycleHost{}
	h.before = func(context.Context) error {
		if charges.Add(1) == 3 {
			cancel()
		}
		return nil
	}
	p, _ := lifecycleProvider(t, server, h, nil)
	if _, err := p.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	out, err := p.Observe(ctx)
	if !errors.Is(err, context.Canceled) || out.Complete || calls.Load() != 2 || charges.Load() != 3 {
		t.Fatal("second list escaped original canceled authority")
	}
}

func TestInventoryEmptyCompleteListsPermitRemovalWithoutInventedDevices(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", integrationMedia)
		if r.URL.Path == "/provider/" {
			_ = json.NewEncoder(w).Encode(inventoryDeclaration())
			return
		}
		_, _ = w.Write([]byte("[]"))
	}))
	defer server.Close()
	p, _ := lifecycleProvider(t, server, &lifecycleHost{}, nil)
	if _, err := p.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	out, err := p.Observe(t.Context())
	if err != nil || !out.Complete || out.Clients == nil || len(out.Clients) != 0 || out.Desired != nil {
		t.Fatalf("empty complete observation: %v", err)
	}
}
