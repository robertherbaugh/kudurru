package sqliteadapter

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

func readLifecycle(ctx context.Context, tx *sqlite.CommandTx, name string) (*zk.ZoneLifecycle, error) {
	var l zk.ZoneLifecycle
	if err := tx.Get(ctx, store.Key{Kind: zk.KindZoneLifecycle, Name: name}, &l); err != nil {
		return nil, err
	}
	if l.Validate() != nil || l.ResourceVersion == "" {
		return nil, pr.ErrInvalid
	}
	return &l, nil
}
func source(ctx context.Context, tx *sqlite.CommandTx, l *zk.ZoneLifecycle) (*public.DNSZone, error) {
	var z public.DNSZone
	err := tx.Get(ctx, store.Key{Kind: "DNSZone", Name: l.Spec.Zone.ObjectName}, &z)
	if l.Spec.Source.Removed {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if string(z.UID) != l.Spec.Zone.UID {
			return nil, nil
		}
		return nil, pr.ErrConflict
	}
	if err != nil {
		return nil, err
	}
	token, err := primaryzone.SourceToken(&z)
	if err != nil {
		return nil, err
	}
	if token.UID != l.Spec.Source.UID || token.ResourceVersion != l.Spec.Source.ResourceVersion || token.IntentDigest != l.Spec.Source.IntentDigest {
		return nil, pr.ErrConflict
	}
	if l.Spec.Phase != zk.PhaseRetiring && l.Spec.Phase != zk.PhaseRetired && (z.Spec.Type != l.Spec.Type || canonical(z.Spec.ZoneName) != l.Spec.Zone.Apex) {
		return nil, pr.ErrConflict
	}
	return &z, nil
}
func (a *Adapter) decode(ctx context.Context, tx *sqlite.CommandTx, head *zk.ZoneState) (*zonestate.View, error) {
	aggregate, err := zonestate.Decode(head, func(d string) (*zk.ZoneChunk, error) {
		var chunk zk.ZoneChunk
		err := tx.Get(ctx, store.Key{Kind: zk.KindZoneChunk, Name: zk.ChunkName(d)}, &chunk)
		return &chunk, err
	}, a.o.Limits)
	if err != nil {
		return nil, err
	}
	return &zonestate.View{Head: head, Aggregate: aggregate, Expected: zonestate.Expected{Zone: head.Spec.Zone, ResourceVersion: head.ResourceVersion, CommitSequence: head.Spec.CommitSequence, RootDigest: head.Spec.RootDigest, AuthEpoch: head.Spec.AuthEpoch, Term: tx.Authority()}}, nil
}
func (a *Adapter) readHead(ctx context.Context, tx *sqlite.CommandTx, id zk.ZoneID) (*zonestate.View, error) {
	var head zk.ZoneState
	err := tx.Get(ctx, store.Key{Kind: zk.KindZoneState, Name: zk.HeadName(id)}, &head)
	if err != nil {
		return &zonestate.View{Expected: zonestate.Expected{Zone: id, Term: tx.Authority()}}, err
	}
	if head.Spec.Zone != id || head.ResourceVersion == "" {
		return nil, pr.ErrInvalid
	}
	return a.decode(ctx, tx, &head)
}

type roster struct {
	ledgers map[string]*zk.ZoneLifecycle
	zones   map[string]*public.DNSZone
}

func (a *Adapter) list(ctx context.Context, tx *sqlite.CommandTx, kind store.Kind, maxItems int, left *int64, each func(store.Object) error) error {
	next := ""
	seen := map[string]bool{}
	count := 0
	for page := 0; page <= maxItems; page++ {
		limit := min(16, maxItems-count)
		if limit <= 0 {
			return pr.ErrCapacity
		}
		items, err := tx.List(ctx, kind, store.ListOptions{Limit: limit, Continue: next})
		if err != nil {
			return err
		}
		if len(items.Items) > limit {
			return pr.ErrCapacity
		}
		for _, obj := range items.Items {
			data, err := json.Marshal(obj)
			if err != nil {
				return err
			}
			*left -= int64(len(data))
			if *left < 0 {
				return pr.ErrCapacity
			}
			if err := each(obj); err != nil {
				return err
			}
			count++
		}
		if items.Continue == "" {
			return nil
		}
		if len(items.Continue) > 4096 || seen[items.Continue] {
			return pr.ErrInvalid
		}
		seen[items.Continue] = true
		next = items.Continue
	}
	return pr.ErrCapacity
}
func (a *Adapter) roster(ctx context.Context, tx *sqlite.CommandTx) (roster, error) {
	out := roster{ledgers: map[string]*zk.ZoneLifecycle{}, zones: map[string]*public.DNSZone{}}
	left := a.o.MaxReadBytes
	err := a.list(ctx, tx, zk.KindZoneLifecycle, a.o.MaxZones, &left, func(obj store.Object) error {
		l, ok := obj.(*zk.ZoneLifecycle)
		if !ok || l == nil || l.Validate() != nil || l.ResourceVersion == "" || out.ledgers[l.Name] != nil || l.Spec.Zone.ClusterID != tx.Authority().ClusterID {
			return pr.ErrInvalid
		}
		out.ledgers[l.Name] = l
		return nil
	})
	if err != nil {
		return out, err
	}
	err = a.list(ctx, tx, "DNSZone", a.o.MaxZones, &left, func(obj store.Object) error {
		z, ok := obj.(*public.DNSZone)
		if !ok || z == nil || z.UID == "" || z.ResourceVersion == "" || out.zones[z.Name] != nil {
			return pr.ErrInvalid
		}
		out.zones[z.Name] = z
		return nil
	})
	return out, err
}
func (a *Adapter) desired(ctx context.Context, tx *sqlite.CommandTx, c pr.Command, l *zk.ZoneLifecycle) error {
	in := c.Desired
	if in == nil || !in.Complete || in.Zone == nil || len(in.RecordSets) > a.o.MaxSources {
		return pr.ErrInvalid
	}
	actual, err := source(ctx, tx, l)
	if err != nil {
		return err
	}
	if actual == nil {
		return pr.ErrConflict
	}
	if !sameSource(actual, in.Zone) || !reflect.DeepEqual(actual.Status.NameserverAddresses, in.Zone.Status.NameserverAddresses) {
		return pr.ErrConflict
	}
	captured := map[store.Key]*public.DNSRecordSet{}
	for _, rs := range in.RecordSets {
		if rs == nil || rs.Spec.ZoneRef.Name != l.Spec.Zone.ObjectName || captured[store.KeyOf(rs)] != nil {
			return pr.ErrInvalid
		}
		captured[store.KeyOf(rs)] = rs
	}
	left := a.o.MaxReadBytes
	seen := map[store.Key]bool{}
	err = a.list(ctx, tx, "DNSRecordSet", a.o.MaxSources, &left, func(obj store.Object) error {
		rs, ok := obj.(*public.DNSRecordSet)
		if !ok || rs == nil {
			return pr.ErrInvalid
		}
		if rs.Spec.ZoneRef.Name != l.Spec.Zone.ObjectName {
			return nil
		}
		key := store.KeyOf(rs)
		if seen[key] || !sameSource(rs, captured[key]) {
			return pr.ErrConflict
		}
		seen[key] = true
		return nil
	})
	if err != nil {
		return err
	}
	if len(seen) != len(captured) {
		return pr.ErrConflict
	}
	// Candidate/no-op policy is checked by the pinned BackgroundGuard in
	// Admission, through the full command boundary.
	return nil
}
func sameSource(a, b store.Object) bool {
	if a == nil || b == nil || reflect.ValueOf(a).IsNil() || reflect.ValueOf(b).IsNil() {
		return false
	}
	at, ae := primaryzone.SourceToken(a)
	bt, be := primaryzone.SourceToken(b)
	return ae == nil && be == nil && at == bt
}
