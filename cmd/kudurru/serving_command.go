package main

import (
	"context"
	"errors"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/providercoordinator"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// pinProviderHost runs inside the coordinator's resource exclusion. It retains
// every lower publication dependency across the entire current command, so the
// original HTTP lease cannot lose a manager/catalog/key installation between
// its final memory check and SQLite commit. Acquisition performs no network work
// inside this callback. The source wrappers keep their existing identical order.
func (s *servingRuntime) pinProviderHost(ctx context.Context, fn func() error) error {
	return s.host.PinHostCurrent(ctx, func(current func() bool) error {
		if current == nil || !current() {
			return serving.ErrDependency
		}
		return s.opts.Keyrings.WithCurrent(ctx, func(_ servingbuild.KeyringCut) error {
			return s.catalog.WithCurrent(ctx, func() error {
				return s.opts.Transferred.WithCurrent(ctx, func() error {
					if !current() {
						return serving.ErrDependency
					}
					return fn()
				})
			})
		})
	})
}

// currentCommand admits ordinary HTTP mutations under their original serving
// lease and elected writer. fn uses only the supplied concrete transaction;
// publication waits and network work belong after this method returns. Provider
// source/authority replacements use the separate exclusive resource protocol.
func (s *servingRuntime) currentCommand(ctx context.Context, attrs rbac.Attributes, fn func(context.Context, *sqlite.CommandTx, auth.Principal) error) error {
	return s.currentCommandAlso(ctx, attrs, nil, fn)
}

// currentCommandAlso is currentCommand for a mutation that needs further
// permissions besides attrs (querylog.raw for a settings change that keeps
// a raw dnstap sink, D372): each is checked again against the same original
// credential in the same transaction.
func (s *servingRuntime) currentCommandAlso(ctx context.Context, attrs rbac.Attributes, also []rbac.Attributes, fn func(context.Context, *sqlite.CommandTx, auth.Principal) error) error {
	if s == nil || ctx == nil || fn == nil {
		return serving.ErrComponents
	}
	proof, err := servinghttp.Credential(ctx)
	if err != nil {
		return err
	}
	return s.currentAuthCommand(ctx, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		principal, _, err := servingauth.ReauthorizeCurrentWithClock(ctx, tx, deps, proof, attrs, s.opts.Now)
		if err != nil {
			return err
		}
		for _, extra := range also {
			if _, _, err := servingauth.ReauthorizeCurrentWithClock(ctx, tx, deps, proof, extra, s.opts.Now); err != nil {
				return err
			}
		}
		return fn(ctx, tx, principal)
	})
}

// currentAuthCommand retains the original HTTP lease, elected writer episode,
// and complete dependency pin through the actual transaction. It grants no
// authentication or permission by itself: login/self-service callbacks must
// verify their raw proof and current account authority before reading private
// data, mutating state, or returning a deduplicated result.
func (s *servingRuntime) currentAuthCommand(ctx context.Context, fn func(context.Context, *sqlite.CommandTx, servingauth.Dependencies) error) error {
	_, err := s.currentAuthCommandThen(ctx, fn, nil)
	return err
}

// currentAuthCommandThen retains the same writer binding through a synchronous
// postcommit observation outside all transaction and dependency pins. committed
// reports actual command completion, including a no-op, even when publication
// acknowledgment fails or the original writer is canceled after commit.
//
// A command marked with servinghttp.WithRecapture that is refused before fn
// began, only because another commit superseded the request's publication,
// captures again and is dispatched once (D399, recaptureCommand). An unmarked
// command keeps the immediate refusal.
func (s *servingRuntime) currentAuthCommandThen(ctx context.Context, fn func(context.Context, *sqlite.CommandTx, servingauth.Dependencies) error, after func(context.Context, serving.TermActivation) error) (committed bool, err error) {
	if s == nil || ctx == nil || fn == nil {
		return false, serving.ErrComponents
	}
	return s.recaptureCommand(ctx, func(began *bool) (bool, error) {
		return s.currentAuthCommandOnce(ctx, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
			*began = true
			servinghttp.MarkDispatched(ctx)
			s.dispatchHook(ctx)
			return fn(ctx, tx, deps)
		}, after)
	})
}

// commandRecaptureWait bounds how long one command waits, in total, for
// publications to capture again on (D399). The request's own deadline may
// end it earlier. It is the same bound as a later import step's wait for its
// cut.
const commandRecaptureWait = 5 * time.Second

// recaptureCommand runs attempt and, while attempt was refused with
// serving.ErrPending before its transaction body began (began false), lets
// the request capture the current publication again and runs attempt once
// more (D399). Only a command marked with servinghttp.WithRecapture is
// captured again; every other command, and every item of a multi-item
// request, answers its first refusal unchanged, as before D399. A command
// whose body began has dispatched: its refusal or uncertain outcome is final
// and never repeated. Any other refusal (a fence, an authority change, a
// credential, session, role or settings change in the new publication) is
// final too. When the bound ends without an admissible publication, the
// original effect-free refusal is returned.
func (s *servingRuntime) recaptureCommand(ctx context.Context, attempt func(began *bool) (bool, error)) (bool, error) {
	var wait context.Context
	for {
		began := false
		committed, err := attempt(&began)
		if committed || began || !errors.Is(err, serving.ErrPending) || !servinghttp.RecaptureAllowed(ctx) {
			return committed, err
		}
		if requestItemsFrom(ctx) != nil {
			// Every item of a multi-item request stays on the request's
			// publication and command chain (D378): its first item is
			// refused exactly as before, and a later one follows items that
			// committed, so its refusal must not invite sending the request
			// again (no Retry-After, D405).
			return false, err
		}
		if servinghttp.Dispatched(ctx) {
			// An earlier command of this request began its transaction body
			// and may have committed: the request as a whole is not
			// effect-free (D405).
			return false, err
		}
		if wait == nil {
			var cancel context.CancelFunc
			wait, cancel = context.WithTimeout(ctx, commandRecaptureWait)
			defer cancel()
		}
		if servinghttp.Recapture(ctx, wait, s.publisher, s.opts.Now) != nil {
			return false, notDispatchedError{err}
		}
		if hooks := s.commandHooks.Load(); hooks != nil && hooks.afterRecapture != nil {
			hooks.afterRecapture(ctx)
		}
	}
}

// notDispatchedError is a current command's serving.ErrPending refusal
// returned before any transaction body of its request began (D399): nothing
// was applied, so the API answer may carry Retry-After. A refusal from inside
// a body, at commit or after it, and a refusal of a later command or item of
// a request one of whose commands already dispatched, are returned unwrapped
// (D405).
type notDispatchedError struct{ err error }

func (e notDispatchedError) Error() string { return e.err.Error() }
func (e notDispatchedError) Unwrap() error { return e.err }

// commandHooks are test observers of current commands. beforeCheck runs in
// every dispatch attempt after the writer binding and before the first
// publication check; afterRecapture runs after a pre-dispatch recapture.
// dispatch runs when an attempt's transaction body begins.
type commandHooks struct {
	beforeCheck    func(ctx context.Context)
	afterRecapture func(ctx context.Context)
	dispatch       func(ctx context.Context)
}

func (s *servingRuntime) dispatchHook(ctx context.Context) {
	if hooks := s.commandHooks.Load(); hooks != nil && hooks.dispatch != nil {
		hooks.dispatch(ctx)
	}
}

// currentAuthCommandOnce is one dispatch attempt of currentAuthCommandThen.
func (s *servingRuntime) currentAuthCommandOnce(ctx context.Context, fn func(context.Context, *sqlite.CommandTx, servingauth.Dependencies) error, after func(context.Context, serving.TermActivation) error) (committed bool, err error) {
	status := s.primary.driver.Status()
	if !status.Running {
		// A request that began on an enrolled follower has no local writer by
		// design: name the primary instead of reporting a fenced writer.
		if err := s.primaryRequired(ctx); err != nil {
			return false, err
		}
		return false, replication.ErrFenced
	}
	writer := s.primary.writer.active.Load()
	request := ctx
	ctx, release, err := s.primary.writer.bind(ctx, status.Writer)
	if err != nil {
		return false, err
	}
	defer release()
	target := serving.TermActivation{Term: status.Writer.Term, Incarnation: status.Writer.Incarnation}
	// A request with several items carries one command chain (D378). Its first
	// item is admitted below exactly as a single command is.
	items := requestItemsFrom(ctx)
	if items != nil {
		if err := items.bind(writer, target); err != nil {
			return false, err
		}
		ctx = providercoordinator.WithChain(ctx, items.chain)
		if items.chain.Anchored() {
			if err := s.paceLaterItem(request, ctx, items, target); err != nil {
				return false, err
			}
		}
	}
	checkWriter := func(ctx context.Context) error {
		// Read the original episode directly: its cancellation can precede
		// propagation through bind's AfterFunc to this request context.
		if !s.primary.writer.current(writer) {
			return replication.ErrFenced
		}
		return s.writerCurrent(ctx, target)
	}
	check := func(ctx context.Context) error {
		if err := checkWriter(ctx); err != nil {
			return err
		}
		if items != nil && items.chain.Anchored() {
			return s.checkLaterItem(ctx, target)
		}
		return servinghttp.CheckWriter(ctx, s.publisher, target)
	}
	if hooks := s.commandHooks.Load(); hooks != nil && hooks.beforeCheck != nil {
		hooks.beforeCheck(ctx)
	}
	if err := check(ctx); err != nil {
		return false, err
	}
	err = s.providers.WithCommand(ctx, target, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		if err := check(ctx); err != nil {
			return err
		}
		if err := tx.RequireCommitCheck(check); err != nil {
			return err
		}
		return fn(ctx, tx, deps)
	})
	if err != nil {
		return false, err
	}
	if after == nil {
		return true, nil
	}
	// Our own commit can supersede the request's HTTP lease. Only the original
	// writer episode and target remain admission constraints on acknowledgment.
	if err := checkWriter(ctx); err != nil {
		return true, err
	}
	if err := after(ctx, target); err != nil {
		return true, err
	}
	return true, checkWriter(ctx)
}
