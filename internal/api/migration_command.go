package api

import (
	"context"
	"sync/atomic"

	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// MigrationCommand captures the original proof and writer for one closed,
// bounded program. Each call to the returned step executes exactly its next
// operation; a failed step terminates the program, without retry or replay.
type MigrationCommand func(context.Context, *MigrationProgram) (MigrationNext, error)
type MigrationNext func(context.Context) error

// MigrationProgram is constructed only by the migration handlers. Clients
// cannot append operations after admission or retain a transaction capability.
type MigrationProgram struct {
	steps []func(context.Context, *sqlite.CommandTx) (*MigrationCommandPlan, error)
}

func (p *MigrationProgram) Len() int {
	if p == nil {
		return 0
	}
	return len(p.steps)
}
func (p *MigrationProgram) Prepare(ctx context.Context, tx *sqlite.CommandTx, index int) (*MigrationCommandPlan, error) {
	if p == nil || index < 0 || index >= len(p.steps) || tx == nil {
		return nil, errUnavailable("current migration program unavailable")
	}
	return p.steps[index](ctx, tx)
}

// MigrationCommandPlan seals the same-transaction permissions and effect. Its
// acceptance callback publishes only results of a successful actual commit.
type MigrationCommandPlan struct {
	tx     *sqlite.CommandTx
	attrs  []rbac.Attributes
	apply  func(context.Context, *sqlite.CommandTx) error
	accept func()
	state  atomic.Uint32
}

func migrationPlanFor(tx *sqlite.CommandTx, attrs []rbac.Attributes, apply func(context.Context, *sqlite.CommandTx) error, accept func()) *MigrationCommandPlan {
	return &MigrationCommandPlan{tx: tx, attrs: append([]rbac.Attributes(nil), attrs...), apply: apply, accept: accept}
}
func (p *MigrationCommandPlan) PreparedFor(tx *sqlite.CommandTx) bool {
	return p != nil && p.tx == tx && tx != nil && p.apply != nil && p.state.Load() == 0
}
func (p *MigrationCommandPlan) Requirements() []rbac.Attributes {
	if p == nil {
		return nil
	}
	return append([]rbac.Attributes(nil), p.attrs...)
}
func (p *MigrationCommandPlan) Apply(ctx context.Context, tx *sqlite.CommandTx) error {
	if !p.PreparedFor(tx) || !p.state.CompareAndSwap(0, 1) {
		return errUnavailable("current migration plan unavailable")
	}
	return p.apply(ctx, tx)
}
func (p *MigrationCommandPlan) Committed() {
	if p != nil && p.state.CompareAndSwap(1, 2) && p.accept != nil {
		p.accept()
	}
}
func (p *MigrationCommandPlan) Release() {
	if p != nil {
		p.state.Store(3)
	}
}

func migrationSettingsRequirement() rbac.Attributes {
	return rbac.Attributes{Resource: "settings", Verb: "write", Scope: clusterScope}
}
func (s *Server) currentMigrations() bool {
	return s.opts.MigrationCommand != nil || s.opts.ObjectCommand != nil || s.opts.CanonicalSettings != nil
}
func (s *Server) admitMigration(ctx context.Context, p *MigrationProgram) (MigrationNext, error) {
	if err := s.readOnly(); err != nil {
		return nil, err
	}
	if s.opts.MigrationCommand == nil {
		return nil, errUnavailable("current migration command unavailable")
	}
	return s.opts.MigrationCommand(ctx, p)
}
