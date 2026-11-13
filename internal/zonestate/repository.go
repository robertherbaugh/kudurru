package zonestate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	zonekinds "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// Repository verifies complete generations and performs one fenced head CAS.
type Repository struct {
	backend Backend
	limits  Limits
}

// NewRepository constructs a repository using finite codec limits.
func NewRepository(backend Backend, l Limits) *Repository {
	return &Repository{backend: backend, limits: l.Defaults()}
}

// readAttempts bounds how often Read starts again because the head it read
// was replaced, and its chunks collected, before it resolved them (D405).
const readAttempts = 16

// Read obtains one consistent head followed by immutable data. A concurrent
// new head cannot mix generations. The returned value is independently owned.
// NotFound returns a View with create-only Expected and the current HA term;
// it means only that the zone has no head, never that a chunk is missing.
//
// The head and its chunks are separate reads. A content commit of the same
// zone in between moves the head and deletes the chunks the old one named
// (D405), so a missing chunk is checked against the head again: if the head
// moved, Read starts over with the new one; if it did not, the stored zone is
// damaged and Read answers ErrInvalid. A head that keeps moving for
// readAttempts reads answers ErrConflict.
func (r *Repository) Read(ctx context.Context, id zonekinds.ZoneID) (*View, error) {
	if err := validateZone(id); err != nil {
		return nil, err
	}
	for attempt := 1; ; attempt++ {
		v, head, err := r.read(ctx, id)
		if err == nil || head == nil || !errors.Is(err, store.ErrNotFound) {
			return v, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		again, _, againErr := r.backend.Head(ctx, id)
		switch {
		case againErr != nil && !errors.Is(againErr, store.ErrNotFound):
			return nil, againErr
		case againErr == nil && again != nil && again.ResourceVersion == head.ResourceVersion:
			return nil, fmt.Errorf("%w: zone head %s/%s names a chunk that is not stored (%s)", ErrInvalid, id.ObjectName, head.ResourceVersion, err.Error())
		case attempt >= readAttempts:
			return nil, fmt.Errorf("%w: zone head %s moved during %d reads", ErrConflict, id.ObjectName, attempt)
		}
	}
}

// read is one attempt of Read. It returns the head it read when resolving the
// head's chunks failed, so that Read can tell a replaced head from a damaged one.
func (r *Repository) read(ctx context.Context, id zonekinds.ZoneID) (*View, *zonekinds.ZoneState, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	head, term, err := r.backend.Head(ctx, id)
	v := &View{Head: head, Expected: Expected{Zone: id, Term: term}}
	if err != nil {
		return v, nil, err
	}
	if head == nil || head.Spec.Zone != id || head.ResourceVersion == "" {
		return nil, nil, ErrInvalid
	}
	a, err := Decode(head, func(d string) (*zonekinds.ZoneChunk, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return r.backend.Chunk(ctx, d)
	}, r.limits)
	if err != nil {
		return nil, head, err
	}
	v.Aggregate = a
	v.Expected.ResourceVersion = head.ResourceVersion
	v.Expected.CommitSequence = head.Spec.CommitSequence
	v.Expected.RootDigest = head.Spec.RootDigest
	v.Expected.AuthEpoch = head.Spec.AuthEpoch
	return v, nil, nil
}

// Commit validates and stages the entire immutable root before performing
// exactly one CAS. Preparations may leak after a conflict/crash; publication
// and serial cannot. No retries or stale resource-version substitution occur.
func (r *Repository) Commit(ctx context.Context, expected Expected, p *Prepared, guard Guard) (*View, error) {
	if _, err := VerifyPrepared(p, r.limits); err != nil {
		return nil, err
	}
	if err := CheckTransition(nil, expected, p.Head); err != nil && expected.ResourceVersion == "" {
		return nil, err
	}
	for _, c := range p.Chunks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := r.backend.Stage(ctx, c.DeepCopy()); err != nil {
			return nil, err
		}
	}
	head, err := r.backend.Commit(ctx, expected, p, guard)
	if err != nil {
		return nil, err
	}
	a, err := Decode(head, func(d string) (*zonekinds.ZoneChunk, error) {
		for _, c := range p.Chunks {
			if c.Spec.Digest == d {
				return c, nil
			}
		}
		return nil, store.ErrNotFound
	}, r.limits)
	if err != nil {
		return nil, err
	}
	return &View{Head: head, Aggregate: a, Expected: Expected{Zone: head.Spec.Zone, ResourceVersion: head.ResourceVersion, CommitSequence: head.Spec.CommitSequence, RootDigest: head.Spec.RootDigest, AuthEpoch: head.Spec.AuthEpoch, Term: expected.Term}}, nil
}

// VerifyPrepared independently validates all candidate chunks and head fields.
func VerifyPrepared(p *Prepared, l Limits) (*Aggregate, error) {
	l = l.Defaults()
	if p == nil || p.Head == nil || len(p.Chunks) > l.MaxChunks {
		return nil, ErrInvalid
	}
	encoded, err := json.Marshal(p.Head)
	if err != nil {
		return nil, err
	}
	if len(encoded) > zonekinds.MaxHeadBytes {
		return nil, ErrCapacity
	}
	chunks := map[string]*zonekinds.ZoneChunk{}
	var total int64
	for _, c := range p.Chunks {
		if c == nil || c.Validate() != nil || chunks[c.Spec.Digest] != nil {
			return nil, ErrInvalid
		}
		total += int64(len(c.Spec.Data))
		if total > l.MaxAggregateBytes+zonekinds.MaxChunkBytes {
			return nil, ErrCapacity
		}
		chunks[c.Spec.Digest] = c
	}
	return Decode(p.Head, func(d string) (*zonekinds.ZoneChunk, error) {
		c := chunks[d]
		if c == nil {
			return nil, fmt.Errorf("%w: missing candidate chunk", ErrInvalid)
		}
		return c, nil
	}, l)
}

// CheckTransition is used by BOTH backends after reading the current head at
// their commit boundary. A term fence is additionally enforced by SQLite.
func CheckTransition(current *zonekinds.ZoneState, e Expected, next *zonekinds.ZoneState) error {
	if next == nil || next.Validate() != nil || next.Spec.Zone != e.Zone {
		return ErrInvalid
	}
	if e.ResourceVersion == "" {
		if current != nil {
			return ErrConflict
		}
		if e.CommitSequence != 0 || e.RootDigest != "" || next.Spec.CommitSequence != 1 {
			return ErrInvalid
		}
		return nil
	}
	if current == nil || current.Validate() != nil || current.Spec.Zone != e.Zone || current.ResourceVersion != e.ResourceVersion || current.Spec.CommitSequence != e.CommitSequence || current.Spec.RootDigest != e.RootDigest || current.Spec.AuthEpoch != e.AuthEpoch {
		return ErrConflict
	}
	if current.Spec.Lifecycle != zonekinds.LifecycleActive || e.CommitSequence == math.MaxUint64 || next.Spec.CommitSequence != e.CommitSequence+1 || next.Spec.AuthEpoch < e.AuthEpoch {
		return ErrInvalid
	}
	if next.Spec.AuthDigest != current.Spec.AuthDigest && next.Spec.AuthEpoch <= e.AuthEpoch {
		return ErrInvalid
	}
	changed := next.Spec.ContentHash != current.Spec.ContentHash || next.Spec.Serial != current.Spec.Serial
	if changed {
		if !dynamicupdate.RFC1982Greater(next.Spec.Serial, current.Spec.Serial) || next.Spec.NotifySequence != next.Spec.CommitSequence || next.Spec.NotifySerial != next.Spec.Serial {
			return ErrInvalid
		}
	} else if next.Spec.NotifySequence != current.Spec.NotifySequence || next.Spec.NotifySerial != current.Spec.NotifySerial {
		// Acknowledging durable notification intent is a metadata-only commit.
		if (next.Spec.NotifySequence != 0 || next.Spec.NotifySerial != 0) && !exactNotifyRearm(current, next) {
			return ErrInvalid
		}
	}
	return nil
}
