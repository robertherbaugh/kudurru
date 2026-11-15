package iamgate

import (
	"context"
	"io"

	"github.com/robertherbaugh/kudurru/internal/store"
)

// Store checks current admission before Kubernetes IAM mutations, including
// bootstrap, file objects, background token status and Unix account operations.
// It is not a cross-object Kubernetes transaction: migration still requires
// every reader/writer to stop. Core DNS configuration is unaffected.
type Store struct {
	store.Store
	Gate *Gate
}

func identityKind(kind store.Kind) bool {
	switch kind {
	case "ServiceAccount", "RoleBinding", "IdentityProvider", "LocalUser", "APIToken", "PrincipalState":
		return true
	default:
		return false
	}
}

func (s *Store) before(ctx context.Context, kind store.Kind) error {
	if !identityKind(kind) {
		return nil
	}
	_, _, err := s.Gate.Capture(ctx)
	return err
}

// Apply checks current IAM admission before delegating the carrier CAS.
func (s *Store) Apply(ctx context.Context, obj store.Object, opts store.ApplyOptions) (store.Object, error) {
	if err := s.before(ctx, store.KeyOf(obj).Kind); err != nil {
		return nil, err
	}
	return s.Store.Apply(ctx, obj, opts)
}

// Delete checks current IAM admission before the underlying deletion.
func (s *Store) Delete(ctx context.Context, key store.Key, opts store.DeleteOptions) error {
	if err := s.before(ctx, key.Kind); err != nil {
		return err
	}
	return s.Store.Delete(ctx, key, opts)
}

// UpdateStatus also gates credential-use and other private identity status.
func (s *Store) UpdateStatus(ctx context.Context, obj store.Object) error {
	if err := s.before(ctx, store.KeyOf(obj).Kind); err != nil {
		return err
	}
	return s.Store.UpdateStatus(ctx, obj)
}

// Restore refuses entry while identity admission is closed.
func (s *Store) Restore(ctx context.Context, r io.Reader) error {
	if _, _, err := s.Gate.Capture(ctx); err != nil {
		return err
	}
	return s.Store.Restore(ctx, r)
}
