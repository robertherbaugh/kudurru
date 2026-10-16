package dhcpsetup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/config"
)

func setupTestScope() config.DHCPScope {
	return config.DHCPScope{Name: "default", Management: "api", Interface: "fixture0", ServerIP: "192.0.2.2", Subnet: "192.0.2.0/24", RangeStart: "192.0.2.10", RangeEnd: "192.0.2.20", Router: "192.0.2.1", DNSServers: []string{"192.0.2.2"}, DomainName: "leases.test"}
}
func setupTestOpen(t *testing.T, dir string, host config.DHCP, locked []string) *Manager {
	t.Helper()
	m, err := Open(dir, host, locked)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}
func setupTestRead(t *testing.T, m *Manager) State {
	t.Helper()
	s, err := m.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func setupTestSave(t *testing.T, m *Manager, enabled bool, scopes []config.DHCPScope, adopt bool) State {
	t.Helper()
	s := setupTestRead(t, m)
	next, err := m.Save(context.Background(), s.Revision, enabled, scopes, adopt, s.ScopeRevisions)
	if err != nil {
		t.Fatal(err)
	}
	return next
}

func TestSetupFreshDurabilityAndFixedStartup(t *testing.T) {
	for _, host := range []config.DHCP{{}, {Management: "file", LeaseDuration: config.Duration(12 * time.Hour), DNSTTL: config.Duration(30 * time.Second)}} {
		dir := t.TempDir()
		m := setupTestOpen(t, dir, host, nil)
		initial := setupTestRead(t, m)
		if initial.Enabled || initial.ManagedBy != "api" || initial.Revision != 1 || initial.RestartRequired || len(initial.Scopes) != 0 {
			t.Fatalf("fresh: %+v", initial)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != lockFilename {
			t.Fatalf("fresh open created state/lease DB: %v", entries)
		}
		next := setupTestSave(t, m, true, []config.DHCPScope{setupTestScope()}, false)
		if next.Revision != 2 || !next.RestartRequired || len(next.PendingScopes) != 1 || m.Effective().Enabled || len(m.Startup()) != 0 {
			t.Fatalf("save activated pending state: %+v", next)
		}
		if next.Scopes[0].LeaseDuration != config.Duration(12*time.Hour) || next.Scopes[0].DNSTTL != config.Duration(30*time.Second) {
			t.Fatal("scope defaults missing")
		}
		same := setupTestSave(t, m, true, next.Scopes, false)
		if same.Revision != next.Revision {
			t.Fatal("unchanged save advanced revision")
		}
		if err := m.Close(); err != nil {
			t.Fatal(err)
		}
		m = setupTestOpen(t, dir, config.DHCP{Enabled: true, Interface: "invalid-host-seed"}, nil)
		if got := m.Effective(); !got.Enabled || len(got.Scopes) != 1 || got.Scopes[0].Interface != "fixture0" {
			t.Fatalf("saved setup did not override host: %+v", got)
		}
		if m.State().RestartRequired || len(m.Startup()) != 1 {
			t.Fatal("restart baseline/plan wrong")
		}
	}
}

func TestSetupAdoptionAndDetachedValues(t *testing.T) {
	scope := setupTestScope()
	scope.Management = "file"
	scope.TrustedRelays = []config.DHCPTrustedRelay{{SourceIP: "198.51.100.1", GatewayIP: "192.0.2.1"}}
	scope.Reservations = []config.DHCPReservation{{MAC: "02:00:00:00:00:01", IP: "192.0.2.90", Hostname: "printer"}}
	host := config.DHCP{Scopes: []config.DHCPScope{scope}}
	m := setupTestOpen(t, t.TempDir(), host, nil)
	state := setupTestRead(t, m)
	if state.ManagedBy != "file" {
		t.Fatal("host ownership lost")
	}
	if _, err := m.Save(context.Background(), state.Revision, false, state.Scopes, false, state.ScopeRevisions); !errors.Is(err, ErrManagedByFile) {
		t.Fatalf("implicit adoption: %v", err)
	}
	state.Scopes[0].Management = "api"
	adopted := setupTestSave(t, m, false, state.Scopes, true)
	if adopted.ManagedBy != "api" || !adopted.RestartRequired {
		t.Fatal("adoption did not require restart")
	}
	baseline := m.Effective()
	host.Scopes[0].DNSServers[0] = "203.0.113.9"
	baseline.Scopes[0].DNSServers[0] = "203.0.113.8"
	adopted.Scopes[0].Reservations[0].Hostname = "mutated"
	adopted.Scopes[0].TrustedRelays[0].GatewayIP = "203.0.113.1"
	adopted.ScopeRevisions["default"] = 999
	adopted.PendingScopes[0] = "mutated"
	current := setupTestRead(t, m)
	if current.Scopes[0].Reservations[0].Hostname != "printer" || current.Scopes[0].TrustedRelays[0].GatewayIP != "192.0.2.1" || current.ScopeRevisions["default"] != 0 || current.PendingScopes[0] != "default" || m.Effective().Scopes[0].DNSServers[0] != "192.0.2.2" {
		t.Fatal("manager aliases caller slices/maps")
	}
}

func TestSetupEnvironmentPreventsTakeover(t *testing.T) {
	for _, path := range []string{"dhcp.enabled", "dhcp.management", "dhcp.interface", "dhcp.serverIP", "dhcp.leaseDuration", "dhcp.scopes"} {
		t.Run(path, func(t *testing.T) {
			dir := t.TempDir()
			m := setupTestOpen(t, dir, config.DHCP{}, []string{path})
			state := m.State()
			if !state.EnvironmentLocked {
				t.Fatal("environment provenance ignored")
			}
			if _, err := m.Save(context.Background(), 1, false, nil, true, nil); !errors.Is(err, ErrEnvironmentLocked) {
				t.Fatalf("environment takeover: %v", err)
			}
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			m = setupTestOpen(t, dir, config.DHCP{}, nil)
			setupTestSave(t, m, true, []config.DHCPScope{setupTestScope()}, false)
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			if reopened, err := Open(dir, config.DHCP{}, []string{path}); !errors.Is(err, ErrEnvironmentLocked) {
				if reopened != nil {
					_ = reopened.Close()
				}
				t.Fatalf("saved UI overrode env: %v", err)
			}
			_ = setupTestOpen(t, dir, config.DHCP{}, []string{"node.name"})
		})
	}
}

func TestSetupDisabledHostDraftAndStrictSaves(t *testing.T) {
	m := setupTestOpen(t, t.TempDir(), config.DHCP{Interface: "fixture0"}, nil)
	state := setupTestRead(t, m)
	if len(state.Scopes) != 1 || state.ManagedBy != "file" || state.Scopes[0].Name != "default" {
		t.Fatalf("disabled host draft lost: %+v", state)
	}
	state.Scopes[0].Management = "api"
	if _, err := m.Save(context.Background(), 1, false, state.Scopes, true, state.ScopeRevisions); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("incomplete disabled rows persisted: %v", err)
	}
	for _, tc := range []struct {
		name string
		edit func(*config.DHCPScope)
	}{
		{"unsafe name", func(s *config.DHCPScope) { s.Name = "../escape" }},
		{"file scope", func(s *config.DHCPScope) { s.Management = "file" }},
		{"missing interface", func(s *config.DHCPScope) { s.Interface = "" }},
		{"router infrastructure reservation", func(s *config.DHCPScope) {
			s.Reservations = []config.DHCPReservation{{MAC: "02:00:00:00:00:01", IP: s.Router}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := setupTestScope()
			tc.edit(&s)
			if _, err := m.Save(context.Background(), 1, false, []config.DHCPScope{s}, true, state.ScopeRevisions); !errors.Is(err, ErrInvalidConfiguration) {
				t.Fatalf("invalid desired state: %v", err)
			}
		})
	}
	if _, err := m.Save(context.Background(), 1, true, nil, true, state.ScopeRevisions); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("enabled empty: %v", err)
	}
}

func TestSetupConcurrentExpectedRevision(t *testing.T) {
	m := setupTestOpen(t, t.TempDir(), config.DHCP{}, nil)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			_, err := m.Save(context.Background(), 1, true, []config.DHCPScope{setupTestScope()}, false, map[string]uint64{})
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	success, conflicts := 0, 0
	for err := range errs {
		if err == nil {
			success++
		} else if errors.Is(err, ErrRevisionConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflicts != 7 {
		t.Fatalf("revision race: %d / %d", success, conflicts)
	}
}

func TestSetupContextAndClose(t *testing.T) {
	m := setupTestOpen(t, t.TempDir(), config.DHCP{}, nil)
	before := m.State()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Save(ctx, 1, true, []config.DHCPScope{setupTestScope()}, false, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled save: %v", err)
	}
	if !reflect.DeepEqual(before, m.State()) {
		t.Fatal("canceled save published")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Save(context.Background(), 1, false, nil, false, nil); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("save after close: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(m.path), settingsFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled save wrote document: %v", err)
	}
}

func TestSetupSavedEmptyRegistryOverridesEnabledHost(t *testing.T) {
	dir := t.TempDir()
	host := config.DHCP{Enabled: true, Scopes: []config.DHCPScope{setupTestScope()}}
	m := setupTestOpen(t, dir, host, nil)
	setupTestSave(t, m, false, nil, true)
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	m = setupTestOpen(t, dir, host, nil)
	if effective := m.Effective(); effective.Enabled || len(effective.Scopes) != 0 || effective.Interface != "" {
		t.Fatalf("saved disabled empty registry resurrected YAML: %+v", effective)
	}
	if state := setupTestRead(t, m); state.Enabled || len(state.Scopes) != 0 || state.ManagedBy != "api" {
		t.Fatalf("saved empty registry: %+v", state)
	}
}
