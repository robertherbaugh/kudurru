package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// MigrationCommand binds one immutable API program to the original raw proof
// and elected writer episode. Capturing a later step's first lease never renews
// credentials or retries a refused operation. Every effect and journal outcome
// share the concrete transaction, original dependency pins and final checks.
func (s *servingRuntime) MigrationCommand(ctx context.Context, program *api.MigrationProgram) (api.MigrationNext, error) {
	if s == nil || ctx == nil || program.Len() < 1 || program.Len() > 1002 {
		return nil, currentCommandAPIError(serving.ErrComponents)
	}
	proof, err := servinghttp.Credential(ctx)
	if err != nil {
		return nil, currentCommandAPIError(err)
	}
	writer := s.primary.writer.active.Load()
	if !s.primary.writer.current(writer) {
		if err := s.primaryRequired(ctx); err != nil {
			return nil, err
		}
		return nil, currentCommandAPIError(replication.ErrFenced)
	}
	deadline := s.opts.Now().Add(10 * time.Minute)
	var mu sync.Mutex
	index, stopped := 0, false
	return func(stepCtx context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		if stopped || index >= program.Len() || !s.opts.Now().Before(deadline) {
			return currentCommandAPIError(serving.ErrReleased)
		}
		stopped = true // every failure is terminal, including capture and no-ops
		if !s.primary.writer.current(writer) {
			return currentCommandAPIError(replication.ErrFenced)
		}
		var plan *api.MigrationCommandPlan
		// began records that the step's transaction body started: from then on
		// it has dispatched, and a refusal is final.
		began := false
		execute := func(commandCtx context.Context) error {
			return s.currentAuthCommand(commandCtx, func(commandCtx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
				began = true
				checkWriter := func(context.Context) error {
					if !s.opts.Now().Before(deadline) {
						return servingauth.ErrCredentials
					}
					if !s.primary.writer.current(writer) {
						return replication.ErrFenced
					}
					return nil
				}
				if err := checkWriter(commandCtx); err != nil {
					return err
				}
				if err := tx.RequireCommitCheck(checkWriter); err != nil {
					return err
				}
				var applied atomic.Bool
				check := func(context.Context) error {
					if !applied.Load() {
						return servingauth.ErrDenied
					}
					return nil
				}
				if err := tx.RequireCommitCheck(check); err != nil {
					return err
				}
				var err error
				plan, err = program.Prepare(commandCtx, tx, index)
				if err != nil {
					return err
				}
				if !plan.PreparedFor(tx) || !migrationCommandRequirements(plan.Requirements()) {
					return servingauth.ErrDenied
				}
				if _, err := servingauth.ReauthorizeCurrentAllWithClock(commandCtx, tx, deps, proof, plan.Requirements(), s.opts.Now); err != nil {
					return err
				}
				if err := plan.Apply(commandCtx, tx); err != nil {
					return err
				}
				applied.Store(true)
				return nil
			})
		}
		if index == 0 {
			// Initialization uses the original HTTP lease through its actual commit.
			err = execute(stepCtx)
		} else {
			// Wait up to 5 s only for the NEXT command's initial cut, before
			// authentication or preparation; the command itself runs on stepCtx.
			// A cut that a later commit (such as the zone lifecycle driver's)
			// superseded before the step's transaction body began is refused with
			// serving.ErrPending before anything was authenticated, prepared or
			// staged. That is the same as a cut not yet published, so the step
			// captures again within the same wait. Once the body begins,
			// currentAuthCommand has dispatched exactly once and a refusal is final.
			wait, cancel := context.WithTimeout(stepCtx, 5*time.Second)
			defer cancel()
			for {
				changed, authority := s.publisher.Subscribe(), s.publisher.AuthorityChanges()
				err = servinghttp.WithCommandStep(stepCtx, wait, s.publisher, func(c context.Context) error {
					if s.afterMigrationCut != nil {
						s.afterMigrationCut(c, index)
					}
					return execute(c)
				})
				if began || !errors.Is(err, serving.ErrPending) {
					break
				}
				select {
				case <-wait.Done():
					err = wait.Err()
				case <-changed:
					continue
				case <-authority:
					continue
				}
				break
			}
		}
		if plan != nil {
			defer plan.Release()
		}
		if err != nil {
			return currentCommandAPIError(err)
		}
		plan.Committed()
		index++
		stopped = false
		s.publisher.Wake()
		return nil
	}, nil
}

func migrationCommandRequirements(attrs []rbac.Attributes) bool {
	if len(attrs) < 1 || len(attrs) > 1001 || attrs[0] != (rbac.Attributes{Resource: "settings", Verb: "write", Scope: rbac.Scope{Kind: rbac.ScopeCluster}}) {
		return false
	}
	for _, a := range attrs[1:] {
		var kind store.Kind
		switch a.Resource {
		case "zones":
			kind = "DNSZone"
		case "records":
			kind = "DNSRecordSet"
		case "blocklists":
			kind = "BlockList"
		case "clientgroups":
			kind = "ClientGroup"
		case "policies":
			kind = "Policy"
		default:
			return false
		}
		if a.Verb != "write" || !objectCommandAttributes(store.Key{Kind: kind, Namespace: a.Namespace, Name: a.Name}, a.Resource, []rbac.Attributes{a}) {
			return false
		}
	}
	return true
}
