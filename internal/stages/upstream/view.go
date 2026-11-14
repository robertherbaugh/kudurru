package upstream

import (
	"fmt"

	"github.com/robertherbaugh/kudurru/internal/dnssec"
	"github.com/robertherbaugh/kudurru/internal/upstream"
)

// View pins routes, validator, opt-in validation default and cache incarnation.
// Root owns the Set's resource reference through Component.Release. A view is
// immutable and must not outlive its request lease. Construct a fresh view after
// retirement, even when the route/trust bytes return to an earlier value.
type View struct {
	set       *upstream.Set
	validator *dnssec.Validator
	enabled   bool
	epoch     uint64
	current   func() bool
}

// NewView does no publication or resource retirement. current is a required
// cheap atomic semantic-cursor check used to fence old detached cache fills.
func NewView(set *upstream.Set, validator *dnssec.Validator, enabled bool, current func() bool) (*View, error) {
	if set == nil || enabled && validator == nil || current == nil {
		return nil, fmt.Errorf("upstream: view requires routes, enabled trust and current cursor")
	}
	return &View{set: set, validator: validator, enabled: enabled, epoch: nextEpoch.Add(1), current: current}, nil
}

// Status samples local health counters on this view's retained routes. Counters
// can advance during a held lease; they are not frozen publication metadata.
// The current predicate fences cache fills and does not invalidate held reads.
func (v *View) Status() ([]upstream.ServerStatus, error) {
	if v == nil || v.set == nil || v.current == nil || v.epoch == 0 {
		return nil, fmt.Errorf("upstream: status requires captured routes")
	}
	if _, ok := v.set.Group(upstream.DefaultGroup); !ok {
		return nil, fmt.Errorf("upstream: status requires captured default routes")
	}
	return v.set.Status(), nil
}
