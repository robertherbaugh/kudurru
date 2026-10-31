package providerhost

import (
	"context"
	"reflect"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/providerentitlement"
	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func verifyWithdrawal(before, after *providerregistry.Registry, old, next servingauth.Security, at time.Time) error {
	oldSources, newSources := before.Sources(), after.Sources()
	// Withdrawal clears config receipt links; it preserves source incarnation,
	// revision, spec and timestamps. Cleared links are not config reconciliation.
	for i := range oldSources {
		oldSources[i].AuthorityKey = ""
	}
	for i := range newSources {
		if newSources[i].AuthorityKey != "" {
			return ErrWithdrawal
		}
	}
	if before.ClusterID() != after.ClusterID() || !reflect.DeepEqual(oldSources, newSources) {
		return ErrWithdrawal
	}
	byRef := map[servingauth.ProviderRef]providerregistry.AuthorityRecord{}
	for _, a := range after.Authorities() {
		if a.Active {
			return ErrWithdrawal
		}
		byRef[a.Provider] = a
	}
	for _, a := range before.Authorities() {
		b, ok := byRef[a.Provider]
		if !ok || a.SpecIdentity != b.SpecIdentity || a.Binding != b.Binding || (!a.Active && !reflect.DeepEqual(a, b)) {
			return ErrWithdrawal
		}
		if a.Active {
			if b.Revision != a.Revision+1 {
				return ErrWithdrawal
			}
			found := false
			for _, f := range next.ProviderFloors {
				if f.Provider == a.Provider.ID && f.UID == a.Provider.UID && !f.ValidAfter.Before(at) {
					found = true
					break
				}
			}
			if !found {
				return ErrWithdrawal
			}
		}
	}
	if len(byRef) != len(before.Authorities()) {
		return ErrWithdrawal
	}
	for _, f := range old.ProviderFloors {
		found := false
		for _, n := range next.ProviderFloors {
			if f.Provider == n.Provider && f.UID == n.UID && !n.ValidAfter.Before(f.ValidAfter) {
				found = true
				break
			}
		}
		if !found {
			return ErrWithdrawal
		}
	}
	for _, f := range next.ProviderFloors {
		known := false
		for _, oldFloor := range old.ProviderFloors {
			if f.Provider == oldFloor.Provider && f.UID == oldFloor.UID {
				known = true
				break
			}
		}
		for _, a := range before.Authorities() {
			if a.Active && f.Provider == a.Provider.ID && f.UID == a.Provider.UID {
				known = true
				break
			}
		}
		if !known {
			return ErrWithdrawal
		}
	}
	if next.Epoch < old.Epoch {
		return ErrWithdrawal
	}
	old.Epoch = next.Epoch
	old.ProviderFloors = next.ProviderFloors
	if !reflect.DeepEqual(old, next) {
		return ErrWithdrawal
	}
	return nil
}

// Reset withdraws complete provider authority/floors with the ORIGINAL writer
// lifetime, then installs a pending nonce only after the real command commits.
// It works without a current/ready resource lease and starts no RunWriter.
func (o *Owner) Reset(original context.Context, target serving.TermActivation) error {
	if err := o.writerCurrent(original, target); err != nil {
		return err
	}
	changed := false
	err := o.opts.Publisher.WithWriterGate(original, target, func() error {
		return o.mutate(original, func(replacement *providerresources.Replacement, g *generation, cut license.Cut) error {
			n, err := copyGeneration(g)
			if err != nil {
				replacement.Retire()
				return err
			}
			fence(n)
			n.license = cut
			n.profile, n.until, _ = providerentitlement.Profile(cut)
			if len(n.baseline) > providerregistry.MaxAuthorities {
				replacement.Retire()
				return ErrBounds
			}
			o.value.Store(n)
			replacement.Retire()
			changed = true
			// This fence remains on failure. The actual original lifetime, not a
			// temporary operation timeout, is retained by the successful pending cut.
			if !n.static.Current() || !cut.Current() {
				return ErrStale
			}
			err = o.opts.Store.Command(original, target.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				if !tx.Authority().Same(target.Term) || tx.Incarnation() != target.Incarnation {
					return ErrContinuity
				}
				before, old, err := readCurrent(ctx, tx)
				if err != nil {
					return err
				}
				sources, err := servingauth.ProviderSourcesCurrent(ctx, tx, before.ConfigSources())
				if err != nil {
					return err
				}
				at := o.opts.Now().UTC()
				if err = o.opts.Withdraw(ctx, tx, at); err != nil {
					return err
				}
				after, next, err := readCurrent(ctx, tx)
				if err != nil {
					return err
				}
				if err = verifyWithdrawal(before, after, old, next, at); err != nil {
					return err
				}
				newSources, err := servingauth.ProviderSourcesCurrent(ctx, tx, after.ConfigSources())
				if err != nil {
					return err
				}
				if !reflect.DeepEqual(sources, newSources) {
					return ErrWithdrawal
				}
				return tx.RequireCommitCheck(func(ctx context.Context) error {
					if err := o.writerCurrent(original, target); err != nil {
						return err
					}
					if err := ctx.Err(); err != nil {
						return err
					}
					if o.value.Load() != n || !n.static.Current() || !cut.Current() {
						return ErrStale
					}
					if !n.until.IsZero() && !o.opts.Now().Before(n.until) {
						return ErrStale
					}
					return nil
				})
			})
			if err != nil {
				return err
			}
			if err = o.writerCurrent(original, target); err != nil {
				return err
			}
			if !n.static.Current() || !cut.Current() {
				return ErrStale
			}
			if n.profile != "" {
				e, err := providerentitlement.NewPending(original, target, n.profile)
				if err != nil {
					return err
				}
				// NewPending can construct a string with a stale token; final actual
				// authority and clock checks remain required before installing it.
				if err = o.writerCurrent(original, target); err != nil {
					return err
				}
				if !cut.Current() || !n.static.Current() {
					return ErrStale
				}
				p := *n
				p.phase = pending
				p.entitlement = e
				p.writer = original
				p.target = target
				o.value.Store(&p)
			}
			return nil
		})
	})
	if changed {
		o.signal()
	}
	return err
}

// FenceWriter invalidates only the matching pending original activation. A late
// old elector callback cannot fence an already installed successor.
func (o *Owner) FenceWriter(ctx context.Context, target serving.TermActivation) error {
	changed := false
	err := o.mutate(ctx, func(r *providerresources.Replacement, g *generation, cut license.Cut) error {
		if g.phase != pending || !g.target.Term.Same(target.Term) || g.target.Incarnation != target.Incarnation {
			return nil
		}
		n, err := copyGeneration(g)
		if err != nil {
			r.Retire()
			return err
		}
		fence(n)
		n.license = cut
		n.profile, n.until, _ = providerentitlement.Profile(cut)
		o.value.Store(n)
		r.Retire()
		changed = true
		return nil
	})
	if changed {
		o.signal()
	}
	return err
}
