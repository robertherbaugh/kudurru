package providerhost

import (
	"context"
	"errors"
	"reflect"

	"github.com/robertherbaugh/kudurru/internal/auth/providercoordinator"
	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func readCurrent(ctx context.Context, tx *sqlite.CommandTx) (*providerregistry.Registry, servingauth.Security, error) {
	if tx == nil {
		return nil, servingauth.Security{}, ErrInvalid
	}
	if err := tx.CheckSectionBounds(ctx, "security", servingauth.MaxRows, servingauth.MaxSecurityBytes); err != nil {
		if errors.Is(err, replication.ErrTooLarge) {
			err = errors.Join(ErrBounds, err)
		}
		return nil, servingauth.Security{}, err
	}
	rows, err := tx.ListSections(ctx, "security")
	if err != nil {
		return nil, servingauth.Security{}, err
	}
	r, err := providerregistry.Decode(rows, tx.Authority().ClusterID)
	if err != nil {
		return nil, servingauth.Security{}, err
	}
	s, err := servingauth.DecodeSecurity(rows)
	return r, s, err
}
func (o *Owner) writerCurrent(ctx context.Context, t serving.TermActivation) error {
	if err := o.check(ctx); err != nil {
		return err
	}
	token, ok := sqlite.WriterTokenFromContext(ctx)
	if !ok || t.Term.Validate() != nil || t.Incarnation == 0 || !token.Term().Same(t.Term) || token.Incarnation() != t.Incarnation {
		return replication.ErrFenced
	}
	a, blocked := o.opts.Store.ServingAuthority()
	if blocked || !a.Writable() || !a.Term.Same(t.Term) || a.Incarnation != t.Incarnation {
		return replication.ErrFenced
	}
	return nil
}

// CheckAdmission checks the opaque complete proposal in the concrete current
// command under coordinator guards. It does not Stage or reacquire outer guards.
func (o *Owner) CheckAdmission(ctx context.Context, tx *sqlite.CommandTx, a *providercoordinator.Admission) error {
	if err := o.check(ctx); err != nil {
		return err
	}
	if tx == nil || a == nil {
		return ErrInvalid
	}
	g := o.value.Load()
	if g.phase != pending || o.writerCurrent(ctx, g.target) != nil || !o.current(g) {
		return ErrContinuity
	}
	if !tx.Authority().Same(g.target.Term) || tx.Incarnation() != g.target.Incarnation {
		return replication.ErrFenced
	}
	r, _, err := readCurrent(ctx, tx)
	if err != nil {
		return err
	}
	h, err := o.Host(ctx, r)
	if err != nil {
		return err
	}
	defer h.Release()
	if a.RegistryDigest() != r.Digest() || a.HostIdentity() != h.Identity || !h.Auth.License.Providers {
		return ErrStale
	}
	sources, err := servingauth.ProviderSourcesCurrent(ctx, tx, r.ConfigSources())
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(a.Sources(), sources) {
		return ErrStale
	}
	receipts := a.Receipts()
	if receipts == nil || len(receipts) > providerregistry.MaxActiveAuthorities {
		return ErrBounds
	}
	ids := map[string]bool{}
	for _, receipt := range receipts {
		if ids[receipt.Source.Ref.ID] || receipt.Binding.EntitlementIdentity != g.entitlement {
			return ErrContinuity
		}
		ids[receipt.Source.Ref.ID] = true
		found := false
		for _, s := range sources {
			if reflect.DeepEqual(s, receipt.Source) {
				found = true
				break
			}
		}
		if !found {
			return ErrStale
		}
	}
	return nil
}
