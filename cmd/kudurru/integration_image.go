package main

import (
	"context"
	"encoding/json"
	"net"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/ee/integration/inbound"
	"github.com/robertherbaugh/kudurru/internal/inventory"
	"github.com/robertherbaugh/kudurru/internal/inventory/persistence"
	"github.com/robertherbaugh/kudurru/internal/policy"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

// prepareIntegrationImage is pure complete-image assembly. Historical sources
// preserve original expiry/evidence, but only a current same-UID configuration
// and all committed target/record receipts grant live inventory/group claims.
func prepareIntegrationImage(ctx context.Context, image *replication.Image, primary *primaryruntime.PreparedImage, schema *servingbuild.Schema, at time.Time) (*servingbuild.InventoryOwner, []*public.ClientGroup, error) {
	options := schema.PrimaryImageOptions(ctx, image.Term.ClusterID)
	decoded, err := inbound.DecodeImage(image, inbound.ImageOptions{Scheme: options.Scheme, Validation: options.Validation})
	if err != nil {
		return nil, nil, err
	}
	state, err := persistence.Decode(image.Payload.Runtime, image.Term.ClusterID, image.CreatedAt)
	if err != nil {
		return nil, nil, err
	}
	objects := map[store.Key]store.Object{}
	heads := map[zk.ZoneID]*zk.ZoneState{}
	chunks := map[string]*zk.ZoneChunk{}
	var groups []*public.ClientGroup
	for _, raw := range image.Payload.Objects {
		var header struct{ Kind string }
		if err = json.Unmarshal(raw, &header); err != nil {
			return nil, nil, err
		}
		obj, err := options.Scheme.New(public.GroupVersion.WithKind(header.Kind))
		if err != nil {
			return nil, nil, err
		}
		if err = json.Unmarshal(raw, obj); err != nil {
			return nil, nil, err
		}
		value := obj.(store.Object)
		objects[store.KeyOf(value)] = value
		switch v := value.(type) {
		case *public.ClientGroup:
			groups = append(groups, v)
		case *zk.ZoneState:
			heads[v.Spec.Zone] = v
		case *zk.ZoneChunk:
			chunks[v.Spec.Digest] = v
		}
	}
	// Only heads selected by this captured primary image can carry live old
	// source work. Historical orphan/superseded heads cannot block settlement.
	type ownedReceipt struct {
		zone     zk.ZoneID
		source   zonestate.Source
		envelope zonestate.SourceEnvelope
	}
	receipts := map[string][]ownedReceipt{}
	if len(decoded.Sources) > 0 {
		for _, version := range primary.Versions() {
			head := heads[version.Zone]
			if head == nil || head.Spec.Lifecycle == zk.LifecycleTombstoned {
				continue
			}
			aggregate, err := zonestate.Decode(head, func(digest string) (*zk.ZoneChunk, error) {
				if c := chunks[digest]; c != nil {
					return c, nil
				}
				return nil, store.ErrNotFound
			}, zonestate.Limits{})
			if err != nil {
				return nil, nil, err
			}
			for _, source := range aggregate.Sources {
				envelope, err := zonestate.DecodeSourceEnvelope(source.Desired)
				if err != nil {
					return nil, nil, err
				}
				if envelope.State == "present" {
					receipts[source.Manager] = append(receipts[source.Manager], ownedReceipt{version.Zone, source, envelope})
				}
			}
		}
	}
	var admissions []inbound.SourceAdmission
	for _, source := range decoded.Sources {
		d, ok := objects[store.Key{Kind: "Integration", Namespace: source.Head.Spec.Owner.Namespace, Name: source.Head.Spec.Owner.Name}].(*public.Integration)
		if !ok || string(d.UID) != source.Head.Spec.Owner.UID || d.Spec.Suspend || !d.DeletionTimestamp.IsZero() {
			continue
		}
		raw, _ := json.Marshal(d.Spec)
		config := kinds.Digest(raw)
		if config != source.Head.Spec.ConfigDigest {
			continue
		}
		document, err := inbound.DecodeSource(source)
		if err != nil {
			return nil, nil, err
		}
		admission := inbound.SourceAdmission{Owner: source.Head.Spec.Owner, ConfigDigest: config, RootDigest: source.Head.Spec.RootDigest, Ready: true, Targets: document.Targets}
		aggregates := map[string]*zonestate.Aggregate{}
		for _, target := range document.Targets {
			z, ok := objects[store.Key{Kind: "DNSZone", Name: target.Object.Name}].(*public.DNSZone)
			if !ok || string(z.UID) != target.Object.UID || public.IsDisabled(z) || !z.DeletionTimestamp.IsZero() {
				admission.Ready = false
				continue
			}
			digest, _ := inbound.IntentDigest(z.Spec)
			observation, present := primary.CommittedPrimaryStatus(z.Name)
			if digest != target.Object.IntentDigest || !present || !observation.ConfigMatchesDesired || !primary.Admitted(target.Apex, at) {
				admission.Ready = false
				continue
			}
			head := heads[observation.HeadZone]
			aggregate, err := zonestate.Decode(head, func(digest string) (*zk.ZoneChunk, error) {
				if c := chunks[digest]; c != nil {
					return c, nil
				}
				return nil, store.ErrNotFound
			}, zonestate.Limits{})
			if err != nil {
				return nil, nil, err
			}
			aggregates[z.Name] = aggregate
		}
		type wantedReceipt struct {
			zone  zk.ZoneID
			token zonestate.SourceToken
		}
		wanted := map[string]wantedReceipt{}
		for _, proof := range document.Objects {
			if proof.Kind != "DNSRecordSet" {
				continue
			}
			record, ok := objects[store.Key{Kind: "DNSRecordSet", Namespace: proof.Namespace, Name: proof.Name}].(*public.DNSRecordSet)
			if !ok || string(record.UID) != proof.UID || !sourceOwned(record, source.Head.Spec.Owner) {
				admission.Ready = false
				continue
			}
			token, err := primaryzone.SourceToken(record)
			if err != nil {
				return nil, nil, err
			}
			if digest, _ := inbound.IntentDigest(record.Spec); digest != proof.IntentDigest {
				admission.Ready = false
				continue
			}
			if observation, ok := primary.CommittedPrimaryStatus(record.Spec.ZoneRef.Name); ok {
				wanted[token.ID] = wantedReceipt{observation.HeadZone, token}
			}
			found := false
			if aggregate := aggregates[record.Spec.ZoneRef.Name]; aggregate != nil {
				for _, receipt := range aggregate.Sources {
					if receipt.ID != token.ID || receipt.UID != token.UID {
						continue
					}
					envelope, err := zonestate.DecodeSourceEnvelope(receipt.Desired)
					if err != nil {
						return nil, nil, err
					}
					found = envelope.State == "present" && envelope.IntentDigest == token.IntentDigest && (envelope.Projection == "served" || envelope.Projection == "viewSpecific")
				}
			}
			if !found {
				admission.Ready = false
			}
		}
		for _, receipt := range receipts[integrationManager(source.Head.Spec.Owner)] {
			expected, ok := wanted[receipt.source.ID]
			if !ok || expected.zone != receipt.zone || expected.token.UID != receipt.source.UID || expected.token.IntentDigest != receipt.envelope.IntentDigest {
				admission.Ready = false
			}
		}
		admissions = append(admissions, admission)
	}
	prepared, err := inbound.Prepare(inbound.ImageInput{ClusterID: image.Term.ClusterID, Sources: decoded.Sources, Admissions: admissions, Inventory: state.State, Groups: groups, At: at})
	if err != nil {
		return nil, nil, err
	}
	holder, err := inventory.RestoreState(prepared.Inventory, func() time.Time { return at })
	if err != nil {
		return nil, nil, err
	}
	owner := &servingbuild.InventoryOwner{Snapshot: holder.Snapshot(), SourceRoots: map[string]string{}}
	for _, source := range prepared.Sources {
		if !source.Eligible {
			continue
		}
		owner.SourceRoots[source.Head.Name] = source.Head.Spec.RootDigest
		deadlines := map[string]time.Time{}
		id := ih.SourceID(source.Head.Spec.Owner)
		for _, client := range source.Document.Snapshot.Clients {
			live := false
			for _, ip := range client.IPs {
				winner := owner.Snapshot.Lookup(ip, at)
				live = live || winner != nil && winner.Source == id && winner.ID == client.ID
			}
			if !live {
				continue
			}
			for _, mac := range client.MACs {
				if client.ExpiresAt.After(deadlines[mac]) {
					deadlines[mac] = client.ExpiresAt
				}
			}
		}
		for _, contribution := range source.Document.Groups {
			group, ok := objects[store.Key{Kind: "ClientGroup", Namespace: contribution.Group.Namespace, Name: contribution.Group.Name}].(*public.ClientGroup)
			if !ok || string(group.UID) != contribution.Group.UID || group.Namespace != source.Head.Spec.Owner.Namespace || group.Spec.Sources == nil || group.Spec.Sources.IntegrationRef == nil {
				continue
			}
			ref := group.Spec.Sources.IntegrationRef
			if ref.Name != source.Head.Spec.Owner.Name || ref.Namespace != "" && ref.Namespace != group.Namespace {
				continue
			}
			manual := map[string]bool{}
			for _, matcher := range group.Spec.Clients {
				if mac, err := net.ParseMAC(matcher.MAC); err == nil {
					manual[mac.String()] = true
				}
			}
			for _, mac := range contribution.MACs {
				if deadlines[mac].IsZero() || manual[mac] {
					continue
				}
				owner.MACGrants = append(owner.MACGrants, policy.MACGrant{Group: group.Namespace + "/" + group.Name, GroupUID: string(group.UID), MAC: mac, Source: id, ExpiresAt: deadlines[mac]})
				if len(owner.MACGrants) > policy.MaxMACGrants {
					return nil, nil, ih.ErrCapacity
				}
			}
		}
	}
	return owner, prepared.Groups, nil
}

// integrationAggregate resolves only the head/chunks from one original image.
func integrationAggregate(image *replication.Image, id zk.ZoneID) (*zk.ZoneState, *zonestate.Aggregate, error) {
	var head *zk.ZoneState
	chunks := map[string]*zk.ZoneChunk{}
	for _, raw := range image.Payload.Objects {
		var header struct{ Kind string }
		if err := json.Unmarshal(raw, &header); err != nil {
			return nil, nil, err
		}
		switch header.Kind {
		case zk.KindZoneState:
			var value zk.ZoneState
			if err := json.Unmarshal(raw, &value); err != nil {
				return nil, nil, err
			}
			if value.Spec.Zone == id {
				head = &value
			}
		case zk.KindZoneChunk:
			var value zk.ZoneChunk
			if err := json.Unmarshal(raw, &value); err != nil {
				return nil, nil, err
			}
			chunks[value.Spec.Digest] = &value
		}
	}
	if head == nil {
		return nil, nil, store.ErrNotFound
	}
	aggregate, err := zonestate.Decode(head, func(digest string) (*zk.ZoneChunk, error) {
		if c := chunks[digest]; c != nil {
			return c, nil
		}
		return nil, store.ErrNotFound
	}, zonestate.Limits{})
	return head, aggregate, err
}
