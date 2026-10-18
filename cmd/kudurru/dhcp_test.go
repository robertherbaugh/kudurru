package main

import (
	"context"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dhcp"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

func dhcpTestConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "kudurru.yaml")
	const settings = `dhcp:
  enabled: true
  interface: kdr-dhcp-test
  serverIP: 192.0.2.2
  subnet: 192.0.2.0/24
  rangeStart: 192.0.2.100
  rangeEnd: 192.0.2.110
  router: 192.0.2.1
  dnsServers: [192.0.2.2]
  domainName: home.arpa
  leaseDuration: 12h
`
	if err := os.WriteFile(path, []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Node.DataDir = dir
	return cfg
}

func dhcpNamedConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := dhcpTestConfig(t)
	cfg.DHCP = config.DHCP{Enabled: true, Scopes: []config.DHCPScope{
		{Name: "lan", Management: "api", Interface: "kdr-dhcp-test", ServerIP: "192.0.2.2", Subnet: "192.0.2.0/24",
			RangeStart: "192.0.2.100", RangeEnd: "192.0.2.110", Router: "192.0.2.1", DNSServers: []string{"192.0.2.2"},
			RegisterDNS: true, DomainName: "lan.home.arpa"},
		{Name: "guest", Interface: "kdr-dhcp-other", ServerIP: "198.51.100.2", Subnet: "198.51.100.0/24",
			RangeStart: "198.51.100.100", RangeEnd: "198.51.100.110", Router: "198.51.100.1", DNSServers: []string{"198.51.100.2"}},
	}}
	return cfg
}

func TestDHCPRuntimeNamedScopesPersistManagementAndPublishDNS(t *testing.T) {
	cfg := dhcpNamedConfig(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service, cleanup, err := newDHCP(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	if !slices.Equal(service.ScopeNames(), []string{"guest", "lan"}) {
		t.Fatalf("scope names: %v", service.ScopeNames())
	}
	ctx := context.Background()
	store, err := service.Scope("lan")
	if err != nil {
		t.Fatal(err)
	}
	reservation := dhcp.Reservation{MAC: "02:00:00:00:00:10", IP: netip.MustParseAddr("192.0.2.10"), Hostname: "printer"}
	if err := service.MutateScope(ctx, "lan", func(ctx context.Context, s *dhcp.LeaseStore) error {
		return s.PutReservation(ctx, s.Configuration().Revision, reservation)
	}); err != nil {
		t.Fatal(err)
	}
	client := dhcp.Client{MAC: reservation.MAC, Hostname: "ignored"}
	offer, err := store.Offer(ctx, client, netip.Addr{})
	if err != nil {
		t.Fatal(err)
	}
	bound, err := store.Acknowledge(ctx, client, offer.Address, dhcp.Selecting)
	if err != nil {
		t.Fatal(err)
	}
	resolver := dhcpLocalRecords(service)
	msg := new(dns.Msg)
	msg.SetQuestion("printer.lan.home.arpa.", dns.TypeA)
	answer, ok := resolver.ResolveLocal(&pipeline.Request{Msg: msg})
	if !ok || len(answer.Msg.Answer) != 1 || !strings.Contains(answer.Msg.Answer[0].String(), reservation.IP.String()) {
		t.Fatalf("runtime resolver did not see binding: %+v", answer)
	}
	if names := resolver.Hostnames(bound.Address); !slices.Equal(names, []string{"printer.lan.home.arpa"}) {
		t.Fatalf("policy hostname wiring: %v", names)
	}
	if second, closeSecond, err := newDHCP(cfg, logger); err == nil {
		closeSecond()
		t.Fatalf("second service owns scopes: %v", second)
	}
	cleanup()
	// File reservations are seeds in API mode. Restart must retain the live edit.
	service, cleanup, err = newDHCP(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	store, err = service.Scope("lan")
	if err != nil {
		t.Fatal(err)
	}
	active, err := store.List(ctx)
	if err != nil || len(active) != 1 || !active[0].ExpiresAt.Equal(bound.ExpiresAt) || active[0].Address != reservation.IP {
		t.Fatalf("lost binding on restart: %+v, %v", active, err)
	}
	if reservations := store.Configuration().Config.Reservations; len(reservations) != 1 || reservations[0] != reservation {
		t.Fatalf("lost API reservation on restart: %+v", reservations)
	}
	for _, name := range []string{"dhcp-lan.db", "dhcp-guest.db"} {
		if _, err := os.Stat(filepath.Join(cfg.Node.DataDir, name)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDHCPRuntimeRetirementAndFailureCleanup(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "live"}[live], func(t *testing.T) {
			cfg := dhcpTestConfig(t)
			opts, err := cfg.DHCP.Config()
			if err != nil {
				t.Fatal(err)
			}
			oldPath := filepath.Join(cfg.Node.DataDir, "dhcp.db")
			old, err := dhcp.OpenLeases(oldPath, opts)
			if err != nil {
				t.Fatal(err)
			}
			if live {
				if _, err := old.Offer(context.Background(), dhcp.Client{MAC: "02:00:00:00:00:01"}, netip.Addr{}); err != nil {
					t.Fatal(err)
				}
			}
			_ = old.Close()
			cfg.DHCP = dhcpNamedConfig(t).DHCP
			service, cleanup, err := newDHCP(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if live {
				if err == nil || !strings.Contains(err.Error(), "live allocations remain") || service != nil {
					t.Fatalf("live scope retirement: service=%v error=%v", service, err)
				}
			} else if err != nil {
				t.Fatal(err)
			} else if other, err := dhcp.OpenLeases(oldPath, opts); err == nil {
				_ = other.Close()
				t.Fatal("retired database was not held exclusively")
			}
			cleanup()
			owner, err := dhcp.LockServiceDirectory(cfg.Node.DataDir)
			if err != nil {
				t.Fatalf("cleanup retained directory lock: %v", err)
			}
			_ = owner.Close()
			configs, err := cfg.DHCP.Configs()
			if err != nil {
				t.Fatal(err)
			}
			for _, scope := range configs {
				store, err := dhcp.OpenLeases(filepath.Join(cfg.Node.DataDir, "dhcp-"+scope.Name+".db"), scope)
				if err != nil {
					t.Fatalf("cleanup retained scope lock: %v", err)
				}
				_ = store.Close()
			}
		})
	}
}

func TestDHCPDisabledDNSResolverFallsThrough(t *testing.T) {
	msg := new(dns.Msg)
	msg.SetQuestion("printer.home.arpa.", dns.TypeA)
	r := dhcpLocalRecords(nil)
	if _, ok := r.ResolveLocal(&pipeline.Request{Msg: msg}); ok {
		t.Fatal("disabled DHCP claimed a DNS name")
	}
}

func TestDHCPRuntimeRejectsEnvironmentOverrideOfPersistedAPIState(t *testing.T) {
	cfg := dhcpTestConfig(t)
	path := filepath.Join(cfg.Node.DataDir, "kudurru.yaml")
	file, err := os.ReadFile(path) // #nosec G304 -- test-owned configuration in t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(file, []byte("  management: api\n")...), 0o600); err != nil { // #nosec G703 -- test-owned path and fixture contents
		t.Fatal(err)
	}
	load := func(env []string) (*config.Config, error) {
		loaded, err := config.Load(path, env)
		if loaded != nil {
			loaded.Node.DataDir = cfg.Node.DataDir
		}
		return loaded, err
	}
	initial, err := load(nil)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service, cleanup, err := newDHCP(initial, logger)
	if err != nil {
		t.Fatal(err)
	}
	store, err := service.Scope("default")
	if err != nil {
		t.Fatal(err)
	}
	before := store.Configuration()
	cleanup()
	if _, err := load([]string{"KUDURRU_DHCP__LEASEDURATION=1h"}); err == nil {
		t.Fatal("API-owned options silently masked an environment override")
	}
	// Removing the conflicting variable must recover without changing the DB.
	restarted, err := load(nil)
	if err != nil {
		t.Fatal(err)
	}
	service, cleanup, err = newDHCP(restarted, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	store, err = service.Scope("default")
	if err != nil {
		t.Fatal(err)
	}
	after := store.Configuration()
	if after.Revision != before.Revision || after.Config.LeaseDuration != 12*time.Hour {
		t.Fatalf("rejected override changed persisted configuration: %+v", after)
	}
}

func TestDHCPDisabledDoesNotOpenDatabase(t *testing.T) {
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Node.DataDir = filepath.Join(t.TempDir(), "absent")
	server, cleanup, err := newDHCP(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if server != nil {
		t.Fatal("default configuration started DHCP")
	}
	if _, err := os.Stat(cfg.Node.DataDir); !os.IsNotExist(err) {
		t.Fatalf("disabled DHCP touched its data directory: %v", err)
	}
}

func TestDHCPRuntimeRetainsLeasesAndCleansUpOnBindFailure(t *testing.T) {
	cfg := dhcpTestConfig(t)
	opts, err := cfg.DHCP.Config()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.Node.DataDir, "dhcp.db")
	leases, err := dhcp.OpenLeases(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	client := dhcp.Client{ID: "mac:02:00:00:00:00:01", MAC: "02:00:00:00:00:01", Hostname: "desktop"}
	offer, err := leases.Offer(context.Background(), client, netip.Addr{})
	if err != nil {
		t.Fatal(err)
	}
	bound, err := leases.Acknowledge(context.Background(), client, offer.Address, dhcp.Selecting)
	if err != nil {
		t.Fatal(err)
	}
	if err := leases.Close(); err != nil {
		t.Fatal(err)
	}
	server, cleanup, err := newDHCP(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	// A nonexistent interface fails before binding or sending any packets.
	if err := server.Listen(); err == nil {
		cleanup()
		t.Fatal("DHCP listener accepted a nonexistent interface")
	}
	cleanup()
	leases, err = dhcp.OpenLeases(path, opts)
	if err != nil {
		t.Fatalf("startup cleanup did not release lease database: %v", err)
	}
	defer func() { _ = leases.Close() }()
	active, err := leases.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].Address != bound.Address || active[0].ClientID != client.ID || !active[0].ExpiresAt.Equal(bound.ExpiresAt) {
		t.Fatalf("runtime restart lost or changed the lease: %+v", active)
	}
}

// On the kube store, internal/dhcpkube serves DHCPScope objects: the
// node-local service is not created and no local lease database appears.
func TestNodeDHCPLeavesTheKubeStoreToDHCPKube(t *testing.T) {
	cfg := dhcpTestConfig(t)
	cfg.Store.Driver = "kube"
	server, cleanup, err := newDHCP(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer cleanup()
	if err != nil || server != nil {
		t.Fatalf("node-local DHCP on the kube store: server=%v, error=%v", server, err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Node.DataDir, "dhcp.db")); !os.IsNotExist(err) {
		t.Fatalf("unsupported mode created lease state: %v", err)
	}
}
