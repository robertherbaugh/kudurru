package main

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dhcp"
	"github.com/robertherbaugh/kudurru/internal/dhcpsetup"
)

// These acceptance tests use real setup files and SQLite leases. The injected
// listener never calls Listen or Run, so no sockets or interface traffic exist.
type setupAcceptanceRuntime struct {
	setup   *dhcpsetup.Manager
	service *dhcp.Service
	failure string
	close   func()
	listens int
}

func startSetupAcceptance(t *testing.T, cfg *config.Config, bindErr error) *setupAcceptanceRuntime {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("dashboard setup runtime is Linux-only")
	}
	r := new(setupAcceptanceRuntime)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, setup, service, failure, cleanup, err := prepareDHCPWithListener(cfg, logger, func(*dhcp.Service) error {
		r.listens++
		return bindErr
	})
	t.Cleanup(cleanup)
	if err != nil {
		t.Fatalf("prepare isolated DHCP setup: %v", err)
	}
	if setup == nil {
		t.Fatal("setup manager is unavailable")
	}
	r.setup, r.service, r.failure, r.close = setup, service, failure, cleanup
	return r
}

func readSetupAcceptance(t *testing.T, manager *dhcpsetup.Manager) dhcpsetup.State {
	t.Helper()
	state, err := manager.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func saveSetupAcceptance(t *testing.T, manager *dhcpsetup.Manager, enabled bool, scopes []config.DHCPScope, adopt bool) dhcpsetup.State {
	t.Helper()
	state := readSetupAcceptance(t, manager)
	expected := make(map[string]uint64, len(state.ScopeRevisions)+len(scopes))
	for name, revision := range state.ScopeRevisions {
		expected[name] = revision
	}
	for _, scope := range scopes {
		if _, ok := expected[scope.Name]; !ok {
			expected[scope.Name] = 0
		}
	}
	saved, err := manager.Save(context.Background(), state.Revision, enabled, scopes, adopt, expected)
	if err != nil {
		t.Fatalf("save isolated DHCP setup: %v", err)
	}
	return saved
}

func setupAcceptanceScope() config.DHCPScope {
	return config.DHCPScope{Name: "default", Management: "api", Interface: "kdr-setup-test",
		ServerIP: "192.0.2.2", Subnet: "192.0.2.0/24", RangeStart: "192.0.2.100", RangeEnd: "192.0.2.110",
		Router: "192.0.2.1", DNSServers: []string{"192.0.2.2"}, DomainName: "home.arpa"}
}

func setupAcceptanceStore(t *testing.T, service *dhcp.Service, name string) *dhcp.LeaseStore {
	t.Helper()
	if service == nil {
		t.Fatal("expected running DHCP service")
	}
	store, err := service.Scope(name)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func setupAcceptanceBind(t *testing.T, store *dhcp.LeaseStore, client dhcp.Client) dhcp.Lease {
	t.Helper()
	ctx := context.Background()
	offer, err := store.Offer(ctx, client, netip.Addr{})
	if err != nil {
		t.Fatal(err)
	}
	bound, err := store.Acknowledge(ctx, client, offer.Address, dhcp.Selecting)
	if err != nil {
		t.Fatal(err)
	}
	return bound
}

func assertSetupAcceptanceLease(t *testing.T, store *dhcp.LeaseStore, want dhcp.Lease) {
	t.Helper()
	leases, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].Address != want.Address || leases[0].ClientID != want.ClientID ||
		leases[0].MAC != want.MAC || leases[0].Hostname != want.Hostname || leases[0].State != want.State || !leases[0].ExpiresAt.Equal(want.ExpiresAt) {
		t.Fatalf("restart changed durable lease: got %+v, want %+v", leases, want)
	}
}

func setupAcceptanceOptions(cfg dhcp.Config) dhcp.PoolOptions {
	return dhcp.PoolOptions{RangeStart: cfg.RangeStart, RangeEnd: cfg.RangeEnd, Router: cfg.Router,
		DNSServers: cfg.DNSServers, DomainName: cfg.DomainName, LeaseDuration: cfg.LeaseDuration,
		RegisterDNS: cfg.RegisterDNS, DNSTTL: cfg.DNSTTL}
}

func TestDHCPSetupAcceptanceEmptySaveAndRestart(t *testing.T) {
	cfg := dhcpTestConfig(t)
	cfg.DHCP = config.DHCP{}
	r := startSetupAcceptance(t, cfg, nil)
	state := readSetupAcceptance(t, r.setup)
	if r.service != nil || r.listens != 0 || state.Enabled || state.ManagedBy != "api" || len(state.Scopes) != 0 {
		t.Fatalf("unexpected initial disabled setup: %+v", state)
	}
	saved := saveSetupAcceptance(t, r.setup, true, []config.DHCPScope{setupAcceptanceScope()}, false)
	if !saved.RestartRequired || !saved.Enabled || r.service != nil || r.setup.Effective().Enabled {
		t.Fatalf("save activated setup before restart: %+v", saved)
	}
	if _, err := os.Stat(filepath.Join(cfg.Node.DataDir, "dhcp.db")); !os.IsNotExist(err) {
		t.Fatalf("saving setup opened a lease database: %v", err)
	}
	r.close()
	r = startSetupAcceptance(t, cfg, nil)
	state = readSetupAcceptance(t, r.setup)
	if r.failure != "" || r.listens != 1 || r.service == nil || !slices.Equal(r.service.ScopeNames(), []string{"default"}) || state.RestartRequired || len(state.PendingScopes) != 0 {
		t.Fatalf("saved setup did not become active: failure=%q state=%+v", r.failure, state)
	}
	store := setupAcceptanceStore(t, r.service, "default")
	if store.Configuration().Config.Management != "api" || state.ScopeRevisions["default"] != store.Configuration().Revision {
		t.Fatal("setup revision view does not match active allocator")
	}
	if _, err := os.Stat(filepath.Join(cfg.Node.DataDir, "dhcp.db")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Node.DataDir, "dhcp-default.db")); !os.IsNotExist(err) {
		t.Fatalf("default scope used a new lease filename: %v", err)
	}
}

func TestDHCPSetupAcceptanceExplicitAdoptionPreservesLegacyLeases(t *testing.T) {
	cfg := dhcpTestConfig(t)
	cfg.DHCP.Reservations = []config.DHCPReservation{{MAC: "02:00:00:00:00:10", IP: "192.0.2.10", Hostname: "printer"}}
	r := startSetupAcceptance(t, cfg, nil)
	store := setupAcceptanceStore(t, r.service, "default")
	client := dhcp.Client{ID: string([]byte{1, 0, 255, 42}), MAC: "02:00:00:00:00:10", Hostname: "printer"}
	bound := setupAcceptanceBind(t, store, client)
	state := readSetupAcceptance(t, r.setup)
	if state.ManagedBy != "file" || len(state.Scopes) != 1 {
		t.Fatalf("legacy scope ownership: %+v", state)
	}
	state.Scopes[0].Management = "api"
	if _, err := r.setup.Save(context.Background(), state.Revision, true, state.Scopes, false, state.ScopeRevisions); !errors.Is(err, dhcpsetup.ErrManagedByFile) {
		t.Fatalf("adoption did not require explicit intent: %v", err)
	}
	saved := saveSetupAcceptance(t, r.setup, true, state.Scopes, true)
	if !saved.RestartRequired || store.Configuration().Config.Management != "file" {
		t.Fatal("adoption changed active ownership before restart")
	}
	r.close()
	r = startSetupAcceptance(t, cfg, nil)
	if r.failure != "" {
		t.Fatal(r.failure)
	}
	store = setupAcceptanceStore(t, r.service, "default")
	assertSetupAcceptanceLease(t, store, bound)
	effective := store.Configuration().Config
	if effective.Management != "api" || len(effective.Reservations) != 1 || effective.Reservations[0].MAC != client.MAC || effective.Reservations[0].IP != bound.Address {
		t.Fatalf("adoption lost reservation or ownership: %+v", effective)
	}
	document, err := store.Export(context.Background())
	if err != nil || len(document.Leases) != 1 || document.Leases[0].Identity.Kind != "client-id" || document.Leases[0].Identity.Value != base64.StdEncoding.EncodeToString([]byte(client.ID)) {
		t.Fatalf("adoption changed opaque client identity: %+v, %v", document, err)
	}
	if _, err := store.Acknowledge(context.Background(), client, bound.Address, dhcp.Renewing); err != nil {
		t.Fatalf("adopted client could not renew: %v", err)
	}
	r.close()
	r = startSetupAcceptance(t, cfg, nil)
	if r.failure != "" || readSetupAcceptance(t, r.setup).ManagedBy != "api" {
		t.Fatalf("second adopted restart failed: %s", r.failure)
	}
}

func TestDHCPSetupAcceptanceLiveOverlayAndApplyToken(t *testing.T) {
	cfg := dhcpTestConfig(t)
	cfg.DHCP = config.DHCP{}
	r := startSetupAcceptance(t, cfg, nil)
	saveSetupAcceptance(t, r.setup, true, []config.DHCPScope{setupAcceptanceScope()}, false)
	r.close()
	r = startSetupAcceptance(t, cfg, nil)
	store := setupAcceptanceStore(t, r.service, "default")
	bound := setupAcceptanceBind(t, store, dhcp.Client{MAC: "02:00:00:00:00:01"})
	mutate := func(domain string) {
		t.Helper()
		if err := r.service.MutateScope(context.Background(), "default", func(ctx context.Context, leases *dhcp.LeaseStore) error {
			state := leases.Configuration()
			options := setupAcceptanceOptions(state.Config)
			options.DomainName = domain
			return leases.UpdateOptions(ctx, state.Revision, options)
		}); err != nil {
			t.Fatal(err)
		}
	}
	stale := readSetupAcceptance(t, r.setup)
	mutate("live.home.arpa")
	if _, err := r.setup.Save(context.Background(), stale.Revision, true, stale.Scopes, false, stale.ScopeRevisions); !errors.Is(err, dhcpsetup.ErrRevisionConflict) {
		t.Fatalf("setup revision accepted stale live pool snapshot: %v", err)
	}
	state := readSetupAcceptance(t, r.setup)
	if state.Scopes[0].DomainName != "live.home.arpa" || state.ScopeRevisions["default"] != store.Configuration().Revision {
		t.Fatalf("setup read hid live options: %+v", state)
	}
	remote := config.DHCPScope{Name: "remote", Management: "api", Interface: state.Scopes[0].Interface, ServerIP: "192.0.2.2",
		Subnet: "203.0.113.0/24", RelayOnly: true, TrustedRelays: []config.DHCPTrustedRelay{{SourceIP: "198.51.100.1", GatewayIP: "203.0.113.1"}},
		RangeStart: "203.0.113.100", RangeEnd: "203.0.113.110", Router: "203.0.113.1", DNSServers: []string{"192.0.2.2"}}
	saved := saveSetupAcceptance(t, r.setup, true, append(state.Scopes, remote), false)
	if !saved.RestartRequired || !slices.Equal(r.service.ScopeNames(), []string{"default"}) {
		t.Fatal("new scope changed active registry before restart")
	}
	beforeRevision := store.Configuration().Revision
	r.close()
	r = startSetupAcceptance(t, cfg, nil)
	store = setupAcceptanceStore(t, r.service, "default")
	if r.failure != "" || !slices.Equal(r.service.ScopeNames(), []string{"default", "remote"}) || store.Configuration().Config.DomainName != "live.home.arpa" || store.Configuration().Revision != beforeRevision {
		t.Fatalf("unrelated topology restart changed live options: %+v, %s", store.Configuration(), r.failure)
	}
	assertSetupAcceptanceLease(t, store, bound)
	mutate("after-apply.home.arpa")
	beforeRevision = store.Configuration().Revision
	r.close()
	r = startSetupAcceptance(t, cfg, nil)
	store = setupAcceptanceStore(t, r.service, "default")
	if r.failure != "" || store.Configuration().Config.DomainName != "after-apply.home.arpa" || store.Configuration().Revision != beforeRevision {
		t.Fatalf("replayed setup token overwrote live edit: %+v, %s", store.Configuration(), r.failure)
	}
	assertSetupAcceptanceLease(t, store, bound)
}

func TestDHCPSetupAcceptanceDisabledEmptyScopeMove(t *testing.T) {
	cfg := dhcpTestConfig(t)
	cfg.DHCP = config.DHCP{}
	r := startSetupAcceptance(t, cfg, nil)
	saveSetupAcceptance(t, r.setup, true, []config.DHCPScope{setupAcceptanceScope()}, false)
	r.close()
	r = startSetupAcceptance(t, cfg, nil)
	store := setupAcceptanceStore(t, r.service, "default")
	state := readSetupAcceptance(t, r.setup)
	moved := state.Scopes[0]
	moved.Subnet, moved.ServerIP, moved.Router = "203.0.113.0/24", "203.0.113.2", "203.0.113.1"
	moved.RangeStart, moved.RangeEnd = "203.0.113.100", "203.0.113.120"
	moved.DNSServers, moved.DomainName = []string{"203.0.113.2"}, "moved.home.arpa"
	saveSetupAcceptance(t, r.setup, false, []config.DHCPScope{moved}, false)
	if store.Configuration().Config.Subnet.String() != "192.0.2.0/24" {
		t.Fatal("staging edited the running allocator")
	}
	r.close()
	r = startSetupAcceptance(t, cfg, nil)
	if r.service != nil || r.listens != 0 || r.failure != "" {
		t.Fatal("disabled restart activated DHCP")
	}
	state = readSetupAcceptance(t, r.setup)
	if state.Enabled || len(state.Scopes) != 1 || state.Scopes[0].DomainName != moved.DomainName || state.Scopes[0].Subnet != moved.Subnet {
		t.Fatalf("disabled setup lost desired edits: %+v", state)
	}
	saveSetupAcceptance(t, r.setup, true, state.Scopes, false)
	r.close()
	r = startSetupAcceptance(t, cfg, nil)
	if r.failure != "" {
		t.Fatalf("empty scope could not move: %s", r.failure)
	}
	effective := setupAcceptanceStore(t, r.service, "default").Configuration().Config
	if effective.Subnet.String() != moved.Subnet || effective.DomainName != moved.DomainName || effective.RangeEnd.String() != moved.RangeEnd || effective.Router.String() != moved.Router {
		t.Fatalf("persisted seeds masked staged desired setup: %+v", effective)
	}
}

func TestDHCPSetupAcceptanceBindFailureRepairAndCleanup(t *testing.T) {
	cfg := dhcpTestConfig(t)
	cfg.DHCP = config.DHCP{}
	r := startSetupAcceptance(t, cfg, nil)
	saveSetupAcceptance(t, r.setup, true, []config.DHCPScope{setupAcceptanceScope()}, false)
	r.close()
	injected := errors.New("injected missing interface or capability")
	r = startSetupAcceptance(t, cfg, injected)
	if r.service != nil || r.listens != 1 || !strings.Contains(r.failure, injected.Error()) {
		t.Fatalf("failed activation did not leave repair manager: %+v", r)
	}
	// Failed activation releases allocator and directory ownership immediately,
	// while the setup manager retains its exclusive lock for UI repair.
	owner, err := dhcp.LockServiceDirectory(cfg.Node.DataDir)
	if err != nil {
		t.Fatalf("failed bind retained runtime ownership: %v", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if other, err := dhcpsetup.Open(cfg.Node.DataDir, cfg.DHCP, nil); err == nil {
		_ = other.Close()
		t.Fatal("failed runtime released the active repair manager lock")
	}
	state := readSetupAcceptance(t, r.setup)
	state.Scopes[0].Interface = "kdr-repaired"
	saveSetupAcceptance(t, r.setup, true, state.Scopes, false)
	r.close()
	r = startSetupAcceptance(t, cfg, nil)
	if r.failure != "" || r.listens != 1 || r.service == nil {
		t.Fatalf("repaired setup did not resume: %s", r.failure)
	}
	if setupAcceptanceStore(t, r.service, "default").Configuration().Config.Interface != "kdr-repaired" {
		t.Fatal("repair did not select staged topology")
	}
}

func TestDHCPSetupAcceptanceLateAllocationBlocksRestartAndCanBeReverted(t *testing.T) {
	for _, allocation := range []string{"offered", "bound", "declined"} {
		t.Run(allocation, func(t *testing.T) {
			cfg := dhcpTestConfig(t)
			cfg.DHCP = config.DHCP{}
			r := startSetupAcceptance(t, cfg, nil)
			saveSetupAcceptance(t, r.setup, true, []config.DHCPScope{setupAcceptanceScope()}, false)
			r.close()
			r = startSetupAcceptance(t, cfg, nil)
			store := setupAcceptanceStore(t, r.service, "default")
			original := readSetupAcceptance(t, r.setup).Scopes
			moved := original[0]
			moved.Subnet, moved.ServerIP, moved.Router = "203.0.113.0/24", "203.0.113.2", "203.0.113.1"
			moved.RangeStart, moved.RangeEnd, moved.DNSServers = "203.0.113.100", "203.0.113.110", []string{"203.0.113.2"}
			saveSetupAcceptance(t, r.setup, true, []config.DHCPScope{moved}, false)
			// Even after an advisory staging check passes, the still-active old
			// scope can allocate before shutdown. Startup must validate again.
			client := dhcp.Client{MAC: "02:00:00:00:00:01"}
			lease, err := store.Offer(context.Background(), client, netip.Addr{})
			if err != nil {
				t.Fatal(err)
			}
			if allocation != "offered" {
				lease, err = store.Acknowledge(context.Background(), client, lease.Address, dhcp.Selecting)
				if err != nil {
					t.Fatal(err)
				}
			}
			if allocation == "declined" {
				if err := store.Decline(context.Background(), client, lease.Address); err != nil {
					t.Fatal(err)
				}
				leases, err := store.List(context.Background())
				if err != nil || len(leases) != 1 {
					t.Fatalf("decline state: %+v %v", leases, err)
				}
				lease = leases[0]
			}
			r.close()
			r = startSetupAcceptance(t, cfg, nil)
			if r.service != nil || r.failure == "" || r.listens != 0 {
				t.Fatalf("incompatible live %s did not block startup: %+v", allocation, r)
			}
			// The failed candidate transaction must leave both the previous
			// options and exact lease intact; the repair manager must still save.
			state := readSetupAcceptance(t, r.setup)
			if len(state.Scopes) != 1 || state.Scopes[0].Subnet != moved.Subnet {
				t.Fatalf("failed desired setup is not available for repair: %+v", state)
			}
			saveSetupAcceptance(t, r.setup, true, original, false)
			r.close()
			r = startSetupAcceptance(t, cfg, nil)
			if r.failure != "" {
				t.Fatal(r.failure)
			}
			assertSetupAcceptanceLease(t, setupAcceptanceStore(t, r.service, "default"), lease)
		})
	}
}

func TestDHCPSetupAcceptanceLateAllocationPreventsScopeRename(t *testing.T) {
	cfg := dhcpTestConfig(t)
	cfg.DHCP = config.DHCP{}
	r := startSetupAcceptance(t, cfg, nil)
	saveSetupAcceptance(t, r.setup, true, []config.DHCPScope{setupAcceptanceScope()}, false)
	r.close()
	r = startSetupAcceptance(t, cfg, nil)
	store := setupAcceptanceStore(t, r.service, "default")
	original := readSetupAcceptance(t, r.setup).Scopes
	renamed := original[0]
	renamed.Name = "renamed"
	saveSetupAcceptance(t, r.setup, true, []config.DHCPScope{renamed}, false)
	bound := setupAcceptanceBind(t, store, dhcp.Client{MAC: "02:00:00:00:00:01"})
	r.close()
	r = startSetupAcceptance(t, cfg, nil)
	if r.service != nil || !strings.Contains(r.failure, "live allocations remain") || r.listens != 0 {
		t.Fatalf("retired live default scope did not block renamed setup: %+v", r)
	}
	saveSetupAcceptance(t, r.setup, true, original, false)
	r.close()
	r = startSetupAcceptance(t, cfg, nil)
	if r.failure != "" {
		t.Fatal(r.failure)
	}
	assertSetupAcceptanceLease(t, setupAcceptanceStore(t, r.service, "default"), bound)
}

func TestDHCPSetupAcceptanceFileBindFailureRemainsFatal(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("dashboard setup runtime is Linux-only")
	}
	cfg := dhcpTestConfig(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	injected := errors.New("injected file-owned bind failure")
	_, manager, service, failure, cleanup, err := prepareDHCPWithListener(cfg, logger, func(*dhcp.Service) error { return injected })
	t.Cleanup(cleanup)
	if !errors.Is(err, injected) || manager != nil || service != nil || failure != "" {
		t.Fatalf("file-owned failure stopped being fatal: manager=%v service=%v runtimeError=%q err=%v", manager, service, failure, err)
	}
	// Both locks and the real lease DB must be reusable after fatal startup.
	r := startSetupAcceptance(t, cfg, nil)
	if r.failure != "" || r.service == nil || readSetupAcceptance(t, r.setup).ManagedBy != "file" {
		t.Fatalf("failed file-owned startup leaked ownership: %+v", r)
	}
}

func TestDHCPSetupAcceptanceMissingStagedDatabaseRecovery(t *testing.T) {
	cfg := dhcpTestConfig(t)
	cfg.DHCP = config.DHCP{}
	r := startSetupAcceptance(t, cfg, nil)
	saveSetupAcceptance(t, r.setup, true, []config.DHCPScope{setupAcceptanceScope()}, false)
	r.close()
	r = startSetupAcceptance(t, cfg, nil)
	state := readSetupAcceptance(t, r.setup)
	if state.ScopeRevisions["default"] == 0 {
		t.Fatal("expected a committed allocator revision before staging")
	}
	state.Scopes[0].DomainName = "recovered.home.arpa"
	staged := saveSetupAcceptance(t, r.setup, true, state.Scopes, false)
	r.close()
	// Model a lost database between staging and startup. The first failed
	// allocator open creates an empty file, then rolls back its schema when
	// the old nonzero expected revision does not match the missing state.
	if err := os.Remove(filepath.Join(cfg.Node.DataDir, "dhcp.db")); err != nil {
		t.Fatal(err)
	}
	r = startSetupAcceptance(t, cfg, nil)
	if r.service != nil || r.listens != 0 || !strings.Contains(r.failure, "revision conflict") {
		t.Fatalf("missing DB was silently seeded despite staged revision: %+v", r)
	}
	state = readSetupAcceptance(t, r.setup)
	if state.ScopeRevisions["default"] != 0 || !slices.Equal(state.PendingScopes, []string{"default"}) || state.Scopes[0].DomainName != "recovered.home.arpa" {
		t.Fatalf("failed startup did not preserve readable pending setup: %+v", state)
	}
	metadata, exists, err := dhcp.ReadLeaseSettings(context.Background(), filepath.Join(cfg.Node.DataDir, "dhcp.db"))
	if err != nil || !exists || metadata.Revision != 0 || metadata.AppliedSetupToken != "" {
		t.Fatalf("failed guarded startup committed new metadata: %+v exists=%v err=%v", metadata, exists, err)
	}
	// Explicitly accepting the freshly observed zero revision must refresh the
	// plan even though its desired scope fields are unchanged.
	saved := saveSetupAcceptance(t, r.setup, true, state.Scopes, false)
	if saved.Revision <= staged.Revision || !saved.RestartRequired {
		t.Fatalf("identical recovery save did not refresh the stale plan: %+v", saved)
	}
	r.close()
	r = startSetupAcceptance(t, cfg, nil)
	if r.failure != "" || r.listens != 1 {
		t.Fatalf("explicit recovery did not restart: %+v", r)
	}
	store := setupAcceptanceStore(t, r.service, "default")
	if effective := store.Configuration(); effective.Revision != 1 || effective.Config.DomainName != "recovered.home.arpa" {
		t.Fatalf("recovered DB did not apply desired setup: %+v", effective)
	}
	if state := readSetupAcceptance(t, r.setup); len(state.PendingScopes) != 0 || state.RestartRequired {
		t.Fatalf("recovered setup still reports pending activation: %+v", state)
	}
}

func TestDHCPSetupAcceptanceRemoveFailedEmptyScope(t *testing.T) {
	cfg := dhcpTestConfig(t)
	cfg.DHCP = config.DHCP{}
	remaining := setupAcceptanceScope()
	remaining.Name, remaining.Interface = "remaining", "kdr-remaining"
	remaining.Subnet, remaining.ServerIP, remaining.Router = "203.0.113.0/24", "203.0.113.2", "203.0.113.1"
	remaining.RangeStart, remaining.RangeEnd = "203.0.113.100", "203.0.113.110"
	remaining.DNSServers = []string{"203.0.113.2"}
	r := startSetupAcceptance(t, cfg, nil)
	saveSetupAcceptance(t, r.setup, true, []config.DHCPScope{setupAcceptanceScope(), remaining}, false)
	r.close()
	r = startSetupAcceptance(t, cfg, nil)
	store := setupAcceptanceStore(t, r.service, "remaining")
	bound := setupAcceptanceBind(t, store, dhcp.Client{ID: "remaining-client", MAC: "02:00:00:00:00:11"})
	remainingRevision := store.Configuration().Revision
	state := readSetupAcceptance(t, r.setup)
	if state.ScopeRevisions["default"] == 0 {
		t.Fatal("expected committed metadata before staging the failed scope")
	}
	for i := range state.Scopes {
		if state.Scopes[i].Name == "default" {
			state.Scopes[i].DomainName = "pending.home.arpa"
		}
	}
	saveSetupAcceptance(t, r.setup, true, state.Scopes, false)
	r.close()
	failedPath := filepath.Join(cfg.Node.DataDir, "dhcp.db")
	if err := os.Remove(failedPath); err != nil {
		t.Fatal(err)
	}
	r = startSetupAcceptance(t, cfg, nil)
	if r.service != nil || r.listens != 0 || !strings.Contains(r.failure, "revision conflict") {
		t.Fatalf("lost staged DB did not fail before activation: %+v", r)
	}
	before, err := os.Stat(failedPath)
	if err != nil {
		t.Fatal(err)
	}
	state = readSetupAcceptance(t, r.setup)
	if state.ScopeRevisions["default"] != 0 || !slices.Equal(state.PendingScopes, []string{"default"}) {
		t.Fatalf("failed scope is not readable for removal: %+v", state)
	}
	var retained []config.DHCPScope
	for _, scope := range state.Scopes {
		if scope.Name == "remaining" {
			retained = append(retained, scope)
		}
	}
	if len(retained) != 1 {
		t.Fatal("repair lost the remaining scope")
	}
	saved := saveSetupAcceptance(t, r.setup, true, retained, false)
	if !saved.Enabled || !saved.RestartRequired || len(saved.Scopes) != 1 {
		t.Fatalf("scope removal did not stage an enabled remaining service: %+v", saved)
	}
	r.close()
	r = startSetupAcceptance(t, cfg, nil)
	if r.failure != "" || r.listens != 1 || r.service == nil || !slices.Equal(r.service.ScopeNames(), []string{"remaining"}) {
		t.Fatalf("empty retired DB blocked remaining scope: %+v", r)
	}
	store = setupAcceptanceStore(t, r.service, "remaining")
	assertSetupAcceptanceLease(t, store, bound)
	if store.Configuration().Revision != remainingRevision {
		t.Fatal("retiring the failed scope reapplied unchanged remaining options")
	}
	if state := readSetupAcceptance(t, r.setup); !state.Enabled || state.RestartRequired || len(state.PendingScopes) != 0 {
		t.Fatalf("repaired runtime still reports pending setup: %+v", state)
	}
	after, err := os.Stat(failedPath)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("repair replaced or deleted retired storage: %v", err)
	}
	metadata, exists, err := dhcp.ReadLeaseSettings(context.Background(), failedPath)
	if err != nil || !exists || metadata.Revision != 0 || metadata.AppliedSetupToken != "" {
		t.Fatalf("retirement initialized the failed DB: %+v exists=%v err=%v", metadata, exists, err)
	}
	// The service must retain ownership even though this retired DB has no
	// schema, and cleanup must release it just like every other retired scope.
	if retired, err := dhcp.CheckRetiredDatabase(failedPath); err == nil {
		_ = retired.Close()
		t.Fatal("active service did not hold the retired empty database lock")
	}
	r.close()
	retired, err := dhcp.CheckRetiredDatabase(failedPath)
	if err != nil {
		t.Fatalf("cleanup retained retired empty database lock: %v", err)
	}
	if err := retired.Close(); err != nil {
		t.Fatal(err)
	}
}
