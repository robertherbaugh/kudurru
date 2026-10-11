package controller

import (
	"context"
	"fmt"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// reconcileBlockList owns observedGeneration and the Ready/Degraded
// conditions of a BlockList. Fetching, digests, entry counts and
// status.sources belong to the listfetcher (§4.6, §7.3); the controller
// only reads status.sources[].error to report Degraded{ListFetchFailed}, so
// one writer owns the conditions and the two never fight over them.
func (c *Controller) reconcileBlockList(ctx context.Context, key store.Key) (bool, error) {
	cur, ok := c.cache.get(key).(*v1alpha1.BlockList)
	if !ok || cur == nil {
		return false, nil
	}
	next := cur.DeepCopy()
	next.Status.ObservedGeneration = cur.Generation
	v := serving(fmt.Sprintf("%d sources", len(cur.Spec.Sources)))
	var failed []string
	for _, s := range cur.Status.Sources {
		if s.Error != "" {
			failed = append(failed, fmt.Sprintf("%s: %s", s.Name, s.Error))
		}
	}
	if len(failed) > 0 {
		v.Degraded, v.DegradedRsn = true, v1alpha1.ReasonFetchFailed
		v.DegradedMsg = joinLimited(failed, 5)
	}
	c.applyEnabled(cur, &v, &next.Status.Conditions)
	return c.writeIfChanged(ctx, &cur.Status, &next.Status, next)
}

// applyEnabled applies v, or the disabled verdict when obj carries
// kudurru.io/enabled: "false": Ready False and Disabled True, both with
// reason Disabled, and Degraded False (switching an object off is not a
// fault). An enabled object carries no Disabled condition.
func (c *Controller) applyEnabled(obj metav1.Object, v *verdict, conds *[]metav1.Condition) {
	v.observedGen, v.transitionNow = obj.GetGeneration(), c.opts.Now()
	if !v1alpha1.IsDisabled(obj) {
		v.apply(conds)
		meta.RemoveStatusCondition(conds, v1alpha1.ConditionDisabled)
		return
	}
	msg := "disabled by the annotation " + v1alpha1.EnabledAnnotation + `: "false"; not applied`
	off := verdict{ReadyReason: v1alpha1.ReasonDisabled, ReadyMessage: msg, observedGen: v.observedGen, transitionNow: v.transitionNow}
	off.apply(conds)
	meta.SetStatusCondition(conds, metav1.Condition{
		Type: v1alpha1.ConditionDisabled, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonDisabled, Message: msg,
		ObservedGeneration: v.observedGen, LastTransitionTime: metav1.NewTime(v.transitionNow.Truncate(time.Second)),
	})
}

// reconcileClientGroup reports the member count. Integration-sourced
// members (spec.sources.integrationRef) arrive with the Integration kind in
// v0.2 and are not validated here yet.
func (c *Controller) reconcileClientGroup(ctx context.Context, key store.Key) (bool, error) {
	cur, ok := c.cache.get(key).(*v1alpha1.ClientGroup)
	if !ok || cur == nil {
		return false, nil
	}
	next := cur.DeepCopy()
	next.Status.ObservedGeneration = cur.Generation
	next.Status.Members = len(cur.Spec.Clients)
	v := serving(fmt.Sprintf("%d client matchers", len(cur.Spec.Clients)))
	c.applyEnabled(cur, &v, &next.Status.Conditions)
	return c.writeIfChanged(ctx, &cur.Status, &next.Status, next)
}

// reconcilePolicy validates a Policy's references within its namespace:
// clientGroupRefs name ClientGroups, blockListRefs and allowListRefs name
// BlockLists. A missing reference is Degraded{MissingReference} naming it;
// the policy stays Ready because the remaining references still apply.
// blockedServices IDs the catalogue does not know (Options.KnownService)
// are Degraded{UnknownService}, unless a reference is also missing, in
// which case the MissingReference message names them too.
func (c *Controller) reconcilePolicy(ctx context.Context, key store.Key) (bool, error) {
	cur, ok := c.cache.get(key).(*v1alpha1.Policy)
	if !ok || cur == nil {
		return false, nil
	}
	var missing, disabled []string
	check := func(kind store.Kind, field string, names []string) {
		for _, n := range names {
			switch o := c.cache.get(store.Key{Kind: kind, Namespace: cur.Namespace, Name: n}); {
			case o == nil:
				missing = append(missing, fmt.Sprintf("%s %q (%s)", kind, n, field))
			case v1alpha1.IsDisabled(o):
				disabled = append(disabled, fmt.Sprintf("%s %q", kind, n))
			}
		}
	}
	check(KindClientGroup, "clientGroupRefs", cur.Spec.ClientGroupRefs)
	check(KindBlockList, "blockListRefs", cur.Spec.BlockListRefs)
	check(KindBlockList, "allowListRefs", cur.Spec.AllowListRefs)
	var unknown []string
	if known := c.opts.KnownService; known != nil {
		for _, id := range cur.Spec.BlockedServices {
			if !known(id) {
				unknown = append(unknown, fmt.Sprintf("service %q (blockedServices)", id))
			}
		}
	}

	next := cur.DeepCopy()
	next.Status.ObservedGeneration = cur.Generation
	v := serving("all references resolved")
	switch {
	case len(missing) > 0:
		v.ReadyMessage = fmt.Sprintf("%d references unresolved", len(missing)+len(unknown))
		v.Degraded, v.DegradedRsn = true, ReasonMissingReference
		v.DegradedMsg = "missing " + joinLimited(append(missing, unknown...), 10)
	case len(unknown) > 0:
		v.ReadyMessage = fmt.Sprintf("%d references unresolved", len(unknown))
		v.Degraded, v.DegradedRsn = true, v1alpha1.ReasonUnknownService
		v.DegradedMsg = "not in the blocked-services catalogue: " + joinLimited(unknown, 10)
	}
	if len(disabled) > 0 {
		v.ReadyMessage += "; skipped (disabled): " + joinLimited(disabled, 10)
	}
	c.applyEnabled(cur, &v, &next.Status.Conditions)
	return c.writeIfChanged(ctx, &cur.Status, &next.Status, next)
}

// policyRefers reports whether p references the ClientGroup or BlockList
// identified by key (same namespace).
func policyRefers(p *v1alpha1.Policy, key store.Key) bool {
	if p.Namespace != key.Namespace {
		return false
	}
	switch key.Kind {
	case KindClientGroup:
		return slices.Contains(p.Spec.ClientGroupRefs, key.Name)
	case KindBlockList:
		return slices.Contains(p.Spec.BlockListRefs, key.Name) || slices.Contains(p.Spec.AllowListRefs, key.Name)
	}
	return false
}

// writeIfChanged writes next when its status differs from the current one.
func (c *Controller) writeIfChanged(ctx context.Context, curStatus, nextStatus any, next store.Object) (bool, error) {
	if equality.Semantic.DeepEqual(curStatus, nextStatus) {
		return false, nil
	}
	if err := c.writeStatus(ctx, next); err != nil {
		return false, err
	}
	return true, nil
}
