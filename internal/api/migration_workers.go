package api

import (
	"context"
	"sync"
	"time"

	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/importers"
)

const (
	migrationTimeout    = 10 * time.Minute
	maxMigrationWorkers = 2
)

// The store owns progress; only the reviewed, credential-free payload lives in
// memory. Bound concurrency and stop workers before the API's store is closed.
type migrationWorkers struct {
	mu      sync.Mutex
	wg      sync.WaitGroup
	cancels map[string]context.CancelFunc
	closed  bool
}

// Close stops background imports and waits for their final journal updates.
// Serve calls it automatically; callers mounting Handler directly should defer it.
func (s *Server) Close() {
	s.imports.mu.Lock()
	s.imports.closed = true
	for _, cancel := range s.imports.cancels {
		cancel()
	}
	s.imports.mu.Unlock()
	s.imports.wg.Wait()
}

func (s *Server) startMigration(ctx context.Context, plan *importers.Plan) (*MigrationRun, error) {
	if s.currentMigrations() {
		return s.startCurrentMigration(ctx, plan)
	}
	if err := s.readOnly(); err != nil {
		return nil, err
	}
	if _, _, err := plan.Selected(); err != nil {
		return nil, errInvalid("%s", err)
	}
	if len(plan.Objects) == 0 && len(plan.Settings) == 0 {
		return nil, errInvalid("Select at least one object or settings section to import")
	}
	s.imports.mu.Lock()
	defer s.imports.mu.Unlock()
	if s.imports.closed || len(s.imports.cancels) >= maxMigrationWorkers {
		return nil, errUnavailable("Import workers are busy or shutting down; review migration history before starting another import")
	}
	run, err := s.newMigrationRun(ctx, plan, "")
	if err != nil {
		return nil, err
	}
	// The request middleware still reads its state while sending the response.
	// Workers get their own copy, retaining the actor and normal authorization.
	state := *stateFrom(ctx)
	workCtx, cancel := context.WithTimeout(withState(context.WithoutCancel(ctx), &state), migrationTimeout)
	principal, ok := auth.PrincipalFrom(workCtx)
	workCtx = audit.WithRequest(workCtx, actorOf(principal, ok), audit.Request{ID: state.id, IP: state.clientIP.String(), Method: state.method, Path: state.path, UA: state.userAgent}, func() string { return state.authz })
	view := migrationView(run)
	if s.imports.cancels == nil {
		s.imports.cancels = map[string]context.CancelFunc{}
	}
	s.imports.cancels[run.Name] = cancel
	s.imports.wg.Add(1)
	go func() {
		defer s.imports.wg.Done()
		defer func() {
			cancel()
			s.imports.mu.Lock()
			delete(s.imports.cancels, run.Name)
			s.imports.mu.Unlock()
		}()
		s.executeMigration(workCtx, run, plan)
	}()
	return &view, nil
}
