package dhcpsetup

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dhcp"
)

func setupTestLeaseConfig(t *testing.T, scope config.DHCPScope) dhcp.Config {
	t.Helper()
	configs, err := (config.DHCP{Scopes: []config.DHCPScope{scope}}).Configs()
	if err != nil {
		t.Fatal(err)
	}
	return configs[0]
}

func setupTestOptions(c dhcp.Config) dhcp.PoolOptions {
	return dhcp.PoolOptions{RangeStart: c.RangeStart, RangeEnd: c.RangeEnd, Router: c.Router,
		DNSServers: c.DNSServers, DomainName: c.DomainName, LeaseDuration: c.LeaseDuration,
		RegisterDNS: c.RegisterDNS, DNSTTL: c.DNSTTL}
}

func TestSetupAdoptionUsesEffectiveAPISettingsOnly(t *testing.T) {
	for _, mode := range []string{"file", "api"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			scope := setupTestScope()
			store, err := dhcp.OpenLeases(filepath.Join(dir, "dhcp.db"), setupTestLeaseConfig(t, scope))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			opts := setupTestOptions(store.Configuration().Config)
			opts.DomainName = "persisted.test"
			if err := store.UpdateOptions(context.Background(), 1, opts); err != nil {
				t.Fatal(err)
			}
			scope.Management = mode
			m := setupTestOpen(t, dir, config.DHCP{Scopes: []config.DHCPScope{scope}}, nil)
			state := setupTestRead(t, m)
			want := scope.DomainName
			if mode == "api" {
				want = opts.DomainName
			}
			if state.Scopes[0].DomainName != want || state.ScopeRevisions["default"] != 2 || len(state.PendingScopes) != 0 {
				t.Fatalf("wrong host ownership overlay: %+v", state)
			}
			state.Scopes[0].Management = "api"
			saved := setupTestSave(t, m, false, state.Scopes, true)
			if saved.Scopes[0].DomainName != want || len(saved.PendingScopes) != 1 {
				t.Fatalf("adoption lost effective settings: %+v", saved)
			}
		})
	}
}

func TestSetupPendingConflictExplicitRefreshAndReplay(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	m := setupTestOpen(t, dir, config.DHCP{}, nil)
	setupTestSave(t, m, true, []config.DHCPScope{setupTestScope()}, false)
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	m = setupTestOpen(t, dir, config.DHCP{}, nil)
	plan := m.Startup()[0]
	cfg := setupTestLeaseConfig(t, m.Effective().Scopes[0])
	path := filepath.Join(dir, "dhcp.db")
	store, err := dhcp.OpenLeasesForSetup(path, cfg, plan.Token, plan.ExpectedRevision)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	state := setupTestRead(t, m)
	if len(state.PendingScopes) != 0 || state.ScopeRevisions["default"] != 1 {
		t.Fatalf("initial apply not visible: %+v", state)
	}
	// Stage desired content; the live allocator remains owned and writable.
	state.Scopes[0].DomainName = "desired.test"
	staged := setupTestSave(t, m, true, state.Scopes, false)
	firstToken := m.plans["default"].Token
	opts := setupTestOptions(store.Configuration().Config)
	opts.DomainName = "later-live.test"
	if err := store.UpdateOptions(ctx, 1, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Save(ctx, staged.Revision, true, staged.Scopes, false, staged.ScopeRevisions); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale staging succeeded: %v", err)
	}
	current := setupTestRead(t, m)
	if current.ScopeRevisions["default"] != 2 || current.Scopes[0].DomainName != "desired.test" || len(current.PendingScopes) != 1 {
		t.Fatalf("pending view hid desired data or current revision: %+v", current)
	}
	refreshed := setupTestSave(t, m, true, current.Scopes, false)
	newPlan := m.plans["default"]
	if refreshed.Revision != staged.Revision+1 || newPlan.Token == firstToken || newPlan.ExpectedRevision != 2 {
		t.Fatal("explicit refresh did not replace revision guard/token")
	}
	meta, _, err := dhcp.ReadLeaseSettings(ctx, path)
	if err != nil || meta.Revision != 2 || meta.Options.DomainName != "later-live.test" || meta.AppliedSetupToken != plan.Token {
		t.Fatalf("Save changed live DB: %+v %v", meta, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	m = setupTestOpen(t, dir, config.DHCP{}, nil)
	newPlan = m.Startup()[0]
	store, err = dhcp.OpenLeasesForSetup(path, setupTestLeaseConfig(t, m.Effective().Scopes[0]), newPlan.Token, newPlan.ExpectedRevision)
	if err != nil {
		t.Fatal(err)
	}
	current = setupTestRead(t, m)
	if len(current.PendingScopes) != 0 || current.ScopeRevisions["default"] != 3 || current.Scopes[0].DomainName != "desired.test" {
		t.Fatalf("successful retry not reflected: %+v", current)
	}
}

func TestSetupRevisionInventoryIncludesRetiredDatabases(t *testing.T) {
	dir := t.TempDir()
	scope := setupTestScope()
	store, err := dhcp.OpenLeases(filepath.Join(dir, "dhcp.db"), setupTestLeaseConfig(t, scope))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	m := setupTestOpen(t, dir, config.DHCP{}, nil)
	state := setupTestRead(t, m)
	if state.ScopeRevisions["default"] != 1 {
		t.Fatalf("retired revision missing in Read: %+v", state)
	}
	// Inventory must also survive Save's returned view so the same response
	// supplies the revision needed to restore a removed scope after failure.
	renamed := scope
	renamed.Name = "renamed"
	saved := setupTestSave(t, m, false, []config.DHCPScope{renamed}, false)
	if saved.ScopeRevisions["default"] != 1 || saved.ScopeRevisions["renamed"] != 0 {
		t.Fatalf("retired revision missing after Save: %+v", saved)
	}
	saved = setupTestSave(t, m, false, []config.DHCPScope{scope}, false)
	if saved.Scopes[0].Name != "default" || saved.ScopeRevisions["default"] != 1 {
		t.Fatal("failed rename could not restore original scope")
	}
}

func TestSetupRevertingPendingEditRestoresRunningToken(t *testing.T) {
	dir := t.TempDir()
	m := setupTestOpen(t, dir, config.DHCP{}, nil)
	setupTestSave(t, m, true, []config.DHCPScope{setupTestScope()}, false)
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	m = setupTestOpen(t, dir, config.DHCP{}, nil)
	plan := m.Startup()[0]
	cfg := setupTestLeaseConfig(t, m.Effective().Scopes[0])
	store, err := dhcp.OpenLeasesForSetup(filepath.Join(dir, "dhcp.db"), cfg, plan.Token, plan.ExpectedRevision)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	opts := setupTestOptions(store.Configuration().Config)
	opts.DomainName = "live.test"
	if err := store.UpdateOptions(context.Background(), 1, opts); err != nil {
		t.Fatal(err)
	}
	active := setupTestRead(t, m)
	desired := cloneScopes(active.Scopes)
	desired[0].Interface = "future0"
	pending := setupTestSave(t, m, true, desired, false)
	if !pending.RestartRequired || len(pending.PendingScopes) != 1 {
		t.Fatal("edit did not stage restart")
	}
	reverted := setupTestSave(t, m, true, active.Scopes, false)
	if reverted.RestartRequired || len(reverted.PendingScopes) != 0 || m.plans["default"].Token != plan.Token {
		t.Fatalf("reverting exact running scope left spurious pending change: %+v", reverted)
	}
	if current := store.Configuration(); current.Revision != 2 || current.Config.DomainName != "live.test" {
		t.Fatal("revert changed live options")
	}
}
