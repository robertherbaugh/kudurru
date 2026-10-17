package dhcp

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
)

// ManagedBackend provides typed, serializable mutations. A replicated backend
// must commit each operation through its quorum before returning success.
type ManagedBackend interface {
	Offer(context.Context, Client, netip.Addr) (Lease, error)
	Acknowledge(context.Context, Client, netip.Addr, RequestMode) (Lease, error)
	Release(context.Context, Client, netip.Addr) error
	Decline(context.Context, Client, netip.Addr) error
	QuarantineOffer(context.Context, Client, netip.Addr) error
	Configuration() ManagementState
	UpdateOptions(context.Context, uint64, PoolOptions) error
	PutReservation(context.Context, uint64, Reservation) error
	DeleteReservation(context.Context, uint64, string) error
	Import(context.Context, string, []byte) (ImportResult, error)
}

// ErrReplicatedMutation rejects callbacks that could bypass consensus.
var ErrReplicatedMutation = errors.New("dhcp: replicated scopes require typed management operations")

// SetBackend replaces a scope's allocator before listeners start. The local
// lease store remains a committed read replica for DNS and management reads.
// The replacement must preserve the scope's immutable topology and ownership.
func (s *Service) SetBackend(name string, backend ManagedBackend) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listening || s.running || s.closed {
		return errors.New("dhcp: set backend before starting the service")
	}
	server := s.scopes[name]
	if server == nil || backend == nil {
		return errors.New("dhcp: known scope and backend are required")
	}
	cfg := normalizedConfig(backend.Configuration().Config)
	if !sameTopology(s.topology[name], cfg) {
		return fmt.Errorf("dhcp: backend topology differs for scope %q", name)
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("dhcp: backend configuration for scope %q: %w", name, err)
	}
	if s.backends == nil {
		s.backends = map[string]ManagedBackend{}
	}
	s.backends[name] = backend
	server.leases = backend
	return nil
}

func (s *Service) mutateBackend(ctx context.Context, name string, fn func(ManagedBackend) error) error {
	server := s.scopes[name]
	if server == nil {
		return errors.New("dhcp: unknown scope")
	}
	server.processMu.Lock()
	defer server.processMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	backend := s.backends[name]
	if backend == nil {
		backend = s.stores[name]
	}
	defer func() { server.cfg = normalizedConfig(backend.Configuration().Config) }()
	return fn(backend)
}

// UpdateOptions changes pool settings with an optimistic configuration revision.
func (s *Service) UpdateOptions(ctx context.Context, name string, revision uint64, options PoolOptions) error {
	return s.mutateBackend(ctx, name, func(b ManagedBackend) error { return b.UpdateOptions(ctx, revision, options) })
}

// PutReservation creates or replaces a reservation through the active backend.
func (s *Service) PutReservation(ctx context.Context, name string, revision uint64, reservation Reservation) error {
	return s.mutateBackend(ctx, name, func(b ManagedBackend) error { return b.PutReservation(ctx, revision, reservation) })
}

// DeleteReservation removes a reservation without changing live lease ownership.
func (s *Service) DeleteReservation(ctx context.Context, name string, revision uint64, mac string) error {
	return s.mutateBackend(ctx, name, func(b ManagedBackend) error { return b.DeleteReservation(ctx, revision, mac) })
}

// Import atomically merges lease state through the active backend.
func (s *Service) Import(ctx context.Context, name, format string, data []byte) (ImportResult, error) {
	var result ImportResult
	err := s.mutateBackend(ctx, name, func(b ManagedBackend) error { var err error; result, err = b.Import(ctx, format, data); return err })
	return result, err
}
