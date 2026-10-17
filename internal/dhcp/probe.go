package dhcp

import (
	"context"
	"errors"
	"net/netip"
)

// addressProbe reports a positive response, not a guarantee that an address is
// unused. It is replaced in unit tests; those tests never open raw sockets.
type addressProbe func(context.Context, Config, netip.Addr) (bool, error)

// offer reserves durably before probing. A conflict is quarantined durably
// before another address is considered. Existing bound leases are not probed:
// their legitimate owner could otherwise be mistaken for an address conflict.
func (s *Server) offer(ctx context.Context, client Client, requested netip.Addr) (Lease, error) {
	for range 3 {
		lease, err := s.leases.Offer(ctx, client, requested)
		if err != nil {
			return Lease{}, err
		}
		if lease.State == "bound" || s.cfg.ConflictDetection == "disabled" {
			return lease, nil
		}
		if !lease.Address.Is4() || !s.cfg.usable(lease.Address) {
			return Lease{}, errors.New("dhcp: invalid probe candidate")
		}
		if s.probe == nil {
			return Lease{}, errors.New("dhcp: conflict probe is not initialized")
		}
		conflict, err := s.probe(ctx, normalizedConfig(s.cfg), lease.Address)
		if err != nil {
			return Lease{}, err
		}
		if !conflict {
			return lease, nil
		}
		quarantine, ok := s.leases.(interface {
			QuarantineOffer(context.Context, Client, netip.Addr) error
		})
		if !ok {
			return Lease{}, errors.New("dhcp: lease backend cannot quarantine conflicts")
		}
		if err := quarantine.QuarantineOffer(ctx, client, lease.Address); err != nil {
			return Lease{}, err
		}
		if s.logger != nil {
			s.logger.Warn("DHCP address conflict quarantined", "scope", s.cfg.Name, "address", lease.Address)
		}
		requested = netip.Addr{}
	}
	return Lease{}, ErrNoAddress
}
