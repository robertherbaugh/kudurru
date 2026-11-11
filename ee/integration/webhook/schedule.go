package webhook

import (
	"time"

	"github.com/robertherbaugh/kudurru/ee/integration/host"
)

var _ host.ObservationScheduler = (*provider)(nil)

// ObservationSchedule derives inventory pacing from the first accepted peer
// declaration. Actions/hooks alone never schedule an empty inventory poll.
func (p *provider) ObservationSchedule() (time.Duration, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.life.Err() != nil || p.advertisement == nil {
		return 0, host.ErrUnavailable
	}
	if p.changed {
		return 0, host.ErrFenced
	}
	if !p.advertisement.has("clients") {
		return 0, nil
	}
	return time.Minute, nil
}

var _ host.RoleProvider = (*provider)(nil)

// Roles reports what the first accepted peer declaration negotiated (D379).
// One Go type serves every Tier-B capability, so the host must not infer the
// roles from it: a peer may be an inventory source, an event sink, both or
// neither. Records are never a role until remote record ownership exists.
func (p *provider) Roles() (host.Roles, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.life.Err() != nil || p.advertisement == nil {
		return host.Roles{}, host.ErrUnavailable
	}
	if p.changed {
		return host.Roles{}, host.ErrFenced
	}
	return host.Roles{Observer: p.advertisement.has("clients"), Hook: p.advertisement.has("hooks") && len(p.hmacKey) != 0}, nil
}
