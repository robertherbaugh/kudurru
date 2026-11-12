package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/ee/integration/inbound"
	"github.com/robertherbaugh/kudurru/internal/inventory"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// kubeObservationKey carries one source observation's original cut and lease
// from BeforeObserve to CommitSource.
type kubeObservationKey struct{}

type kubeObservation struct {
	guard ih.ExecutionGuard
	cut   *kubeIntegrationCut
	lease context.Context
}

// validateSource is pure: the observation must be complete and its clients
// one valid inventory batch.
func (r *kubeIntegrationRuntime) validateSource(g ih.ExecutionGuard, snapshot ih.SourceSnapshot) error {
	if g.Ledger.Validate() != nil || !snapshot.Complete {
		return ih.ErrInvalid
	}
	verifier := inventory.New(r.opts.Now)
	if err := verifier.Replace(ih.SourceID(g.Ledger.Owner), 1, snapshot.Clients); err != nil {
		return ih.ErrInvalid
	}
	return nil
}

// beforeObserve binds the observation to the host's original request cut and
// lease, after the same current check every effect makes.
func (r *kubeIntegrationRuntime) beforeObserve(ctx, _ context.Context, g ih.ExecutionGuard) (context.Context, func(), error) {
	ctx, lease, original, cut, err := r.requestContext(ctx, g.Ledger)
	if err != nil || original != g {
		return nil, nil, ih.ErrFenced
	}
	r.mu.RLock()
	_, err = r.current(g)
	r.mu.RUnlock()
	if err != nil {
		return nil, nil, err
	}
	return context.WithValue(ctx, kubeObservationKey{}, &kubeObservation{guard: g, cut: cut, lease: lease}), func() {}, nil
}

// commitSource is the Kubernetes inbound commit (F09b-K4, D384). The single
// commit point is the Integration UID's IntegrationSourceHead:
//
//  1. every ClientGroup the observation contributes to is read live and must
//     be the captured incarnation, still naming this Integration;
//  2. the document is encoded and its immutable chunks are staged, each by a
//     fenced create-only write (an identical replay is success);
//  3. inside the host's source scope, after the host's final check and the
//     root's fence, exactly one create or exact-resourceVersion update of the
//     head moves it from sequence n to n+1; a lost CAS writes nothing and
//     leaves the previous source in force;
//  4. the previous head's chunks are deleted by exact UID and resourceVersion.
//
// An observation that would publish DNS objects (UniFi spec.unifi.inventory)
// is refused before any write: source-owned DNS children are not mounted on
// this store, and such instances are refused on their objects. Committed
// means the head CAS succeeded; Published means this replica's source
// runtime installed that root.
func (r *kubeIntegrationRuntime) commitSource(ctx context.Context, g ih.ExecutionGuard, snapshot ih.SourceSnapshot) (*ih.Result, error) {
	obs, ok := ctx.Value(kubeObservationKey{}).(*kubeObservation)
	if !ok || obs == nil || obs.guard != g {
		return unavailableSourceResult(ih.ErrFenced)
	}
	if snapshot.Desired != nil && (len(snapshot.Desired.Zones) != 0 || len(snapshot.Desired.RecordSets) != 0) {
		return unavailableSourceResult(ih.ErrUnsupported)
	}
	capture, err := r.host.SourceCapture(ctx, g)
	if err != nil {
		return unavailableSourceResult(err)
	}
	backend, err := r.opts.Store.IntegrationSourceBackend()
	if err != nil {
		return unavailableSourceResult(err)
	}
	owner := g.Ledger.Owner
	manager := integrationManager(owner)
	doc := inbound.Document{FormatVersion: kinds.FormatVersion, Owner: owner, Snapshot: snapshot}
	if snapshot.Desired != nil {
		for _, group := range snapshot.Desired.ClientGroups {
			contribution, err := r.admitGroup(ctx, obs, owner, group)
			if err != nil {
				return unavailableSourceResult(err)
			}
			doc.Groups = append(doc.Groups, contribution)
		}
	}
	previous, err := backend.Head(ctx, owner)
	absent := errors.Is(err, store.ErrNotFound)
	if err != nil && !absent {
		return unavailableSourceResult(err)
	}
	sequence, expected := uint64(1), ""
	if !absent {
		if previous.Spec.Sequence == math.MaxUint64 {
			return unavailableSourceResult(ih.ErrCapacity)
		}
		sequence, expected = previous.Spec.Sequence+1, previous.ResourceVersion
	}
	encoded, err := inbound.EncodeSource(doc, sequence, g.Ledger.ConfigDigest, g.Ledger.CredentialDigest, r.opts.Now().UTC())
	if err != nil {
		return unavailableSourceResult(ih.ErrInvalid)
	}
	staged := make([]*kinds.IntegrationSourceChunk, 0, len(encoded.Chunks))
	for _, chunk := range encoded.Chunks {
		err := r.host.WithLease(ctx, g.Ledger, func(ctx context.Context) error {
			if err := r.check(ctx, obs.lease, g, obs.cut, false); err != nil {
				return err
			}
			out, err := backend.Stage(ctx, chunk, manager)
			if err == nil {
				staged = append(staged, out)
			}
			return err
		})
		if err != nil {
			return unavailableSourceResult(err)
		}
	}
	var committed *kinds.IntegrationSourceHead
	err = r.host.WithSource(ctx, capture, func(ctx context.Context, final func(context.Context) error) error {
		if err := final(ctx); err != nil {
			return err
		}
		if err := r.check(ctx, obs.lease, g, obs.cut, false); err != nil {
			return err
		}
		var e error
		committed, e = backend.Commit(ctx, expected, encoded.Head, staged, manager)
		return e
	})
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			err = ih.ErrFenced
		}
		return unavailableSourceResult(err)
	}
	result := &ih.Result{Clients: len(snapshot.Clients), Source: &ih.SourceOutcome{Committed: true, ReceiptID: committed.Spec.RootDigest}}
	if !absent {
		// The replaced head's chunks are no longer part of any source. Each
		// is deleted by the exact identity read here; a failure leaves an
		// unreferenced chunk, never a broken source.
		for _, ref := range previous.Spec.Chunks {
			var old kinds.IntegrationSourceChunk
			if err := r.opts.Store.Get(ctx, store.Key{Kind: kinds.KindIntegrationSourceChunk, Name: ref.Name}, &old); err != nil {
				continue
			}
			_ = r.host.WithLease(ctx, g.Ledger, func(ctx context.Context) error {
				if err := r.check(ctx, obs.lease, g, obs.cut, false); err != nil {
					return err
				}
				return backend.DeleteChunk(ctx, &old, manager)
			})
		}
	}
	if r.opts.Sources == nil {
		result.Pending = 1
		return result, nil
	}
	r.opts.Sources.Wake()
	if err := r.opts.Sources.await(ctx, committed.Name, committed.Spec.RootDigest); err != nil {
		result.Pending = 1
		return result, err
	}
	result.Source.Published = true
	return result, nil
}

// admitGroup reads one contributed ClientGroup live. It must be the captured
// incarnation with the same identity (everything but status and
// resourceVersion), and still name this Integration as its source. No write
// authority over the group is inferred: the contribution is evidence in the
// head, and every replica grants it only while the group still matches.
func (r *kubeIntegrationRuntime) admitGroup(ctx context.Context, obs *kubeObservation, owner kinds.Owner, group *public.ClientGroup) (inbound.GroupContribution, error) {
	if group == nil {
		return inbound.GroupContribution{}, ih.ErrInvalid
	}
	original, ok := obs.cut.input.Objects.Get(store.KeyOf(group))
	if !ok {
		return inbound.GroupContribution{}, ih.ErrFenced
	}
	var current public.ClientGroup
	if err := r.opts.Store.Get(ctx, store.KeyOf(group), &current); err != nil {
		return inbound.GroupContribution{}, ih.ErrFenced
	}
	ref := current.Spec.Sources
	if current.UID != original.GetUID() || kubeGroupIdentity(&current) != kubeGroupIdentity(original) || ref == nil || ref.IntegrationRef == nil || ref.IntegrationRef.Name != owner.Name || ref.IntegrationRef.Namespace != "" && ref.IntegrationRef.Namespace != owner.Namespace || current.Namespace != owner.Namespace {
		return inbound.GroupContribution{}, ih.ErrFenced
	}
	contribution := inbound.GroupContribution{Group: evidence(&current, current.Spec)}
	for _, m := range group.Spec.Clients {
		contribution.MACs = append(contribution.MACs, m.MAC)
	}
	return contribution, nil
}

// kubeGroupIdentity digests a ClientGroup without its status,
// resourceVersion and managed fields, as kubeSourceIdentity does for sources.
func kubeGroupIdentity(obj store.Object) string {
	g, ok := obj.(*public.ClientGroup)
	if !ok || g == nil {
		return ""
	}
	c := g.DeepCopy()
	c.ResourceVersion, c.ManagedFields, c.Status = "", nil, public.ClientGroupStatus{}
	raw, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return kinds.Digest(raw)
}
