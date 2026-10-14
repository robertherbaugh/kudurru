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
