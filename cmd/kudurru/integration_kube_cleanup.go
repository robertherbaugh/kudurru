package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// kubeIntegrationState is what the Kubernetes store holds for one
// Integration incarnation besides the object and the shared request budget,
// as one bounded scan reads it (the counterpart of integrationState).
type kubeIntegrationState struct {
	ledgers, operations []store.Object
	// heads precede chunks: a purge removes the commit point first, so no
	// replica ever reads a head whose chunks are gone.
	heads, chunks, allocations, deliveries []store.Object
	objects                                []store.Object
}

func (s kubeIntegrationState) local() int {
	return len(s.heads) + len(s.chunks) + len(s.allocations) + len(s.deliveries) + len(s.objects)
}

func (s kubeIntegrationState) String() string {
	return fmt.Sprintf("%d ledger rows, %d operations, %d source rows, %d name allocations, %d deliveries and %d source-owned objects", len(s.ledgers), len(s.operations), len(s.heads)+len(s.chunks), len(s.allocations), len(s.deliveries), len(s.objects))
}

// kubeOwner returns the incarnation a private carrier belongs to.
func kubeOwner(obj store.Object) (kinds.Owner, bool) {
	switch o := obj.(type) {
	case *kinds.IntegrationLedger:
		return o.Spec.Record.Owner, true
	case *kinds.IntegrationOperation:
		return o.Spec.Record.Owner, true
	case *kinds.IntegrationSourceHead:
		return o.Spec.Owner, true
	case *kinds.IntegrationSourceChunk:
		return o.Spec.Owner, true
	case *kinds.IntegrationNameAllocation:
		return o.Spec.Owner, true
	case *kinds.IntegrationDelivery:
		return o.Spec.Owner, true
	case *kinds.IntegrationEventCursor:
		return o.Spec.Owner, true
	}
	return kinds.Owner{}, false
}

// readKubeIntegrationState scans every private kind of the incarnation by
// its exact Owner, which includes the UID, in bounded pages, and collects
// the source-owned public objects of the replica's snapshot.
func (r *kubeIntegrationRuntime) readKubeIntegrationState(ctx context.Context, owner kinds.Owner, objects func(store.Kind) []store.Object) (kubeIntegrationState, error) {
	var out kubeIntegrationState
	for _, step := range []struct {
		kind store.Kind
		into *[]store.Object
	}{
		{kinds.KindIntegrationLedger, &out.ledgers},
		{kinds.KindIntegrationOperation, &out.operations},
		{kinds.KindIntegrationSourceHead, &out.heads},
		{kinds.KindIntegrationSourceChunk, &out.chunks},
		{kinds.KindIntegrationNameAllocation, &out.allocations},
		{kinds.KindIntegrationDelivery, &out.deliveries},
		{kinds.KindIntegrationEventCursor, &out.deliveries},
	} {
		continuation, scanned := "", 0
		for {
			page, err := r.opts.Store.List(ctx, step.kind, store.ListOptions{Limit: 500, Continue: continuation})
			if err != nil {
				return kubeIntegrationState{}, err
			}
			for _, obj := range page.Items {
				if scanned++; scanned > integrationScanLimit {
					return kubeIntegrationState{}, ih.ErrCapacity
				}
				row, ok := kubeOwner(obj)
				if !ok {
					return kubeIntegrationState{}, ih.ErrInvalid
				}
				if row == owner {
					*step.into = append(*step.into, obj)
				}
			}
			if page.Continue == "" || page.Continue == continuation {
				break
			}
			continuation = page.Continue
		}
	}
	for _, kind := range []store.Kind{"DNSRecordSet", "DNSZone"} {
		for _, obj := range objects(kind) {
			if sourceOwned(obj, owner) {
				out.objects = append(out.objects, obj)
			}
		}
	}
	return out, nil
}

// snapshotObjects lists one kind of the replica's current snapshot.
func (r *kubeIntegrationRuntime) snapshotObjects(kind store.Kind) []store.Object {
	snap := r.opts.Cache.Snapshot()
	if snap == nil {
		return nil
	}
	return snap.Objects.List(kind)
}

// purge is the local half of a Delete cleanup on Kubernetes (F09b-DEL-8,
// D367, D384). The store has no multi-object transaction, so the purge is a
// sequence of fenced deletes, each by the exact UID and resourceVersion read
// here (D302), in an order where every intermediate state is a valid one:
// source-owned objects first, then the source head (the commit point), then
// its chunks, then name allocations and deliveries. It is idempotent and
// resumable: a purge cut short leaves a suffix of that order, which the next
// run removes. It returns once this replica's source runtime no longer
// serves the head; other replicas converge on their next relist.
func (r *kubeIntegrationRuntime) purge(ctx context.Context, g ih.ExecutionGuard) (ih.PurgeResult, error) {
	if g.Mode != ih.GuardCleanup {
		return ih.PurgeResult{}, ih.ErrFenced
	}
	ctx, lease, original, cut, err := r.requestContext(ctx, g.Ledger)
	if err != nil || original != g {
		return ih.PurgeResult{}, ih.ErrFenced
	}
	owner := g.Ledger.Owner
	state, err := r.readKubeIntegrationState(ctx, owner, r.snapshotObjects)
	if err != nil {
		return ih.PurgeResult{}, err
	}
	var out ih.PurgeResult
	held := false
	for _, group := range []struct {
		rows  []store.Object
		count *int
	}{{state.objects, &out.Objects}, {state.heads, &out.SourceRows}, {state.chunks, &out.SourceRows}, {state.allocations, &out.Allocations}, {state.deliveries, &out.Deliveries}} {
		for _, obj := range group.rows {
			if len(obj.GetFinalizers()) > 0 {
				// Whoever set that finalizer removes the object; it is not purged.
				held = true
				if obj.GetDeletionTimestamp() != nil {
					continue
				}
			}
			err := r.host.WithLease(ctx, g.Ledger, func(ctx context.Context) error {
				if err := r.check(ctx, lease, g, cut, false); err != nil {
					return err
				}
				return r.opts.Store.Delete(ctx, store.KeyOf(obj), store.DeleteOptions{Manager: store.ManagedBy(obj), ExpectedUID: string(obj.GetUID()), ExpectedResourceVersion: obj.GetResourceVersion()})
			})
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return out, err
			}
			if len(obj.GetFinalizers()) == 0 {
				*group.count++
			}
		}
	}
	if out != (ih.PurgeResult{}) {
		r.opts.Logger.Info("integration purge", "integration", owner.Namespace+"/"+owner.Name, "uid", owner.UID, "objects", out.Objects, "sourceRows", out.SourceRows, "allocations", out.Allocations, "deliveries", out.Deliveries)
	}
	if held {
		return out, ih.ErrRemains
	}
	if r.opts.Sources == nil {
		return out, nil
	}
	r.opts.Sources.Wake()
	return out, r.opts.Sources.await(ctx, kinds.SourceHeadName(owner), "")
}

// finalize removes the reserved cleanup finalizer of a terminating
// Integration on Kubernetes, which removes the object (D367). Under
// GuardCleanup it first reads, fenced, that nothing of the incarnation is
// left (ledger rows, operations, source rows, allocations, deliveries,
// source-owned objects); anything left is ErrRemains. Under GuardRelease it
// removes the finalizer without cleanup and says what it leaves. The
// removal is one metadata patch bound to the guard's UID and
// resourceVersion: a changed object is a conflict, never a retry. Kubernetes
// has no kudurru audit table; the summary is a structured log line written
// before the removal.
func (r *kubeIntegrationRuntime) finalize(ctx context.Context, g ih.ExecutionGuard, summary ih.CleanupSummary) error {
	if g.Mode != ih.GuardCleanup && g.Mode != ih.GuardRelease || summary.Released != (g.Mode == ih.GuardRelease) {
		return ih.ErrFenced
	}
	ctx, lease, original, cut, err := r.requestContext(ctx, g.Ledger)
	if err != nil || original != g {
		return ih.ErrFenced
	}
	owner := g.Ledger.Owner
	state, err := r.readKubeIntegrationState(ctx, owner, r.snapshotObjects)
	if err != nil {
		return err
	}
	if g.Mode == ih.GuardCleanup && len(state.ledgers)+len(state.operations)+state.local() != 0 {
		return ih.ErrRemains
	}
	key := store.Key{Kind: "Integration", Namespace: owner.Namespace, Name: owner.Name}
	err = r.host.WithLease(ctx, g.Ledger, func(ctx context.Context) error {
		if err := r.check(ctx, lease, g, cut, false); err != nil {
			return err
		}
		if g.Mode == ih.GuardRelease {
			r.opts.Logger.Info("integration release", "integration", key.Namespace+"/"+key.Name, "uid", owner.UID, "reason", "released without cleanup: deletionPolicy is not Delete; removing "+store.IntegrationCleanupFinalizer, "retained", state.String())
		} else {
			r.opts.Logger.Info("integration cleanup", "integration", key.Namespace+"/"+key.Name, "uid", owner.UID, "reason", "deletionPolicy Delete: cleanup finished and nothing of the incarnation remains; removing "+store.IntegrationCleanupFinalizer, "recordSetsDeleted", summary.RecordSetsDeleted, "recordsForgotten", summary.RecordsForgotten, "operationsForgotten", summary.OperationsForgotten, "purgedSourceRows", summary.Purged.SourceRows, "runs", summary.Runs)
		}
		_, err := r.opts.Store.UpdateFinalizer(ctx, key, store.FinalizerOptions{Finalizer: store.IntegrationCleanupFinalizer, Remove: true, ExpectedUID: owner.UID, ExpectedResourceVersion: g.Ledger.IntegrationResourceVersion})
		return err
	})
	if errors.Is(err, store.ErrConflict) {
		return ih.ErrFenced
	}
	if err == nil {
		r.host.Wake()
	}
	return err
}

// armCleanup is the Kubernetes arming of the reserved cleanup finalizer
// (D367, DEL-8). This store has no kudurru admission, so the root arms it:
// a live Integration in the release namespace that runs here, has
// deletionPolicy Delete and lacks the finalizer gets it by one metadata
// patch bound to the UID and resourceVersion of the snapshot; a live one
// whose policy is no longer Delete is disarmed the same way. Until a fresh
// capture shows the finalizer the host pauses the instance
// (CleanupFinalizerMissing) and every fence refuses it, so nothing is owned
// unarmed. Several replicas may race; the losers' patches conflict and
// change nothing. Arming needs the integrations license; disarming does not.
func (r *kubeIntegrationRuntime) armCleanup(ctx context.Context) {
	snap := r.opts.Cache.Snapshot()
	if snap == nil || !r.opts.Cache.Synced() {
		return
	}
	lc := r.opts.License.Capture()
	licensed := lc.Current() && lc.Status().Allows(license.FeatureIntegrations)
	for _, obj := range snap.Objects.List("Integration") {
		d, ok := obj.(*public.Integration)
		if !ok || !d.DeletionTimestamp.IsZero() || r.refusal(d) != "" {
			continue
		}
		has := store.HasFinalizer(d, store.IntegrationCleanupFinalizer)
		want := d.Spec.DeletionPolicy == public.IntegrationDeletionDelete
		if has == want || want && !licensed {
			continue
		}
		patchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := r.opts.Store.UpdateFinalizer(patchCtx, store.KeyOf(d), store.FinalizerOptions{Finalizer: store.IntegrationCleanupFinalizer, Remove: !want, ExpectedUID: string(d.UID), ExpectedResourceVersion: d.ResourceVersion})
		cancel()
		if err != nil && !errors.Is(err, store.ErrConflict) && !errors.Is(err, store.ErrNotFound) && ctx.Err() == nil {
			r.opts.Logger.Warn("integration cleanup finalizer", "integration", d.Namespace+"/"+d.Name, "arm", want, "error", err)
		}
	}
}
