package controller

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// reconcileRecordSet sets a DNSRecordSet's conditions from the zone model:
// ZoneNotFound, NotSelected and Invalid keep the whole object out of the
// zone (Ready=False); OutsideZone and Duplicated drop single endpoints
// (Degraded=True, Ready while anything is still served).
func (c *Controller) reconcileRecordSet(ctx context.Context, key store.Key) (bool, error) {
	cur, ok := c.cache.get(key).(*v1alpha1.DNSRecordSet)
	if !ok || cur == nil {
		return false, nil
	}
	view := c.cache.view(cur.Spec.ZoneRef.Name, c.opts.Validate)
	rv := view.Verdicts[key]
	if rv == nil {
		return false, fmt.Errorf("%s is not in the view of zone %q yet", key, cur.Spec.ZoneRef.Name)
	}

	next := cur.DeepCopy()
	next.Status.ObservedGeneration = cur.Generation
	next.Status.Names = rv.Names
	v, segments := recordSetVerdict(view, rv)
	v.observedGen, v.transitionNow = cur.Generation, c.opts.Now()
	v.apply(&next.Status.Conditions)

	if equality.Semantic.DeepEqual(cur.Status, next.Status) {
		return false, nil
	}
	if err := c.writeStatus(ctx, next); err != nil {
		return false, err
	}
	// One Warning per problem whose details are new: a transition, not a
	// repeat on every reconcile.
	old := conditionText(cur.Status.Conditions)
	for _, s := range segments {
		if !strings.Contains(old, s.text) {
			c.opts.Events.Event(next, EventWarning, s.reason, s.text)
		}
	}
	return true, nil
}

// segment is one problem description, also the text of its Event.
type segment struct{ reason, text string }

// recordSetVerdict maps a zone-model verdict to conditions and the problem
// segments their messages are made of.
func recordSetVerdict(view *zoneView, rv *rrsetVerdict) (verdict, []segment) {
	if rv.Blocked != "" {
		v := verdict{ReadyReason: rv.Blocked, ReadyMessage: rv.BlockedMsg}
		var segs []segment
		if rv.Blocked == v1alpha1.ReasonInvalid {
			segs = append(segs, segment{EventInvalid, rv.BlockedMsg})
		}
		return v, segs
	}
	var segs []segment
	if len(rv.Outside) > 0 {
		segs = append(segs, segment{EventOutsideZone, fmt.Sprintf("not in zone %s: %s", view.Origin, joinLimited(rv.Outside, 10))})
	}
	if len(rv.Duplicates) > 0 {
		segs = append(segs, segment{EventDuplicated, "duplicated: " + joinLimited(rv.Duplicates, 10)})
	}
	if len(segs) == 0 {
		return serving(fmt.Sprintf("%d endpoints in zone %s", rv.Kept, view.Origin)), nil
	}
	texts := make([]string, len(segs))
	for i, s := range segs {
		texts[i] = s.text
	}
	msg := strings.Join(texts, "; ")
	reason := rv.problem()
	v := verdict{Degraded: true, DegradedRsn: reason, DegradedMsg: msg}
	if rv.Endpoints > 0 && rv.Kept == 0 {
		v.ReadyReason, v.ReadyMessage = reason, "no endpoint is served: "+msg
	} else {
		v.Ready, v.ReadyReason = true, v1alpha1.ReasonServing
		v.ReadyMessage = fmt.Sprintf("%d of %d endpoints in zone %s", rv.Kept, rv.Endpoints, view.Origin)
	}
	return v, segs
}

// conditionText concatenates the condition messages, for transition
// detection.
func conditionText(conds []metav1.Condition) string {
	var b strings.Builder
	for _, c := range conds {
		b.WriteString(c.Reason)
		b.WriteByte(' ')
		b.WriteString(c.Message)
		b.WriteByte('\n')
	}
	return b.String()
}
