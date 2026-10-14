package controller

import (
	"context"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// CommittedPrimaryStatus owns the primary status transaction when installed by
// a committed-primary assembly. It must fence the exact desired object,
// lifecycle, head and original complete serving lease through its actual write.
// Pending publication defers without a legacy status write. Reconcile returns
// whether it wrote and a positive delay for bounded pending/expiry rechecks.
// Subscribe returns the current close-and-replace broadcast channel; obtaining
// it and Reconcile must not wait for publication or perform network I/O.
type CommittedPrimaryStatus interface {
	Reconcile(ctx context.Context, cached *v1alpha1.DNSZone) (wrote bool, retryAfter time.Duration, err error)
	Subscribe() <-chan struct{}
}

func (c *Controller) requeueCommittedPrimaries(ctx context.Context, capability CommittedPrimaryStatus) {
	// Subscribe before scanning so a transition racing the scan cannot be lost.
	changed := capability.Subscribe()
	for {
		if ctx.Err() != nil {
			return
		}
		for _, o := range c.cache.list(KindDNSZone, "") {
			if z, ok := o.(*v1alpha1.DNSZone); ok && z.Spec.Type == v1alpha1.ZoneTypePrimary {
				c.queues[KindDNSZone].AddAfter(store.KeyOf(z), c.opts.ZoneCoalesce)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
			changed = capability.Subscribe()
		}
	}
}
