package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/inventory"
	"github.com/robertherbaugh/kudurru/internal/inventory/persistence"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

// integrationScanLimit bounds one kind scan of a terminating command. The
// largest private kind is capped far below it (50,000 name allocations).
const integrationScanLimit = 200000

// integrationScan visits every stored object of one kind inside a command,
// in bounded pages. Keyset pagination keeps it stable under the deletes the
// same command stages afterwards.
func integrationScan(ctx context.Context, tx *sqlite.CommandTx, kind store.Kind, visit func(store.Object) error) error {
	continuation := ""
	for scanned := 0; ; {
		page, err := tx.List(ctx, kind, store.ListOptions{Limit: 512, Continue: continuation})
		if err != nil {
			return err
		}
		for _, obj := range page.Items {
			if scanned++; scanned > integrationScanLimit {
				return ih.ErrCapacity
			}
			if err := visit(obj); err != nil {
				return err
			}
		}
		if page.Continue == "" || page.Continue == continuation {
			return nil
		}
		continuation = page.Continue
	}
}

// integrationState is everything stored for one Integration incarnation
// besides the object itself and the shared request budget, as one command
// reads it: the publication ledger, the source carriers, and the public
// objects its source owns.
type integrationState struct {
	ledgers, operations              int
	sources, allocations, deliveries []store.Object
	objects                          []store.Object
}

func (s integrationState) local() int {
	return len(s.sources) + len(s.allocations) + len(s.deliveries) + len(s.objects)
}

func (s integrationState) String() string {
	return fmt.Sprintf("%d ledger rows, %d operations, %d source rows, %d name allocations, %d deliveries and %d source-owned objects", s.ledgers, s.operations, len(s.sources), len(s.allocations), len(s.deliveries), len(s.objects))
}

// readIntegrationState scans by exact Owner, which includes the object UID:
// a recreated Integration of the same name owns none of these rows.
func readIntegrationState(ctx context.Context, tx *sqlite.CommandTx, owner kinds.Owner) (integrationState, error) {
	var out integrationState
	type step struct {
		kind  store.Kind
		visit func(store.Object) error
	}
	steps := []step{
		{kinds.KindIntegrationLedger, func(obj store.Object) error {
			if row, ok := obj.(*kinds.IntegrationLedger); !ok {
				return ih.ErrInvalid
			} else if row.Spec.Record.Owner == owner {
				out.ledgers++
			}
			return nil
		}},
		{kinds.KindIntegrationOperation, func(obj store.Object) error {
			if row, ok := obj.(*kinds.IntegrationOperation); !ok {
				return ih.ErrInvalid
			} else if row.Spec.Record.Owner == owner {
				out.operations++
			}
			return nil
		}},
		// Chunks precede their head, as a source commit deletes them.
		{kinds.KindIntegrationSourceChunk, func(obj store.Object) error {
			if row, ok := obj.(*kinds.IntegrationSourceChunk); !ok {
				return ih.ErrInvalid
			} else if row.Spec.Owner == owner {
				out.sources = append(out.sources, obj)
			}
			return nil
		}},
		{kinds.KindIntegrationSourceHead, func(obj store.Object) error {
			if row, ok := obj.(*kinds.IntegrationSourceHead); !ok {
				return ih.ErrInvalid
			} else if row.Spec.Owner == owner {
				out.sources = append(out.sources, obj)
			}
			return nil
		}},
		{kinds.KindIntegrationNameAllocation, func(obj store.Object) error {
			if row, ok := obj.(*kinds.IntegrationNameAllocation); !ok {
				return ih.ErrInvalid
			} else if row.Spec.Owner == owner {
				out.allocations = append(out.allocations, obj)
			}
			return nil
		}},
		{kinds.KindIntegrationDelivery, func(obj store.Object) error {
			if row, ok := obj.(*kinds.IntegrationDelivery); !ok {
				return ih.ErrInvalid
			} else if row.Spec.Owner == owner {
				out.deliveries = append(out.deliveries, obj)
			}
			return nil
		}},
	}
	// Record sets precede the zones they belong to.
	for _, kind := range []store.Kind{"DNSRecordSet", "DNSZone"} {
		steps = append(steps, step{kind, func(obj store.Object) error {
			if sourceOwned(obj, owner) {
				out.objects = append(out.objects, obj)
			}
			return nil
		}})
	}
	for _, step := range steps {
		if err := integrationScan(ctx, tx, step.kind, step.visit); err != nil {
			return integrationState{}, err
		}
	}
	return out, nil
}

// purge is the local half of a Delete cleanup (D367). ONE command removes
// everything this Integration incarnation's source stored locally: the
// record sets and zones it owns, its source head and chunks, its name
// allocations and its deliveries, and it withdraws the source's clients from
// the global inventory. A complete image must never hold a head without its
// chunks or allocation evidence without its allocation, so there is no
// partial purge: every delete names the exact UID and resource version read
// in this transaction, a public object that changed since the captured cut
// fences the command, and any refusal rolls all of it back. The publication
// ledger and the shared request budget are not touched here.
//
// It returns once the published serving state shows none of it, so a
// finalized Integration never leaves records answering. A second purge finds
// nothing, writes nothing and only waits for that publication.
func (r *integrationRuntime) purge(ctx context.Context, g ih.ExecutionGuard) (ih.PurgeResult, error) {
	if g.Mode != ih.GuardCleanup {
		return ih.PurgeResult{}, ih.ErrFenced
	}
	bound, original, err := r.requestContext(ctx, g.Ledger)
	if err != nil || original != g {
		return ih.PurgeResult{}, ih.ErrFenced
	}
	ctx = bound
	owner := g.Ledger.Owner
	var out ih.PurgeResult
	held := false
	err = r.host.WithLease(ctx, g.Ledger, func(ctx context.Context) error {
		return r.command(ctx, g, func(ctx context.Context, tx *sqlite.CommandTx, c *integrationCut) error {
			out, held = ih.PurgeResult{}, false
			state, err := readIntegrationState(ctx, tx, owner)
			if err != nil {
				return err
			}
			for _, obj := range state.objects {
				captured, present := c.input.Objects.Get(store.KeyOf(obj))
				if !present || captured.GetUID() != obj.GetUID() || captured.GetResourceVersion() != obj.GetResourceVersion() {
					return ih.ErrFenced
				}
			}
			for _, group := range []struct {
				rows  []store.Object
				count *int
			}{{state.objects, &out.Objects}, {state.sources, &out.SourceRows}, {state.allocations, &out.Allocations}, {state.deliveries, &out.Deliveries}} {
				for _, obj := range group.rows {
					if len(obj.GetFinalizers()) > 0 {
						// A delete only marks such an object; whoever set its
						// finalizer removes it. It is not purged, and one
						// that is already marked is left alone.
						held = true
						if obj.GetDeletionTimestamp() != nil {
							continue
						}
					} else {
						*group.count++
					}
					// The manager is the one that wrote the row; the exact
					// identity is the one this transaction read.
					if err := tx.Delete(ctx, store.KeyOf(obj), store.DeleteOptions{Manager: store.ManagedBy(obj), ExpectedUID: string(obj.GetUID()), ExpectedResourceVersion: obj.GetResourceVersion()}); err != nil {
						return err
					}
				}
			}
			at := r.s.opts.Now().UTC()
			saved, err := persistence.Load(ctx, tx, owner.ClusterID, at)
			if err != nil && !errors.Is(err, persistence.ErrUninitialized) {
				return err
			}
			if err == nil {
				id := ih.SourceID(owner)
				for _, source := range saved.State.Sources {
					if source.ID != id || len(source.Clients) == 0 {
						continue
					}
					if source.Revision == math.MaxUint64 {
						return ih.ErrCapacity
					}
					// An emptied batch keeps the removal watermarks, so a
					// withdrawn address cannot resurrect behind a newer claim.
					next, err := inventory.PrepareReplacement(saved.State, id, source.Revision+1, nil, at)
					if err != nil {
						return err
					}
					if _, err = persistence.Stage(ctx, tx, saved.Version, next, at); err != nil {
						return err
					}
					out.Clients = len(source.Clients)
				}
			}
			if out == (ih.PurgeResult{}) {
				return nil
			}
			key := store.Key{Kind: "Integration", Namespace: owner.Namespace, Name: owner.Name}
			return tx.RecordAudit(ctx, sqlite.AuditEntry{Action: "integration.purge", Key: key, ResourceVersion: g.Ledger.IntegrationResourceVersion, Manager: integrationManager(owner), Reason: fmt.Sprintf("deletionPolicy Delete: removed %d source-owned objects, %d source rows, %d name allocations and %d deliveries of uid %s and withdrew %d inventory clients", out.Objects, out.SourceRows, out.Allocations, out.Deliveries, owner.UID, out.Clients)})
		})
	})
	if err != nil {
		return ih.PurgeResult{}, err
	}
	r.s.publisher.Wake()
	if held {
		// Something of the source waits for another finalizer. Publication
		// cannot show it gone, and the instance is not clean.
		return out, ih.ErrRemains
	}
	return out, r.awaitPurged(ctx, owner)
}

// awaitPurged waits until the published bundle carries no object, source
// root or inventory client of the purged source.
func (r *integrationRuntime) awaitPurged(ctx context.Context, owner kinds.Owner) error {
	timer := time.NewTicker(10 * time.Millisecond)
	defer timer.Stop()
	id, head := ih.SourceID(owner), kinds.SourceHeadName(owner)
	for {
		changed := r.s.publisher.Subscribe()
		lease, err := r.s.publisher.CaptureHTTP(ctx)
		if err == nil {
			purged := true
			if value, ok := lease.Bundle().Component("objects"); ok {
				if snapshot, ok := value.(*zonecache.Snapshot); ok && snapshot != nil {
					for _, kind := range []store.Kind{"DNSRecordSet", "DNSZone"} {
						for _, obj := range snapshot.Objects.List(kind) {
							purged = purged && !sourceOwned(obj, owner)
						}
					}
				}
			}
			if value, ok := lease.Bundle().Component("inventory"); ok {
				if published, ok := value.(*servingbuild.InventoryOwner); ok && published != nil {
					if _, live := published.SourceRoots[head]; live {
						purged = false
					}
					if published.Snapshot != nil && purged {
						for _, client := range published.Snapshot.Clients() {
							purged = purged && client.Source != id
						}
					}
				}
			}
			lease.Release()
			if purged {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		case <-timer.C:
		}
	}
}

// finalize removes the reserved cleanup finalizer of a terminating
// Integration, which removes the object, in ONE command with its checks and
// its audit summary (D367).
//
// Under GuardCleanup the command first verifies, in the same transaction,
// that nothing of the incarnation is left: no ledger row, no operation, no
// source head or chunk, no name allocation, no delivery and no source-owned
// object. Anything left is ErrRemains and the finalizer stays. The shared
// request budget is not the instance's and is never counted or removed.
//
// Under GuardRelease the object carries the finalizer although its policy is
// no longer Delete. The command removes it without any cleanup, provider
// call or license, and records what it leaves behind.
//
// Both bind the removal to the UID and resource version of the guard: a
// changed object is a conflict, never a retry against the newer one.
func (r *integrationRuntime) finalize(ctx context.Context, g ih.ExecutionGuard, summary ih.CleanupSummary) error {
	if g.Mode != ih.GuardCleanup && g.Mode != ih.GuardRelease || summary.Released != (g.Mode == ih.GuardRelease) {
		return ih.ErrFenced
	}
	bound, original, err := r.requestContext(ctx, g.Ledger)
	if err != nil || original != g {
		return ih.ErrFenced
	}
	ctx = bound
	owner := g.Ledger.Owner
	key := store.Key{Kind: "Integration", Namespace: owner.Namespace, Name: owner.Name}
	err = r.host.WithLease(ctx, g.Ledger, func(ctx context.Context) error {
		return r.command(ctx, g, func(ctx context.Context, tx *sqlite.CommandTx, c *integrationCut) error {
			state, err := readIntegrationState(ctx, tx, owner)
			if err != nil {
				return err
			}
			action, reason := "integration.cleanup", fmt.Sprintf("deletionPolicy Delete: cleanup finished and nothing of uid %s remains; removed %s. This lease deleted %d provider record sets, forgot %d ledger rows and %d operations, and purged %d source-owned objects, %d source rows, %d name allocations and %d deliveries in %d runs", owner.UID, store.IntegrationCleanupFinalizer, summary.RecordSetsDeleted, summary.RecordsForgotten, summary.OperationsForgotten, summary.Purged.Objects, summary.Purged.SourceRows, summary.Purged.Allocations, summary.Purged.Deliveries, summary.Runs)
			if g.Mode == ih.GuardRelease {
				action, reason = "integration.release", fmt.Sprintf("released without cleanup: deletionPolicy is %s; removed %s. Retained for uid %s: %s, and every record it published at its provider", c.instances[owner].Definition().Spec.DeletionPolicy, store.IntegrationCleanupFinalizer, owner.UID, state)
			} else if state.ledgers+state.operations+state.local() != 0 {
				return ih.ErrRemains
			}
			removed, err := tx.UpdateFinalizer(ctx, key, store.FinalizerOptions{Finalizer: store.IntegrationCleanupFinalizer, Remove: true, ExpectedUID: owner.UID, ExpectedResourceVersion: g.Ledger.IntegrationResourceVersion})
			if err != nil {
				return err
			}
			return tx.RecordAudit(ctx, sqlite.AuditEntry{Action: action, Key: key, ResourceVersion: removed.GetResourceVersion(), Manager: integrationManager(owner), Reason: reason})
		})
	})
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			return ih.ErrFenced
		}
		return err
	}
	r.s.publisher.Wake()
	r.host.Wake()
	return nil
}
