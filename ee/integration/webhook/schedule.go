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
