package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dhcp"
	"github.com/robertherbaugh/kudurru/internal/dhcpdns"
	"github.com/robertherbaugh/kudurru/internal/dhcpsetup"
)

// prepareDHCP loads dashboard-owned setup before constructing the allocator.
// A failed dashboard setup leaves DNS and the management UI available so an
// administrator can correct it. File-owned startup failures remain fatal.
func prepareDHCP(cfg *config.Config, logger *slog.Logger) (*config.Config, *dhcpsetup.Manager, *dhcp.Service, string, func(), error) {
	return prepareDHCPWithListener(cfg, logger, (*dhcp.Service).Listen)
}

func prepareDHCPWithListener(cfg *config.Config, logger *slog.Logger, listen func(*dhcp.Service) error) (*config.Config, *dhcpsetup.Manager, *dhcp.Service, string, func(), error) {
	cleanup := func() {}
	var setup *dhcpsetup.Manager
	if cfg.Store.Driver == "sqlite" && runtime.GOOS == "linux" {
		var err error
		setup, err = dhcpsetup.Open(cfg.Node.DataDir, cfg.DHCP, cfg.EnvLocked)
		if err != nil {
			return cfg, nil, nil, "", cleanup, fmt.Errorf("open DHCP setup: %w", err)
		}
		cleanup = func() { _ = setup.Close() }
		effective := *cfg
		effective.DHCP = setup.Effective()
		cfg = &effective
	}
	var plans []dhcpsetup.ScopePlan
	if setup != nil {
		plans = setup.Startup()
	}
	service, closeService, err := newDHCPWithSetup(cfg, logger, plans)
	closeSetup := cleanup
	cleanup = func() { closeService(); closeSetup() }
	if err == nil && service != nil {
		err = listen(service)
	}
	if err != nil {
		closeService()
		if setup != nil && setup.State().ManagedBy == "api" {
			logger.Error("DHCP setup could not start; correct it in the dashboard and restart", "error", err)
			return cfg, setup, nil, err.Error(), closeSetup, nil
		}
		cleanup()
		return cfg, nil, nil, "", func() {}, err
	}
	return cfg, setup, service, "", cleanup, nil
}

// newDHCP prepares the optional, host-local DHCPv4 service. The caller
// binds it before starting the runner and closes the socket before the
// lease database on every exit, including later startup failures.
func newDHCP(cfg *config.Config, logger *slog.Logger) (*dhcp.Service, func(), error) {
	return newDHCPWithSetup(cfg, logger, nil)
}

func newDHCPWithSetup(cfg *config.Config, logger *slog.Logger, plans []dhcpsetup.ScopePlan) (*dhcp.Service, func(), error) {
	cleanup := func() {}
	if !cfg.DHCP.Enabled || cfg.Store.Driver == "kube" {
		// On the kube store, internal/dhcpkube serves DHCPScope objects.
		return nil, cleanup, nil
	}
	if cfg.Store.Driver != "sqlite" {
		return nil, cleanup, errors.New("dhcp.enabled requires store.driver: sqlite for durable local lease replicas")
	}
	opts, err := cfg.DHCP.Configs()
	if err != nil {
		return nil, cleanup, err
	}
	if err := cfg.DHCP.Failover.ValidateScopes(opts); err != nil {
		return nil, cleanup, err
	}
	_, identityErr := os.Stat(filepath.Join(cfg.Node.DataDir, "dhcp-raft", "identity.json"))
	if identityErr != nil && !errors.Is(identityErr, os.ErrNotExist) {
		return nil, cleanup, identityErr
	}
	if !cfg.DHCP.Failover.Enabled && identityErr == nil {
		return nil, cleanup, errors.New("DHCP scope belongs to a failover cluster; restore failover settings instead of starting an independent allocator")
	}
	if err := os.MkdirAll(cfg.Node.DataDir, 0o750); err != nil {
		return nil, cleanup, err
	}
	owner, err := dhcp.LockServiceDirectory(cfg.Node.DataDir)
	if err != nil {
		return nil, cleanup, err
	}
	closers := []func(){func() { _ = owner.Close() }}
	cleanup = func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}
	fail := func(err error) (*dhcp.Service, func(), error) {
		cleanup()
		return nil, func() {}, err
	}
	paths := make(map[string]bool, len(opts))
	stores := make(map[string]*dhcp.LeaseStore, len(opts))
	staged := make(map[string]dhcpsetup.ScopePlan, len(plans))
	for _, plan := range plans {
		staged[plan.Name] = plan
	}
	for _, scope := range opts {
		name := scope.Name
		if name == "" {
			name = "default"
		}
		filename := "dhcp-" + name + ".db"
		if name == "default" {
			filename = "dhcp.db"
		}
		paths[filename] = true
		var leases *dhcp.LeaseStore
		if cfg.DHCP.Failover.Enabled {
			if plan, ok := staged[name]; ok && cfg.DHCP.Failover.Bootstrap && errors.Is(identityErr, os.ErrNotExist) {
				leases, err = dhcp.OpenReplicaLeasesForSetup(filepath.Join(cfg.Node.DataDir, filename), scope, plan.Token, plan.ExpectedRevision)
			} else {
				leases, err = dhcp.OpenReplicaLeases(filepath.Join(cfg.Node.DataDir, filename), scope)
			}
		} else if plan, ok := staged[name]; ok {
			leases, err = dhcp.OpenLeasesForSetup(filepath.Join(cfg.Node.DataDir, filename), scope, plan.Token, plan.ExpectedRevision)
		} else {
			leases, err = dhcp.OpenLeases(filepath.Join(cfg.Node.DataDir, filename), scope)
		}
		if err != nil {
			return fail(fmt.Errorf("open DHCP scope %s: %w", name, err))
		}
		stores[name] = leases
		closers = append(closers, func() { _ = leases.Close() })
	}
	// A renamed or removed scope must not leave a live allocation behind
	// that a new scope can reuse. Keep retired databases locked as well, so
	// another process cannot resume them while this service is active.
	entries, err := os.ReadDir(cfg.Node.DataDir)
	if err != nil {
		return fail(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		leaseDatabase := name == "dhcp.db" || strings.HasPrefix(name, "dhcp-") && strings.HasSuffix(name, ".db")
		if paths[name] || !leaseDatabase {
			continue
		}
		retired, err := dhcp.CheckRetiredDatabase(filepath.Join(cfg.Node.DataDir, name))
		if err != nil {
			return fail(fmt.Errorf("retired DHCP scope %s: %w", name, err))
		}
		closers = append(closers, func() { _ = retired.Close() })
	}
	service, err := dhcp.NewService(opts, stores, logger)
	if err != nil {
		return fail(fmt.Errorf("create DHCP service: %w", err))
	}
	if cfg.DHCP.Failover.Enabled {
		node, err := openDHCPFailover(cfg, stores, service, logger)
		if err != nil {
			_ = service.Close()
			return fail(err)
		}
		closers = append(closers, func() { _ = node.Close() })
	}
	closers = append(closers, func() { _ = service.Close() })
	return service, cleanup, nil
}

func dhcpLocalRecords(service *dhcp.Service) *dhcpdns.Resolver {
	stores := map[string]*dhcp.LeaseStore{}
	if service != nil {
		for _, name := range service.ScopeNames() {
			if store, err := service.Scope(name); err == nil {
				stores[name] = store
			}
		}
	}
	return dhcpdns.New(stores)
}
