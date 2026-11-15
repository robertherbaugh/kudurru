package providercoordinator

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func completeReceipts(d providerresources.Desired, allowed bool, candidates []*providerresources.AuthorityCandidate) ([]providerregistry.DesiredAuthority, bool) {
	byID := map[string]providerregistry.DesiredAuthority{}
	if allowed {
		for _, a := range d.Authorities {
			if !a.Active {
				continue
			}
			for _, s := range d.Sources {
				if s.Ref == a.Provider && s.SpecIdentity == a.SpecIdentity {
					byID[s.Ref.ID] = providerregistry.DesiredAuthority{Source: s, Binding: a.Binding}
					break
				}
			}
		}
		for _, candidate := range candidates {
			if candidate.Match(d) != nil {
				continue
			}
			receipt := candidate.Receipt()
			for _, s := range d.Sources {
				if s.Ref == receipt.Source.Ref && s.SpecIdentity == receipt.Source.SpecIdentity && s.PolicyIdentity == receipt.Source.PolicyIdentity {
					byID[s.Ref.ID] = receipt
					break
				}
			}
		}
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	out := make([]providerregistry.DesiredAuthority, 0, len(ids))
	for _, id := range ids {
		out = append(out, byID[id])
	}
	unchanged := true
	count := 0
	for _, a := range d.Authorities {
		if !a.Active {
			continue
		}
		count++
		r, ok := byID[a.Provider.ID]
		if !ok || r.Source.Ref != a.Provider || r.Source.SpecIdentity != a.SpecIdentity || r.Binding != a.Binding {
			unchanged = false
		}
	}
	return out, !unchanged || count != len(out)
}

func (c *Coordinator) admit(ctx context.Context, target serving.TermActivation, candidates []*providerresources.AuthorityCandidate) error {
	if c.opts.Admit == nil {
		return ErrAdmission
	}
	var h HostCapture
	defer func() { releaseHost(h) }()
	return c.opts.Publisher.WithWriterGate(ctx, target, func() error {
		return c.opts.Resources.WithReplacement(ctx, func(replacement *providerresources.Replacement) error {
			return c.opts.PinHost(ctx, func() error {
				var next providerresources.Desired
				changed := false
				err := replacement.WithCurrent(func(p *providerresources.Pinned) error {
					return c.opts.Store.Command(ctx, target.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
						r, err := registryCurrent(ctx, tx)
						if err != nil {
							return err
						}
						h, err = c.host(ctx, r)
						if err != nil {
							return err
						}
						if _, err = p.DependenciesCurrent(ctx, tx, h.Providers); err != nil {
							return err
						}
						if err = c.checkHost(ctx, tx, h, target); err != nil {
							return err
						}
						d, err := desiredCurrent(ctx, tx, h.Providers)
						if err != nil {
							return err
						}
						receipts, modify := completeReceipts(d, h.Auth.License.Providers, candidates)
						if !modify {
							return nil
						}
						a := &Admission{registryDigest: r.Digest(), hostIdentity: h.Identity, receipts: receipts, sources: d.Sources}
						a.active.Store(true)
						err = func() error {
							defer a.active.Store(false)
							return c.opts.Admit(ctx, tx, a)
						}()
						if err != nil {
							return err
						}
						// Root callback may deny or stage admitted security work; it must
						// not edit source/registry/host authority under our prior proof.
						after, err := desiredCurrent(ctx, tx, h.Providers)
						if err != nil {
							return err
						}
						if err = p.Match(after); err != nil {
							return err
						}
						changed = true
						if err = security.AcceptProviderAuthorities(ctx, tx, receipts, c.opts.Now().UTC()); err != nil {
							return err
						}
						next, err = desiredCurrent(ctx, tx, h.Providers)
						return err
					})
				})
				if err != nil {
					replacement.Retire()
					return err
				}
				if changed {
					if err = replacement.SetDesired(next); err != nil {
						replacement.Retire()
						return err
					}
				}
				return nil
			})
		})
	})
}

// RunWriter retains only the original carried opaque elector token/context.
// It may be restarted sequentially by root with a NEW admitted context, but a
// running invocation never samples/adopts a successor. Followers only use Run.
func (c *Coordinator) RunWriter(ctx context.Context, target serving.TermActivation) error {
	ctx, stop, err := c.context(ctx)
	if err != nil {
		return err
	}
	defer stop()
	if err = c.writerCurrent(ctx, target); err != nil {
		return err
	}
	if c.opts.Admit == nil {
		return ErrAdmission
	}
	c.mu.Lock()
	if c.closed.Load() {
		c.mu.Unlock()
		return ErrClosed
	}
	if c.writerRunning {
		c.mu.Unlock()
		return ErrRunning
	}
	done := make(chan struct{})
	c.writerRunning = true
	c.writerDone = done
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.writerRunning = false; c.mu.Unlock(); close(done) }()
	timer := time.NewTimer(0)
	defer timer.Stop()
	candidateChanges := c.opts.Resources.AuthorityCandidates()
	var retryAt time.Time
	for {
		changes := c.Changes()
		generation := c.opts.Store.GenerationChanges()
		authority := c.opts.Store.AuthorityChanges()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-authority:
		case <-generation:
		case <-changes:
		case <-candidateChanges:
		case <-timer.C:
		}
		if err := c.writerCurrent(ctx, target); err != nil {
			return err
		}
		delay := c.opts.RetryInterval
		if now := c.opts.Now(); !now.Before(retryAt) {
			candidates := c.opts.Resources.TakeCandidates()
			opctx, finish := context.WithTimeout(ctx, c.opts.OperationTimeout)
			err = c.admit(opctx, target, candidates)
			finish()
			if errors.Is(err, store.ErrClosed) || errors.Is(err, providerresources.ErrClosed) || errors.Is(err, ErrClosed) {
				return err
			}
			if err = c.writerCurrent(ctx, target); err != nil {
				return err
			}
			retryAt = c.opts.Now().Add(c.opts.RetryInterval)
		} else {
			delay = retryAt.Sub(now)
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(delay)
	}
}
