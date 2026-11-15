package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
	"k8s.io/apimachinery/pkg/util/validation"
)

// IntegrationRelease is the reviewed release command (D367): it ends the
// cleanup of one terminating Integration without finishing it. The operator names
// the exact UID and resource version they reviewed; the command removes the
// reserved cleanup finalizer of exactly that object, which removes it, and
// records an integration.release audit row with what stays behind, in ONE
// current command that re-authorizes the original credential for
// integrations.release.
//
// It is the API form of declaring the terminating object with Retain, for
// the operator who can no longer declare it: on SQLite the only delete path
// removes the document, and a credential that is gone for good, an expired
// license or a blocked cleanup would otherwise hold the object. It therefore
// needs no license, no credential and no provider, makes no provider call
// and does not take the host's per-instance lease. A cleanup run that holds
// that lease is fenced by the removal: each of its later effects re-reads the
// stored object in its own transaction. A provider request already in flight
// may still complete; its ledger row then keeps its pending intent, as for
// any released incarnation. The provider records and the ledger rows of the
// incarnation stay, like under Retain; a recreated Integration has a new UID
// and sees them as another owner's.
//
// Only an object that is being deleted and still carries the finalizer can be
// released, whatever its policy: anything else, or a changed UID or resource
// version, is a conflict and never a retry against the newer object.
func (r *integrationRuntime) IntegrationRelease(ctx context.Context, namespace, name string, review api.IntegrationReview) (*api.IntegrationReleaseResult, error) {
	if ctx == nil || len(validation.IsDNS1123Label(namespace)) != 0 || len(validation.IsDNS1123Subdomain(name)) != 0 || review.UID == "" || len(review.UID) > 128 || review.Revision == "" || len(review.Revision) > 256 || review.Mode != "" || review.PreviewDigest != "" || len(review.Params) != 0 {
		return nil, integrationAdminError(ih.ErrInvalid)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	view, err := servinghttp.Component[*servingauth.View](ctx, "auth")
	if err != nil {
		return nil, integrationAdminError(err)
	}
	raw, err := servinghttp.Credential(ctx)
	if err != nil {
		return nil, integrationAdminError(err)
	}
	attrs := rbac.Attributes{Resource: "integrations", Verb: "release", Namespace: namespace, Name: name, Scope: rbac.Scope{Kind: rbac.ScopeCluster}}
	proof, err := servingauth.CaptureOperation(ctx, view, raw, attrs, r.s.opts.Now())
	if err != nil {
		return nil, integrationAdminError(err)
	}
	objects, err := servinghttp.Component[*zonecache.Snapshot](ctx, "objects")
	if err != nil {
		return nil, integrationAdminError(err)
	}
	key := store.Key{Kind: "Integration", Namespace: namespace, Name: name}
	if original, ok := objects.Objects.Get(key); !ok || !integrationReleasable(original, review) {
		return nil, integrationAdminError(ih.ErrFenced)
	}
	out := &api.IntegrationReleaseResult{}
	err = r.s.currentAuthCommand(ctx, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		if _, err := servingauth.ReauthorizeOperationCurrent(ctx, tx, deps, proof, r.s.opts.Now); err != nil {
			return err
		}
		var stored public.Integration
		if err := tx.Get(ctx, key, &stored); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return ih.ErrFenced
			}
			return err
		}
		if !integrationReleasable(&stored, review) {
			return ih.ErrFenced
		}
		owner := kinds.Owner{ClusterID: r.s.opts.Store.Info().ClusterID, Namespace: stored.Namespace, Name: stored.Name, UID: string(stored.UID)}
		retained := "rows the command did not count"
		out.LedgerRows, out.Operations = -1, -1
		state, err := readIntegrationState(ctx, tx, owner)
		switch {
		case err == nil:
			retained = state.String()
			out.LedgerRows, out.Operations = state.ledgers, state.operations
		case !errors.Is(err, ih.ErrCapacity):
			return err
		}
		removed, err := tx.UpdateFinalizer(ctx, key, store.FinalizerOptions{Finalizer: store.IntegrationCleanupFinalizer, Remove: true, ExpectedUID: review.UID, ExpectedResourceVersion: review.Revision})
		if err != nil {
			return err
		}
		out.Released = store.Terminated(removed)
		policy := stored.Spec.DeletionPolicy
		if policy == "" {
			policy = public.IntegrationDeletionRetain
		}
		return tx.RecordAudit(ctx, sqlite.AuditEntry{Action: "integration.release", Key: key, ResourceVersion: removed.GetResourceVersion(), Manager: store.ManagerAPI, Reason: fmt.Sprintf("released without cleanup by the reviewed release command: deletionPolicy is %s; removed %s. Retained for uid %s: %s, and every record it published at its provider", policy, store.IntegrationCleanupFinalizer, owner.UID, retained)})
	})
	if err != nil {
		if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrTerminating) || errors.Is(err, replication.ErrFenced) {
			err = ih.ErrFenced
		}
		return nil, integrationAdminError(err)
	}
	r.s.publisher.Wake()
	r.host.Wake()
	return out, nil
}

// integrationReleasable reports whether obj is the exact reviewed Integration
// and is held only for its cleanup: being deleted, with the reserved
// finalizer.
func integrationReleasable(obj store.Object, review api.IntegrationReview) bool {
	d, ok := obj.(*public.Integration)
	return ok && d != nil && string(d.UID) == review.UID && d.ResourceVersion == review.Revision && d.DeletionTimestamp != nil && store.HasFinalizer(d, store.IntegrationCleanupFinalizer)
}
