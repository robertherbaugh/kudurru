package controller

import (
	"context"
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/api/equality"

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
	v.observedGen, v.transitionNow = cur.Generation, c.opts.Now()
	v.apply(&next.Status.Conditions)
	return c.writeIfChanged(ctx, &cur.Status, &next.Status, next)
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
	v.observedGen, v.transitionNow = cur.Generation, c.opts.Now()
	v.apply(&next.Status.Conditions)
	return c.writeIfChanged(ctx, &cur.Status, &next.Status, next)
}

// reconcilePolicy validates a Policy's references within its namespace:
// clientGroupRefs name ClientGroups, blockListRefs and allowListRefs name
// BlockLists. A missing reference is Degraded{MissingReference} naming it;
// the policy stays Ready because the remaining references still apply.
func (c *Controller) reconcilePolicy(ctx context.Context, key store.Key) (bool, error) {
	cur, ok := c.cache.get(key).(*v1alpha1.Policy)
	if !ok || cur == nil {
		return false, nil
	}
	var missing []string
	check := func(kind store.Kind, field string, names []string) {
		for _, n := range names {
			if c.cache.get(store.Key{Kind: kind, Namespace: cur.Namespace, Name: n}) == nil {
				missing = append(missing, fmt.Sprintf("%s %q (%s)", kind, n, field))
			}
		}
	}
	check(KindClientGroup, "clientGroupRefs", cur.Spec.ClientGroupRefs)
	check(KindBlockList, "blockListRefs", cur.Spec.BlockListRefs)
	check(KindBlockList, "allowListRefs", cur.Spec.AllowListRefs)

	next := cur.DeepCopy()
	next.Status.ObservedGeneration = cur.Generation
	v := serving("all references resolved")
	if len(missing) > 0 {
		v.ReadyMessage = fmt.Sprintf("%d references unresolved", len(missing))
		v.Degraded, v.DegradedRsn = true, ReasonMissingReference
		v.DegradedMsg = "missing " + joinLimited(missing, 10)
	}
	v.observedGen, v.transitionNow = cur.Generation, c.opts.Now()
	v.apply(&next.Status.Conditions)
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
