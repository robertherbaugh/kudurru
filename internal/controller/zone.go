package controller

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
	if cur.Spec.Type == v1alpha1.ZoneTypeSecondary {
		// A shared status cannot attest to every node's independent copy.
		// In particular, never run the primary serial stamper for this type.
		next := cur.DeepCopy()
		next.Status.ObservedGeneration = cur.Generation
		next.Status.Serial, next.Status.RecordCount, next.Status.ContentHash = 0, 0, ""
		meta.SetStatusCondition(&next.Status.Conditions, metav1.Condition{
			Type: v1alpha1.ConditionReady, Status: metav1.ConditionUnknown,
			Reason: "PerNodeTransfer", Message: "Each node transfers independently; inspect that node's secondary status for its served serial and expiry.",
			ObservedGeneration: cur.Generation, LastTransitionTime: metav1.NewTime(c.opts.Now().Truncate(time.Second)),
		})
		meta.RemoveStatusCondition(&next.Status.Conditions, v1alpha1.ConditionDegraded)
		if equality.Semantic.DeepEqual(cur.Status, next.Status) {
			return false, nil
		}
		return true, c.writeStatus(ctx, next)
	}
	view := c.cache.view(key.Name, c.opts.Validate)
	build, prev := c.cache.build(key.Name)
	// Record sets whose zone-model conflicts changed re-check their
	// conditions against this build.
	for _, k := range conflictsChanged(prev, build) {
		c.queues[KindDNSRecordSet].Add(k)
	}
	if !build.NextExpiry.IsZero() {
		// The next expiresAt changes what the zone serves without any
		// object changing: reconcile again then, so the serial moves
		// on time (the build is stale from that instant).
		c.queues[KindDNSZone].AddAfter(key, time.Until(build.NextExpiry))
	}
	now := c.opts.Now()

	next := cur.DeepCopy()
	st := &next.Status
	st.ObservedGeneration = cur.Generation
	st.RecordCount = build.RecordCount
	bumped := st.ContentHash != build.Hash
	if bumped {
		st.Serial = NextSerial(cur.Status.Serial, now)
		st.ContentHash = build.Hash
	}
	v := zoneVerdict(cur, view, build)
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

// zoneVerdict derives a zone's conditions from its view and build.
func zoneVerdict(z *v1alpha1.DNSZone, view *zoneView, build *zoneBuild) verdict {
	var v verdict
	switch {
	case view.SelectorErr != nil:
		v = verdict{ReadyReason: v1alpha1.ReasonInvalid, ReadyMessage: "spec.recordSetSelector: " + view.SelectorErr.Error()}
	case z.Spec.Type != v1alpha1.ZoneTypePrimary && z.Spec.Type != v1alpha1.ZoneTypeForward:
		v = verdict{ReadyReason: ReasonUnsupported, ReadyMessage: fmt.Sprintf("zone type %q is not served by this version", z.Spec.Type)}
	case build.ZoneErr != nil:
		v = verdict{ReadyReason: v1alpha1.ReasonInvalid, ReadyMessage: "the zone is not served: " + build.ZoneErr.Error()}
	default:
		v = serving(fmt.Sprintf("%d records", build.RecordCount))
	}
	// Degraded summarises the record sets that are not fully served.
	type issue struct{ key, reason string }
	var issues []issue
	for k, rv := range view.Verdicts {
		if p := rv.withConflicts(build.Conflicts[k]).problem(); p != "" {
			issues = append(issues, issue{k.Namespace + "/" + k.Name, p})
		}
	}
	if build.PolicyErr != nil {
		v.Degraded, v.DegradedRsn = true, v1alpha1.ReasonInvalid
		v.DegradedMsg = "spec.policy (transfers denied, bad notify targets ignored): " + build.PolicyErr.Error()
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
	for _, r := range []string{v1alpha1.ReasonDuplicated, v1alpha1.ReasonOutsideZone, v1alpha1.ReasonOccluded, v1alpha1.ReasonInvalid, ReasonNotSelected, ReasonZoneNotFound} {
		if reasons[r] {
			v.DegradedRsn = r
			break
		}
	}
	msg := fmt.Sprintf("%d record sets are not fully served: %s", len(issues), joinLimited(items, 10))
	if v.Degraded {
		msg = v.DegradedMsg + "; " + msg
	}
	v.Degraded = true
	v.DegradedMsg = msg
	return v
}
