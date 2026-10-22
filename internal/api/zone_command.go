package api

import (
	"context"
	"sync/atomic"

	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// ZoneCommandAction identifies one closed public zone convenience operation.
type ZoneCommandAction string

// The four actions are the complete closed set of compound zone commands.
const (
	ZoneCommandUpsertRecord ZoneCommandAction = "upsert_record"
	ZoneCommandDeleteRecord ZoneCommandAction = "delete_record"
	ZoneCommandImport       ZoneCommandAction = "import"
	ZoneCommandDeleteZone   ZoneCommandAction = "delete_zone"
)

// ZoneCommandTarget names the DNSZone object, rather than its DNS apex.
type ZoneCommandTarget struct {
	Zone   string
	Action ZoneCommandAction
}

// ZoneCommandPrepare reads and plans inside the original concrete transaction.
// It must not mutate the transaction or perform external effects.
type ZoneCommandPrepare func(context.Context, *sqlite.CommandTx) (*ZoneCommandPlan, error)

// ZoneCommand unconditionally authenticates the complete sealed requirements
// before applying a same-transaction plan, including unchanged and dry runs.
// Installed refusals must never fall back to legacy Store helpers.
type ZoneCommand func(context.Context, ZoneCommandTarget, ZoneCommandPrepare) error

// ZoneCommandPlan is created only by the API's four zone planners. Its private
// requirements and operation bind authorization to that exact transaction.
type ZoneCommandPlan struct {
	tx           *sqlite.CommandTx
	target       ZoneCommandTarget
	requirements []rbac.Attributes
	apply        func(context.Context, *sqlite.CommandTx) error
	state        atomic.Uint32
}

func newZoneCommandPlan(tx *sqlite.CommandTx, target ZoneCommandTarget, requirements []rbac.Attributes, apply func(context.Context, *sqlite.CommandTx) error) *ZoneCommandPlan {
	return &ZoneCommandPlan{tx: tx, target: target, requirements: append([]rbac.Attributes(nil), requirements...), apply: apply}
}

// PreparedFor verifies identity and lifetime without reading mutable store data.
func (p *ZoneCommandPlan) PreparedFor(tx *sqlite.CommandTx, target ZoneCommandTarget) bool {
	return p != nil && tx != nil && p.tx == tx && p.target == target && p.apply != nil && p.state.Load() == 0
}

// Requirements returns a detached copy of the complete bounded permission set.
func (p *ZoneCommandPlan) Requirements() []rbac.Attributes {
	if p == nil || p.state.Load() == 2 {
		return nil
	}
	return append([]rbac.Attributes(nil), p.requirements...)
}

// Apply executes once in its original transaction. Root must authenticate every
// requirement first and retain original writer, proof and dependency exclusions.
func (p *ZoneCommandPlan) Apply(ctx context.Context, tx *sqlite.CommandTx) error {
	if p == nil || ctx == nil || tx == nil || tx != p.tx || p.apply == nil || !p.state.CompareAndSwap(0, 1) {
		return errUnavailable("current zone plan is unavailable")
	}
	defer p.Release()
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.apply(ctx, tx)
}

// Release invalidates even a retained empty plan; no tx access is required.
func (p *ZoneCommandPlan) Release() {
	if p != nil {
		p.state.Store(2)
	}
}
