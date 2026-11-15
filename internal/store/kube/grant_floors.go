package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/internal/store"
)

const (
	grantFloorsName                   = "kudurru-grant-floors"
	grantFloorsType corev1.SecretType = "kudurru.io/grant-floors"
	// MaxGrantFloors bounds the persisted revocation floors. Floors for grants
	// no longer granted by any binding are kept; this is a ceiling, not a GC.
	MaxGrantFloors     = 10000
	maxGrantFloorBytes = 1 << 20
	grantFloorAttempts = 5
)

// ErrGrantFloors reports a malformed or oversized persisted floor document.
var ErrGrantFloors = errors.New("store/kube: invalid grant floors")

type grantFloorDocument struct {
	Version int                  `json:"version"`
	Floors  map[string]time.Time `json:"floors"`
}

func decodeGrantFloors(raw []byte) (map[string]time.Time, error) {
	var doc grantFloorDocument
	if len(raw) == 0 || len(raw) > maxGrantFloorBytes || json.Unmarshal(raw, &doc) != nil || doc.Version != 1 || len(doc.Floors) > MaxGrantFloors {
		return nil, ErrGrantFloors
	}
	for grant, at := range doc.Floors {
		if grant == "" || len(grant) > 512 || at.IsZero() {
			return nil, ErrGrantFloors
		}
	}
	if doc.Floors == nil {
		doc.Floors = map[string]time.Time{}
	}
	return doc.Floors, nil
}

func (s *Store) readGrantFloors(ctx context.Context) (*corev1.Secret, map[string]time.Time, error) {
	var current corev1.Secret
	err := s.coreREST.Get().Namespace(s.opts.Namespace).Resource("secrets").Name(grantFloorsName).MaxRetries(0).Do(ctx).Into(&current)
	if apierrors.IsNotFound(err) {
		return nil, map[string]time.Time{}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if current.Type != grantFloorsType || current.UID == "" || current.ResourceVersion == "" || current.DeletionTimestamp != nil || len(current.OwnerReferences) != 0 || len(current.Data) != 1 {
		return nil, nil, ErrGrantFloors
	}
	floors, err := decodeGrantFloors(current.Data["floors"])
	return &current, floors, err
}

// ReadGrantFloors returns the durable RoleBinding revocation floors: for each
// grant, the latest time any replica observed a binding change remove it.
// A missing document is empty. Malformed content refuses rather than resets.
func (s *Store) ReadGrantFloors(ctx context.Context) (map[string]time.Time, error) {
	if s == nil || s.isClosed() {
		return nil, store.ErrClosed
	}
	if ctx == nil {
		return nil, ErrGrantFloors
	}
	_, floors, err := s.readGrantFloors(ctx)
	return floors, err
}

// MergeGrantFloors durably raises floors to at least the supplied times. The
// merge is a commutative maximum, so a resourceVersion conflict with another
// replica's merge is resolved by re-reading and merging again (bounded); it
// never lowers or removes a floor.
func (s *Store) MergeGrantFloors(ctx context.Context, raise map[string]time.Time) error {
	if s == nil || s.isClosed() {
		return store.ErrClosed
	}
	if ctx == nil || len(raise) > MaxGrantFloors {
		return ErrGrantFloors
	}
	for grant, at := range raise {
		if grant == "" || len(grant) > 512 || at.IsZero() {
			return ErrGrantFloors
		}
	}
	var last error
	for attempt := 0; attempt < grantFloorAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		current, floors, err := s.readGrantFloors(ctx)
		if err != nil {
			return err
		}
		changed := false
		for grant, at := range raise {
			if old, ok := floors[grant]; !ok || at.After(old) {
				floors[grant] = at.UTC()
				changed = true
			}
		}
		if !changed {
			return nil
		}
		if len(floors) > MaxGrantFloors {
			return ErrGrantFloors
		}
		raw, err := json.Marshal(grantFloorDocument{Version: 1, Floors: floors})
		if err != nil {
			return err
		}
		if len(raw) > maxGrantFloorBytes {
			return ErrGrantFloors
		}
		create := current == nil
		if create {
			current = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: grantFloorsName, Namespace: s.opts.Namespace}, Type: grantFloorsType}
		}
		current.Data = map[string][]byte{"floors": raw}
		req := s.coreREST.Put().Namespace(s.opts.Namespace).Resource("secrets").Name(grantFloorsName)
		if create {
			req = s.coreREST.Post().Namespace(s.opts.Namespace).Resource("secrets")
		}
		var saved corev1.Secret
		err = req.Body(current).MaxRetries(0).Do(ctx).Into(&saved)
		if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
			last = err
			continue
		}
		if err != nil {
			return err
		}
		if saved.UID == "" || saved.Type != grantFloorsType || string(saved.Data["floors"]) != string(raw) {
			return fmt.Errorf("%w: write response mismatch", ErrGrantFloors)
		}
		return nil
	}
	return fmt.Errorf("store/kube: grant floor merge contended: %w", last)
}
