package main

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/miekg/dns"
	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/ee/integration/inbound"
	"github.com/robertherbaugh/kudurru/ee/integration/unifi"
	"github.com/robertherbaugh/kudurru/internal/inventory"
	"github.com/robertherbaugh/kudurru/internal/inventory/persistence"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

const integrationSourceAnnotation = "kudurru.io/integration-source"

func integrationManager(o kinds.Owner) string { return store.ManagerIntegration(ih.SourceID(o)) }
func evidence(obj store.Object, spec any) inbound.ObjectEvidence {
	digest, _ := inbound.IntentDigest(spec)
	return inbound.ObjectEvidence{Kind: string(store.KindOf(obj)), Namespace: obj.GetNamespace(), Name: obj.GetName(), UID: string(obj.GetUID()), ResourceVersion: obj.GetResourceVersion(), IntentDigest: digest}
}
func integrationObservationFrom(ctx context.Context) (*integrationObservation, error) {
	o, ok := ctx.Value(integrationObservationKey{}).(*integrationObservation)
	if !ok || o == nil {
		return nil, ih.ErrFenced
	}
	return o, nil
}
func (r *integrationRuntime) decodeSources(ctx context.Context, c *integrationCut) (*inbound.DecodedImage, error) {
	o := r.schema.PrimaryImageOptions(ctx, c.image.Term.ClusterID)
	return inbound.DecodeImage(c.image, inbound.ImageOptions{Scheme: o.Scheme, Validation: o.Validation})
}

// allocate plans permanent reservations from the ORIGINAL complete cut; rows
// remain create-only intent until the whole observation's single commit.
func (r *integrationRuntime) allocate(ctx context.Context, owner kinds.Owner, zone *public.DNSZone, requests []unifi.NameRequest) (map[string]string, error) {
	obs, err := integrationObservationFrom(ctx)
	if err != nil {
		return nil, err
	}
	if owner != obs.guard.Ledger.Owner || obs.allocations != nil || obs.used != nil {
		return nil, ih.ErrFenced
	}
	captured, ok := obs.cut.input.Objects.Zone(zone.Name)
	if !ok || captured.UID != zone.UID || captured.ResourceVersion != zone.ResourceVersion {
		return nil, ih.ErrFenced
	}
	decoded, err := r.decodeSources(ctx, obs.cut)
	if err != nil {
		return nil, err
	}
	in := inbound.AllocationInput{Owner: owner, Scope: kinds.AllocationScope{ClusterID: owner.ClusterID, ZoneUID: string(zone.UID), Apex: dns.CanonicalName(zone.Spec.ZoneName)}, Existing: decoded.Allocations, At: r.s.opts.Now()}
	for _, q := range requests {
		in.Requests = append(in.Requests, inbound.NameRequest{ID: q.ID, Base: q.Base})
	}
	for _, rs := range obs.cut.input.Objects.RecordSets() {
		if rs.Spec.ZoneRef.Name != zone.Name || sourceOwned(rs, owner) {
			continue
		}
		for _, e := range rs.Spec.Endpoints {
			in.Reservations = append(in.Reservations, inbound.NameReservation{Name: dns.CanonicalName(e.DNSName), Subtree: e.RecordType == "DNAME" || e.RecordType == "NS"})
		}
	}
	// Dynamic UPDATE/child authorities are also checked under the current source
	// admission below. Stable reservations can never steal foreign desired names.
	result, err := inbound.AllocateNames(in)
	if err != nil {
		return nil, err
	}
	obs.allocations = result.New
	for _, a := range decoded.Allocations {
		if a.Spec.Owner == owner && a.Spec.Scope == in.Scope && result.Labels[a.Spec.ClientID] == a.Spec.Label {
			obs.used = append(obs.used, a.DeepCopy())
		}
	}
	return result.Labels, nil
}
func sourceOwned(obj store.Object, o kinds.Owner) bool {
	return store.ManagedBy(obj) == integrationManager(o) && obj.GetAnnotations()[integrationSourceAnnotation] == ih.SourceID(o)
}
func (r *integrationRuntime) validateSource(g ih.ExecutionGuard, snapshot ih.SourceSnapshot) error {
	if g.Ledger.Validate() != nil || !snapshot.Complete {
		return ih.ErrInvalid
	}
	verifier := inventory.New(r.s.opts.Now)
	if err := verifier.Replace(ih.SourceID(g.Ledger.Owner), 1, snapshot.Clients); err != nil {
		return ih.ErrInvalid
	}
	return nil
}

func (r *integrationRuntime) commitSource(ctx context.Context, g ih.ExecutionGuard, snapshot ih.SourceSnapshot) (*ih.Result, error) {
	obs, err := integrationObservationFrom(ctx)
	if err != nil {
		return unavailableSourceResult(err)
	}
	if obs.guard != g {
		return unavailableSourceResult(ih.ErrFenced)
	}
	capture, err := r.host.SourceCapture(ctx, g)
	if err != nil {
		return unavailableSourceResult(err)
	}
	var root string
	err = r.host.WithSource(ctx, capture, func(ctx context.Context, final func(context.Context) error) error {
		return r.command(ctx, g, func(ctx context.Context, tx *sqlite.CommandTx, c *integrationCut) error {
			if c.instances[g.Ledger.Owner].BundleID() != obs.cut.instances[g.Ledger.Owner].BundleID() {
				return ih.ErrFenced
			}
			if err := tx.RequireCommitCheck(final); err != nil {
				return err
			}
			doc := inbound.Document{FormatVersion: kinds.FormatVersion, Owner: g.Ledger.Owner, Snapshot: snapshot}
			at := r.s.opts.Now().UTC()
			var previous kinds.IntegrationSourceHead
			err := tx.Get(ctx, store.Key{Kind: kinds.KindIntegrationSourceHead, Name: kinds.SourceHeadName(g.Ledger.Owner)}, &previous)
			absent := errors.Is(err, store.ErrNotFound)
			if err != nil && !absent {
				return err
			}
			sequence := uint64(1)
			if !absent {
				if previous.Spec.Sequence == math.MaxUint64 {
					return ih.ErrCapacity
				}
				sequence = previous.Spec.Sequence + 1
			}
			// All exact target/grant and original source comparisons precede mutations.
			if err := r.admitDesired(ctx, tx, obs, &doc); err != nil {
				return err
			}
			for _, a := range obs.allocations {
				out, err := tx.Apply(ctx, a, store.ApplyOptions{Manager: integrationManager(g.Ledger.Owner), CreateOnly: true})
				if err != nil {
					return err
				}
				obs.used = append(obs.used, out.(*kinds.IntegrationNameAllocation))
			}
			for _, a := range obs.used {
				doc.Allocations = append(doc.Allocations, inbound.AllocationEvidence{Object: evidence(a, a.Spec), Spec: a.Spec})
			}
			if err := r.stageDesired(ctx, tx, obs, &doc); err != nil {
				return err
			}
			encoded, err := inbound.EncodeSource(doc, sequence, g.Ledger.ConfigDigest, g.Ledger.CredentialDigest, at)
			if err != nil {
				return err
			}
			for _, chunk := range encoded.Chunks {
				if _, err = tx.Apply(ctx, chunk, store.ApplyOptions{Manager: integrationManager(g.Ledger.Owner), CreateOnly: true}); err != nil {
					return err
				}
			}
			encoded.Head.ResourceVersion = previous.ResourceVersion
			if _, err = tx.Apply(ctx, encoded.Head, store.ApplyOptions{Manager: integrationManager(g.Ledger.Owner), CreateOnly: absent, ExpectedResourceVersion: previous.ResourceVersion}); err != nil {
				return err
			}
			if !absent {
				for _, chunk := range previous.Spec.Chunks {
					if err = tx.Delete(ctx, store.Key{Kind: kinds.KindIntegrationSourceChunk, Name: chunk.Name}, store.DeleteOptions{Manager: integrationManager(g.Ledger.Owner)}); err != nil {
						return err
					}
				}
			}
			state, err := persistence.Load(ctx, tx, g.Ledger.Owner.ClusterID, at)
			if err != nil {
				return err
			}
			next, err := inventory.PrepareReplacement(state.State, ih.SourceID(g.Ledger.Owner), sequence, snapshot.Clients, at)
			if err != nil {
				return err
			}
			if _, err = persistence.Stage(ctx, tx, state.Version, next, at); err != nil {
				return err
			}
			root = encoded.Head.Spec.RootDigest
			return nil
		})
	})
	if err != nil {
		return unavailableSourceResult(err)
	}
	result := &ih.Result{Clients: len(snapshot.Clients), Source: &ih.SourceOutcome{Committed: true, ReceiptID: root}}
	r.s.publisher.Wake()
	if err = r.awaitSource(ctx, g, root); err != nil {
		result.Pending = 1
		return result, err
	}
	result.Source.Published = true
	return result, nil
}

// admitDesired checks original targets and explicit public group grants. No
// broad object-write authority is inferred from having an Integration object.
func (r *integrationRuntime) admitDesired(ctx context.Context, tx *sqlite.CommandTx, obs *integrationObservation, doc *inbound.Document) error {
	if doc.Snapshot.Desired == nil {
		return nil
	}
	targets := map[string]bool{}
	for _, rs := range doc.Snapshot.Desired.RecordSets {
		targets[rs.Spec.ZoneRef.Name] = true
	}
	definition := obs.cut.instances[doc.Owner].Definition()
	if definition.Spec.UniFi == nil {
		return ih.ErrUnsupported
	}
	if config := definition.Spec.UniFi.Inventory; config != nil {
		targets[config.ZoneRef.Name] = true
	}
	generated := map[string]bool{}
	for _, z := range doc.Snapshot.Desired.Zones {
		generated[z.Name] = true
	}
	for name := range targets {
		if generated[name] {
			continue
		}
		z, ok := obs.cut.input.Objects.Zone(name)
		if !ok {
			return ih.ErrFenced
		}
		var current public.DNSZone
		if err := tx.Get(ctx, store.KeyOf(z), &current); err != nil {
			return err
		}
		if current.UID != z.UID || current.ResourceVersion != z.ResourceVersion || current.Spec.Type != public.ZoneTypePrimary || public.IsDisabled(&current) || !current.DeletionTimestamp.IsZero() {
			return ih.ErrFenced
		}
		selector, err := zone.NewRecordSetSelector(&current)
		if err != nil {
			return ih.ErrInvalid
		}
		for _, rs := range doc.Snapshot.Desired.RecordSets {
			if rs.Spec.ZoneRef.Name == name && !selector.Admits(rs) {
				return ih.ErrFenced
			}
		}
		doc.Targets = append(doc.Targets, inbound.TargetEvidence{Object: evidence(&current, current.Spec), Apex: dns.CanonicalName(current.Spec.ZoneName)})
	}
	for _, group := range doc.Snapshot.Desired.ClientGroups {
		original, ok := obs.cut.input.Objects.Get(store.KeyOf(group))
		if !ok {
			return ih.ErrFenced
		}
		var current public.ClientGroup
		if err := tx.Get(ctx, store.KeyOf(group), &current); err != nil {
			return err
		}
		if current.UID != original.GetUID() || current.ResourceVersion != original.GetResourceVersion() || current.Spec.Sources == nil || current.Spec.Sources.IntegrationRef == nil || current.Spec.Sources.IntegrationRef.Name != doc.Owner.Name || current.Spec.Sources.IntegrationRef.Namespace != "" && current.Spec.Sources.IntegrationRef.Namespace != doc.Owner.Namespace {
			return ih.ErrFenced
		}
		contribution := inbound.GroupContribution{Group: evidence(&current, current.Spec)}
		for _, m := range group.Spec.Clients {
			contribution.MACs = append(contribution.MACs, m.MAC)
		}
		doc.Groups = append(doc.Groups, contribution)
	}
	return nil
}

func (r *integrationRuntime) stageDesired(ctx context.Context, tx *sqlite.CommandTx, obs *integrationObservation, doc *inbound.Document) error {
	if doc.Snapshot.Desired == nil {
		return nil
	}
	owner := doc.Owner
	manager := integrationManager(owner)
	keep := map[store.Key]bool{}
	apply := func(obj store.Object) (store.Object, error) {
		key := store.KeyOf(obj)
		keep[key] = true
		// Exact current incarnation is required for updates. The manager string alone
		// never permits same-name recreation to adopt another UID's source objects.
		old := obj.DeepCopyObject().(store.Object)
		err := tx.Get(ctx, key, old)
		absent := errors.Is(err, store.ErrNotFound)
		if err != nil && !absent {
			return nil, err
		}
		captured, present := obs.cut.input.Objects.Get(key)
		if absent && present || !absent && (!present || captured.GetUID() != old.GetUID() || captured.GetResourceVersion() != old.GetResourceVersion() || !sourceOwned(old, owner)) {
			return nil, ih.ErrFenced
		}
		if !absent {
			obj.SetResourceVersion(old.GetResourceVersion())
		}
		out, err := tx.Apply(ctx, obj, store.ApplyOptions{Manager: manager, ExpectedResourceVersion: obj.GetResourceVersion(), CreateOnly: absent})
		return out, err
	}
	for i, z := range doc.Snapshot.Desired.Zones {
		if z.Namespace != "" || z.Spec.Type != public.ZoneTypePrimary {
			return ih.ErrInvalid
		}
		out, err := apply(z)
		if err != nil {
			return err
		}
		current := out.(*public.DNSZone)
		doc.Snapshot.Desired.Zones[i] = current
		proof := evidence(current, current.Spec)
		doc.Objects = append(doc.Objects, proof)
		doc.Targets = append(doc.Targets, inbound.TargetEvidence{Object: proof, Apex: dns.CanonicalName(current.Spec.ZoneName)})
	}
	for i, rs := range doc.Snapshot.Desired.RecordSets {
		out, err := apply(rs)
		if err != nil {
			return err
		}
		current := out.(*public.DNSRecordSet)
		doc.Snapshot.Desired.RecordSets[i] = current
		doc.Objects = append(doc.Objects, evidence(current, current.Spec))
	}
	// Omission withdraws source-owned records. Reverse-zone retirement is left to
	// the existing primary lifecycle after deleting only this source's desired CR.
	for _, kind := range []store.Kind{"DNSRecordSet", "DNSZone"} {
		page, err := tx.List(ctx, kind, store.ListOptions{Limit: kinds.MaxSourceRecordSets + kinds.MaxSourceZones + 1})
		if err != nil {
			return err
		}
		if page.Continue != "" {
			return ih.ErrCapacity
		}
		for _, obj := range page.Items {
			if sourceOwned(obj, owner) && !keep[store.KeyOf(obj)] {
				original, present := obs.cut.input.Objects.Get(store.KeyOf(obj))
				if !present || original.GetUID() != obj.GetUID() || original.GetResourceVersion() != obj.GetResourceVersion() {
					return ih.ErrFenced
				}
				if err = tx.Delete(ctx, store.KeyOf(obj), store.DeleteOptions{Manager: manager}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (r *integrationRuntime) awaitSource(ctx context.Context, g ih.ExecutionGuard, root string) error {
	timer := time.NewTicker(10 * time.Millisecond)
	defer timer.Stop()
	for {
		changed := r.s.publisher.Subscribe()
		lease, err := r.s.publisher.CaptureHTTP(ctx)
		if err == nil {
			value, _ := lease.Bundle().Component("inventory")
			owner, ok := value.(*servingbuild.InventoryOwner)
			published := ok && owner != nil && owner.SourceRoots[kinds.SourceHeadName(g.Ledger.Owner)] == root
			lease.Release()
			if published {
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
