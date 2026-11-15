package webhook

import (
	"slices"
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
// neither. The publisher role needs a records peer and configured domains
// (D380).
//
// A records-only peer without domains has no role at all, and is refused
// here rather than in Check: an ordinary lease asks for roles before it
// schedules anything and ends as ProvisionFailed, while a Delete cleanup,
// which provisions and checks but never schedules or starts, can still
// read the peer and remove the record sets this instance's registry rows
// claim after every domain was removed.
func (p *provider) Roles() (host.Roles, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.life.Err() != nil || p.advertisement == nil {
		return host.Roles{}, host.ErrUnavailable
	}
	if p.changed {
		return host.Roles{}, host.ErrFenced
	}
	if len(p.records.domains) == 0 && slices.Equal(p.advertisement.declaration.Capabilities, []string{"records"}) {
		return host.Roles{}, host.ErrUnsupported
	}
	return host.Roles{Publisher: p.advertisement.has("records") && len(p.records.domains) != 0, Observer: p.advertisement.has("clients"), Hook: p.advertisement.has("hooks") && len(p.hmacKey) != 0}, nil
}
