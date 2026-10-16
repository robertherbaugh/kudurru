package dhcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"slices"
	"time"
)

var (
	// ErrRevisionConflict means another management write changed the scope.
	ErrRevisionConflict = errors.New("dhcp: configuration revision conflict")
	// ErrManagedByFile prevents API writes to file-owned scope settings.
	ErrManagedByFile = errors.New("dhcp: scope configuration is managed by file")
)

// ManagementState is a detached copy of a scope's effective configuration.
type ManagementState struct {
	Config   Config
	Revision uint64
}

// PoolOptions are the mutable options of an explicitly API-managed scope.
// Host topology and management ownership never change through these methods.
type PoolOptions struct {
	RangeStart    netip.Addr    `json:"rangeStart"`
	RangeEnd      netip.Addr    `json:"rangeEnd"`
	Router        netip.Addr    `json:"router"`
	DNSServers    []netip.Addr  `json:"dnsServers"`
	DomainName    string        `json:"domainName"`
	LeaseDuration time.Duration `json:"leaseDuration"`
	RegisterDNS   bool          `json:"registerDNS"`
	DNSTTL        time.Duration `json:"dnsTTL"`
}

func cloneLeaseConfig(cfg Config) Config {
	cfg.DNSServers = slices.Clone(cfg.DNSServers)
	cfg.Reservations = slices.Clone(cfg.Reservations)
	cfg.TrustedRelays = slices.Clone(cfg.TrustedRelays)
	return cfg
}

func normalizeLeaseConfig(cfg Config) Config {
	cfg = cloneLeaseConfig(cfg)
	if cfg.Name == "" {
		cfg.Name = "default"
	}
	if cfg.Management == "" {
		cfg.Management = "file"
	}
	if cfg.DNSTTL == 0 {
		cfg.DNSTTL = 30 * time.Second
	}
	for i := range cfg.Reservations {
		cfg.Reservations[i].MAC, _ = canonicalMAC(cfg.Reservations[i].MAC)
	}
	return cfg
}

func optionsFor(cfg Config) PoolOptions {
	return PoolOptions{RangeStart: cfg.RangeStart, RangeEnd: cfg.RangeEnd, Router: cfg.Router,
		DNSServers: slices.Clone(cfg.DNSServers), DomainName: cfg.DomainName, LeaseDuration: cfg.LeaseDuration,
		RegisterDNS: cfg.RegisterDNS, DNSTTL: cfg.DNSTTL}
}

func applyPoolOptions(cfg Config, p PoolOptions) Config {
	cfg = cloneLeaseConfig(cfg)
	cfg.RangeStart, cfg.RangeEnd, cfg.Router = p.RangeStart, p.RangeEnd, p.Router
	cfg.DNSServers = slices.Clone(p.DNSServers)
	cfg.DomainName, cfg.LeaseDuration = p.DomainName, p.LeaseDuration
	cfg.RegisterDNS, cfg.DNSTTL = p.RegisterDNS, p.DNSTTL
	return normalizeLeaseConfig(cfg)
}

func (s *LeaseStore) setLeaseConfig(cfg Config) {
	s.cfg = cfg
	s.byMAC = make(map[string]Reservation, len(cfg.Reservations))
	s.byIP = make(map[netip.Addr]string, len(cfg.Reservations))
	for _, r := range cfg.Reservations {
		s.byMAC[r.MAC], s.byIP[r.IP] = r, r.MAC
	}
}

// Configuration reads memory only, returning copies of all mutable slices.
func (s *LeaseStore) Configuration() ManagementState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ManagementState{Config: cloneLeaseConfig(s.cfg), Revision: s.revision}
}

func writeLeaseSettings(ctx context.Context, tx *sql.Tx, cfg Config, revision uint64) error {
	opts, err := json.Marshal(optionsFor(cfg))
	if err != nil {
		return err
	}
	reservations, err := json.Marshal(cfg.Reservations)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO lease_settings(singleton, revision, options, reservations)
VALUES (1, ?, ?, ?) ON CONFLICT(singleton) DO UPDATE SET
revision=excluded.revision, options=excluded.options, reservations=excluded.reservations`, revision, string(opts), string(reservations))
	return err
}

// initializeManagement runs in the schema transaction. API scopes seed once;
// later file pool values are defaults, while topology still comes from file.
func (s *LeaseStore) initializeManagement(tx *sql.Tx) (Config, uint64, error) {
	cfg := s.cfg
	if cfg.Management != "api" {
		if s.setup != nil {
			return Config{}, 0, fmt.Errorf("dhcp: UI setup requires management=api")
		}
		return cfg, 1, nil
	}
	var optsJSON, reservationsJSON, token string
	var revision uint64
	err := tx.QueryRow("SELECT revision, options, reservations, setup_token FROM lease_settings WHERE singleton=1").Scan(&revision, &optsJSON, &reservationsJSON, &token)
	if errors.Is(err, sql.ErrNoRows) {
		if s.setup != nil {
			return s.applySetup(tx, 0)
		}
		return cfg, 1, writeLeaseSettings(context.Background(), tx, cfg, 1)
	}
	if err != nil {
		return Config{}, 0, err
	}
	if revision == 0 || revision > math.MaxInt64 {
		return Config{}, 0, fmt.Errorf("dhcp: invalid persisted configuration revision")
	}
	if s.setup != nil && token != s.setup.token {
		return s.applySetup(tx, revision)
	}
	var opts PoolOptions
	var reservations []Reservation
	if err := json.Unmarshal([]byte(optsJSON), &opts); err != nil {
		return Config{}, 0, fmt.Errorf("dhcp: decode persisted options: %w", err)
	}
	if err := json.Unmarshal([]byte(reservationsJSON), &reservations); err != nil {
		return Config{}, 0, fmt.Errorf("dhcp: decode persisted reservations: %w", err)
	}
	cfg = applyPoolOptions(cfg, opts)
	cfg.Reservations = reservations
	if err := cfg.Validate(); err != nil {
		return Config{}, 0, fmt.Errorf("dhcp: persisted configuration: %w", err)
	}
	return normalizeLeaseConfig(cfg), revision, nil
}

func validateLeaseCandidate(cfg Config, leases []storedLease) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	candidate := &LeaseStore{}
	candidate.setLeaseConfig(normalizeLeaseConfig(cfg))
	for _, l := range leases {
		if err := candidate.compatible(l); err != nil {
			return fmt.Errorf("%w: configuration conflicts with live lease %s", ErrUnavailable, l.Address)
		}
	}
	return nil
}

func (s *LeaseStore) manage(ctx context.Context, revision uint64, edit func(Config) (Config, error)) error {
	var candidate Config
	var nextRevision uint64
	return s.transaction(ctx, func(tx *sql.Tx, leases []storedLease, _ time.Time) error {
		if s.cfg.Management != "api" {
			return ErrManagedByFile
		}
		if revision != s.revision {
			return ErrRevisionConflict
		}
		if s.revision >= math.MaxInt64 {
			return fmt.Errorf("dhcp: configuration revision exhausted")
		}
		var err error
		candidate, err = edit(cloneLeaseConfig(s.cfg))
		if err != nil {
			return err
		}
		if err := validateLeaseCandidate(candidate, leases); err != nil {
			return err
		}
		candidate = normalizeLeaseConfig(candidate)
		nextRevision = s.revision + 1
		return writeLeaseSettings(ctx, tx, candidate, nextRevision)
	}, func() {
		s.setLeaseConfig(candidate)
		s.revision = nextRevision
	})
}

// UpdateOptions atomically applies mutable options after checking all live leases.
func (s *LeaseStore) UpdateOptions(ctx context.Context, revision uint64, options PoolOptions) error {
	return s.manage(ctx, revision, func(cfg Config) (Config, error) { return applyPoolOptions(cfg, options), nil })
}

// PutReservation adds or replaces a reservation without invalidating live state.
func (s *LeaseStore) PutReservation(ctx context.Context, revision uint64, reservation Reservation) error {
	mac, err := canonicalMAC(reservation.MAC)
	if err != nil {
		return err
	}
	reservation.MAC = mac
	return s.manage(ctx, revision, func(cfg Config) (Config, error) {
		for i := range cfg.Reservations {
			if cfg.Reservations[i].MAC == mac {
				cfg.Reservations[i] = reservation
				return cfg, nil
			}
		}
		cfg.Reservations = append(cfg.Reservations, reservation)
		return cfg, nil
	})
}

// DeleteReservation removes a reservation only when every live lease stays valid.
func (s *LeaseStore) DeleteReservation(ctx context.Context, revision uint64, mac string) error {
	mac, err := canonicalMAC(mac)
	if err != nil {
		return err
	}
	return s.manage(ctx, revision, func(cfg Config) (Config, error) {
		cfg.Reservations = slices.DeleteFunc(cfg.Reservations, func(r Reservation) bool { return r.MAC == mac })
		return cfg, nil
	})
}
