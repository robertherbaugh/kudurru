package providerhost

import (
	"context"

	"github.com/robertherbaugh/kudurru/internal/auth/providerentitlement"
	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// ObserveInstalled accepts one validated CURRENT actual Store image/token. Cold
// followers establish historical baseline and wait for a different primary E;
// no local pending value, follower command, or same-byte restart fallback.
func (o *Owner) ObserveInstalled(ctx context.Context, image *replication.Image, token sqlite.GenerationToken) error {
	if err := o.check(ctx); err != nil {
		return err
	}
	if image == nil || !image.Generation().Same(token.Generation()) || token.DNSOnly() {
		return ErrContinuity
	}
	start := o.value.Load()
	// Bound auth before generic validator cloning. ValidateImage includes root's
	// complete inventory validator, not just public object validation.
	if _, err := servingauth.CaptureImage(ctx, image, o.opts.Schema); err != nil {
		return err
	}
	validated, err := o.opts.Store.ValidateImage(ctx, *image)
	if err != nil {
		return err
	}
	im := validated.Image()
	r, err := providerregistry.Decode(im.Payload.Security, im.Term.ClusterID)
	if err != nil {
		return err
	}
	if _, err := servingauth.DecodeSecurity(im.Payload.Security); err != nil {
		return err
	}
	if r.ClusterID() != start.cluster || token.Authority().Term.ClusterID != start.cluster {
		return ErrContinuity
	}
	changed := false
	err = o.mutate(ctx, func(replacement *providerresources.Replacement, g *generation, cut license.Cut) error {
		if g != start {
			return ErrStale
		}
		n, err := copyGeneration(g)
		if err != nil {
			return err
		}
		p, until, _ := providerentitlement.Profile(cut)
		localChange := g.license.Identity() != cut.Identity() || !g.license.Current() || p != g.profile
		if localChange {
			taint(n)
			n.license = cut
			n.profile = p
			n.until = until
		}
		mode := token.Authority().Mode
		if mode == replication.Follower {
			e, selectionErr := providerentitlement.Installed(r, g.cluster, n.profile)
			if !n.observed {
				fence(n)
				n.observed = true
				for _, a := range r.Authorities() {
					addBaseline(n, a.Binding.EntitlementIdentity)
				}
			} else if selectionErr == nil && !n.baseline[e] && !n.historyFull {
				if len(n.baseline) == 0 {
					// An empty historical image is not proven first enrollment.
					// First actual E becomes baseline, never a self-admission.
					addBaseline(n, e)
				} else {
					if n.entitlement != "" && n.entitlement != e {
						addBaseline(n, n.entitlement)
					}
					n.phase = active
					n.entitlement = e
					n.writer = nil
					if n.historyFull {
						fence(n)
					}
				}
			} else if n.phase == active && (selectionErr != nil || e != n.entitlement) {
				fence(n)
			}
		}
		if len(n.baseline) > providerregistry.MaxAuthorities {
			return ErrBounds
		}
		// Ignore unrelated registry churn once continuity state is unchanged.
		same := sameContinuity(g, n)
		if err = o.opts.Store.PublishIfCurrent(ctx, token, sqlite.PublicationOptions{}, func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if o.value.Load() != g || !cut.Current() || !g.static.Current() {
				return ErrStale
			}
			return nil
		}); err != nil {
			return err
		}
		if !same {
			o.value.Store(n)
			replacement.Retire()
			changed = true
		}
		return nil
	})
	if changed {
		o.signal()
	}
	return err
}
func sameContinuity(a, b *generation) bool {
	if a.phase != b.phase || a.entitlement != b.entitlement || a.observed != b.observed || a.historyFull != b.historyFull || a.license.Identity() != b.license.Identity() || a.profile != b.profile || len(a.baseline) != len(b.baseline) {
		return false
	}
	for k := range a.baseline {
		if !b.baseline[k] {
			return false
		}
	}
	return true
}
