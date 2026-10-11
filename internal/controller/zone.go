package controller

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// NextSerial is the serial rule of decision D12: max(prev+1, unix_now),
// where max and +1 are RFC 1982 serial arithmetic on 32 bits, so a serial
// at 2^32-1 wraps instead of sticking. Unix time fits until 2106.
func NextSerial(prev uint32, now time.Time) uint32 {
	candidate := prev + 1 // wraps
	u := now.Unix()
	if u <= 0 {
		return candidate
	}
	unix := uint32(uint64(u) & math.MaxUint32)
	if d := unix - candidate; d != 0 && d < 1<<31 {
		return unix // unix is serial-greater than prev+1
	}
	return candidate
}

// reconcileZone computes a DNSZone's status from the record sets that
// reference it. The serial moves only when contentHash changes, so
// re-reconciling identical content never bumps it.
func (c *Controller) reconcileZone(ctx context.Context, key store.Key) (bool, error) {
	cur, ok := c.cache.get(key).(*v1alpha1.DNSZone)
	if !ok || cur == nil {
		return false, nil // deleted
	}
	view := c.cache.view(key.Name, c.opts.Validate)
	now := c.opts.Now()

	next := cur.DeepCopy()
	st := &next.Status
	st.ObservedGeneration = cur.Generation
	st.RecordCount = view.RecordCount
	bumped := st.ContentHash != view.Hash
	if bumped {
		st.Serial = NextSerial(cur.Status.Serial, now)
		st.ContentHash = view.Hash
	}
	v := zoneVerdict(cur, view)
	v.observedGen, v.transitionNow = cur.Generation, now
	v.apply(&st.Conditions)

	if equality.Semantic.DeepEqual(cur.Status, next.Status) {
		return false, nil
	}
	if err := c.writeStatus(ctx, next); err != nil {
		return false, err
	}
	if bumped {
		c.opts.Events.Event(next, EventNormal, EventSerialBumped,
			fmt.Sprintf("serial %d -> %d: content changed (%d records, contentHash %.12s)", cur.Status.Serial, st.Serial, st.RecordCount, st.ContentHash))
	}
	return true, nil
}

// zoneVerdict derives a zone's conditions from its view.
func zoneVerdict(z *v1alpha1.DNSZone, view *zoneView) verdict {
	var v verdict
	switch {
	case view.SelectorErr != nil:
		v = verdict{ReadyReason: v1alpha1.ReasonInvalid, ReadyMessage: "spec.recordSetSelector: " + view.SelectorErr.Error()}
	case z.Spec.Type != v1alpha1.ZoneTypePrimary && z.Spec.Type != v1alpha1.ZoneTypeForward:
		v = verdict{ReadyReason: ReasonUnsupported, ReadyMessage: fmt.Sprintf("zone type %q is not served by this version", z.Spec.Type)}
	default:
		v = serving(fmt.Sprintf("%d records", view.RecordCount))
	}
	// Degraded summarises the record sets that are not fully served.
	type issue struct{ key, reason string }
	var issues []issue
	for k, rv := range view.Verdicts {
		if p := rv.problem(); p != "" {
			issues = append(issues, issue{k.Namespace + "/" + k.Name, p})
		}
	}
	if len(issues) == 0 {
		return v
	}
	slices.SortFunc(issues, func(a, b issue) int { return strings.Compare(a.key, b.key) })
	// The reason is the most actionable problem present.
	reasons := map[string]bool{}
	items := make([]string, 0, len(issues))
	for _, is := range issues {
		reasons[is.reason] = true
		items = append(items, fmt.Sprintf("%s (%s)", is.key, is.reason))
	}
	for _, r := range []string{v1alpha1.ReasonDuplicated, v1alpha1.ReasonOutsideZone, v1alpha1.ReasonInvalid, ReasonNotSelected, ReasonZoneNotFound} {
		if reasons[r] {
			v.DegradedRsn = r
			break
		}
	}
	v.Degraded = true
	v.DegradedMsg = fmt.Sprintf("%d record sets are not fully served: %s", len(issues), joinLimited(items, 10))
	return v
}
