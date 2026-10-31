package main

import (
	"context"
	"errors"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/robertherbaugh/kudurru/internal/auth/providercoordinator"
	"github.com/robertherbaugh/kudurru/internal/auth/providerhost"
	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// begin registers before Close can start waiting. An always-on Run is single
// use; original elector lifetimes may run sequentially but never overlap.
func (s *servingRuntime) begin(ctx context.Context, writer bool) (context.Context, func(), error) {
	if ctx == nil {
		return nil, nil, serving.ErrComponents
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || s.life.Err() != nil {
		return nil, nil, serving.ErrClosed
	}
	if writer && s.writing || !writer && s.running {
		return nil, nil, providercoordinator.ErrRunning
	}
	if writer {
		s.writing = true
	} else {
		s.running = true
	}
	s.workers.Add(1)
	child, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.life, cancel)
	return child, func() {
		stop()
		cancel()
		s.mu.Lock()
		if writer {
			s.writing = false
		}
		s.mu.Unlock()
		s.workers.Done()
	}, nil
}

// Run keeps complete publication and read-only provider acquisition alive on
// every node. These workers never sample or mint an elected writer token.
func (s *servingRuntime) Run(ctx context.Context) error {
	ctx, finish, err := s.begin(ctx, false)
	if err != nil {
		return err
	}
	defer finish()
	defer s.cancel()
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return s.publisher.Run(ctx) })
	g.Go(func() error { return s.providers.Run(ctx) })
	g.Go(func() error { return s.integrations.host.Run(ctx) })
	g.Go(func() error { return s.catalog.Watch(ctx, s.publisher.Wake) })
	g.Go(func() error { return s.opts.Transferred.Watch(ctx, s.publisher.Wake) })
	g.Go(func() error {
		for {
			changed := s.opts.Keyrings.Changes()
			s.publisher.Wake()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-changed:
			}
		}
	})
	g.Go(func() error { return s.observeHost(ctx) })
	return g.Wait()
}

// lifecycleCall only serializes root lifecycle operations. It precedes all
// owner gates and is never acquired in a capture, command or final-check hook.
// Thus Reset plus its retained Host capture cannot adopt an intervening local
// replacement. Independent license replacement still fails the original cursor.
func (s *servingRuntime) lifecycleCall(ctx context.Context, fn func() error) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.lifecycle:
	}
	defer func() { s.lifecycle <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}

func transientServing(err error) bool {
	return errors.Is(err, providerhost.ErrStale) || errors.Is(err, serving.ErrSuperseded) || errors.Is(err, serving.ErrDependency) || errors.Is(err, store.ErrConflict) || errors.Is(err, replication.ErrStale) || errors.Is(err, replication.ErrCAS)
}

func (s *servingRuntime) observeHost(ctx context.Context) error {
	timer := time.NewTicker(time.Second) // private license nbf/expiry has no broadcast
	defer timer.Stop()
	for {
		licenseChanged := s.opts.License.Changes()
		generation := s.opts.Store.GenerationChanges()
		authority := s.opts.Store.AuthorityChanges()
		err := s.lifecycleCall(ctx, func() error {
			if err := s.host.ObserveLicense(ctx); err != nil {
				return err
			}
			a, blocked := s.opts.Store.ServingAuthority()
			if blocked || a.Mode != replication.Follower {
				return nil
			}
			image, token, err := s.opts.Store.ReadServingCandidate(ctx)
			// An enrolled follower can start before its first replica arrives.
			// Keep observing without granting a host or initializing any roots;
			// the subscribed generation/authority signals cover a racing install.
			// This exception is local to a missing initial follower image, never
			// a general retry of read-only commands or malformed installed data.
			if errors.Is(err, replication.ErrReadOnly) && a.Installed.Term.ClusterID == "" {
				return nil
			}
			if err != nil {
				return err
			}
			return s.host.ObserveInstalled(ctx, image, token)
		})
		if err != nil && !transientServing(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-licenseChanged:
		case <-generation:
		case <-authority:
		case <-timer.C:
		}
	}
}

func (s *servingRuntime) writerCurrent(ctx context.Context, target serving.TermActivation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	token, ok := sqlite.WriterTokenFromContext(ctx)
	a, blocked := s.opts.Store.ServingAuthority()
	if !ok || !token.Term().Same(target.Term) || token.Incarnation() != target.Incarnation || blocked || !a.Writable() || !a.Term.Same(target.Term) || a.Incarnation != target.Incarnation {
		return replication.ErrFenced
	}
	return nil
}

// reconcileConfigs admits only the immutable startup manifest, under the same
// original writer and local resource guards as a current command. Floors are
// staged before the registry plan by the security owner; failures roll back.
func (s *servingRuntime) reconcileConfigs(ctx context.Context, target serving.TermActivation) error {
	return s.publisher.WithWriterGate(ctx, target, func() error {
		return s.resources.WithReplacement(ctx, func(replacement *providerresources.Replacement) error {
			return s.host.PinHostCurrent(ctx, func(current func() bool) error {
				err := s.opts.Store.Command(ctx, target.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
					if !tx.Authority().Same(target.Term) || tx.Incarnation() != target.Incarnation || current == nil || !current() {
						return replication.ErrFenced
					}
					if err := tx.RequireCommitCheck(func(ctx context.Context) error {
						if !current() {
							return serving.ErrDependency
						}
						return s.writerCurrent(ctx, target)
					}); err != nil {
						return err
					}
					return security.ReconcileProviderConfigs(ctx, tx, s.local, s.opts.Now().UTC())
				})
				// A new source inventory must be rebuilt after commit. No resource
				// preparation or network work occurs while the SQLite gate is held.
				replacement.Retire()
				return err
			})
		})
	})
}

func (s *servingRuntime) resetWriter(ctx context.Context, target serving.TermActivation) (h providercoordinator.HostCapture, err error) {
	err = s.lifecycleCall(ctx, func() error {
		if err := s.host.ObserveLicense(ctx); err != nil {
			return err
		}
		if err := s.host.Reset(ctx, target); err != nil {
			return err
		}
		if err := s.reconcileConfigs(ctx, target); err != nil {
			return err
		}
		image, err := s.opts.Store.ReadGeneration(ctx)
		if err != nil {
			return err
		}
		registry, err := providerregistry.Decode(image.Payload.Security, image.Term.ClusterID)
		if err != nil {
			return err
		}
		// Reset deliberately retired old provider material. Host capture needs
		// the replacement gate, not a ready resource lease that cannot exist yet.
		return s.resources.WithReplacement(ctx, func(_ *providerresources.Replacement) error {
			return s.host.PinHost(ctx, func() error { var e error; h, e = s.host.Host(ctx, registry); return e })
		})
	})
	return h, err
}

// RunWriter starts only under the actual elector's carried token. Each local
// host loss/replacement cancels and joins the old episode, withdraws authorities
// and floors, then admits a fresh activation. No successor token is sampled.
func (s *servingRuntime) RunWriter(ctx context.Context) error {
	ctx, finish, err := s.begin(ctx, true)
	if err != nil {
		return err
	}
	defer finish()
	token, ok := sqlite.WriterTokenFromContext(ctx)
	if !ok {
		return replication.ErrFenced
	}
	target := serving.TermActivation{Term: token.Term(), Incarnation: token.Incarnation()}
	defer func() {
		// Cancellation must not leave a pending activation usable. Its original
		// context already denies immediately; this closes the local lifecycle.
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.lifecycleCall(cleanup, func() error { return s.host.FenceWriter(cleanup, target) })
	}()
	for {
		if err := s.writerCurrent(ctx, target); err != nil {
			return err
		}
		h, err := s.resetWriter(ctx, target)
		if err == nil {
			err = s.writerEpisode(ctx, target, h)
		}
		if h.Release != nil {
			h.Release()
		}
		if err != nil && !transientServing(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (s *servingRuntime) writerEpisode(ctx context.Context, target serving.TermActivation, host providercoordinator.HostCapture) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return s.providers.RunWriter(ctx, target) })
	g.Go(func() error { return s.primary.Run(ctx) })
	g.Go(func() error {
		timer := time.NewTicker(time.Second)
		defer timer.Stop()
		for {
			changed := s.host.Changes() // subscribe before the original cursor check
			if err := s.writerCurrent(ctx, target); err != nil {
				return err
			}
			if host.Current == nil || !host.Current() {
				return providerhost.ErrStale
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-changed:
			case <-timer.C:
			}
		}
	})
	return g.Wait()
}
