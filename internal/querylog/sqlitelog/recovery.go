package sqlitelog

import (
	"context"
	"errors"
	"time"
)

// Recovery errors carry fixed operational state, never SQL or raw DNS data.
var (
	ErrRollupLegacy = errors.New("legacy_provenance_required")
	ErrRollupStale  = errors.New("stale_rollup_evidence")
	ErrRollupBound  = errors.New("rollup_evidence_exceeds_bounds")
)

// RollupEvidence binds a bounded retained-input snapshot to one source/hour
// and exact node scope. Digest is independently recomputed before replacement;
// it is not permission or proof of ownership of any opaque legacy cell.
type RollupEvidence struct {
	Scope
	SourceID      string    `json:"source_id"`
	Node          string    `json:"node"`
	Hour          int64     `json:"hour"`
	Revision      int64     `json:"revision"`
	Head          int64     `json:"head"`
	Digest        string    `json:"digest"`
	ExpiresAt     time.Time `json:"expires_at"`
	BlockedReason string    `json:"blocked_reason,omitempty"`
}

// RollupRepairResult reports application or a previously committed receipt.
type RollupRepairResult struct {
	Applied        bool `json:"applied"`
	AlreadyApplied bool `json:"already_applied"`
}

// RollupRecovery owns source-locked, source-provenance recovery operations.
// check must validate current authority immediately before commit, including a
// duplicate result. It never supplies or invents ownership of a legacy cell.
type RollupRecovery interface {
	Evidence(context.Context, string, string, int64) (*RollupEvidence, error)
	Verify(context.Context, RollupEvidence) error
	Repair(context.Context, RollupEvidence, func(context.Context) error) (*RollupRepairResult, error)
}

// RollupBackfillStatus bounds detail to 25 sources/buckets in the selected scope.
type RollupBackfillStatus struct {
	LegacyCells  int64                  `json:"legacy_cells"`
	PendingDays  int64                  `json:"pending_days"`
	PendingHours int64                  `json:"pending_hours"`
	Details      []RollupBackfillDetail `json:"details"`
	Capped       bool                   `json:"capped"`
	Truncated    bool                   `json:"truncated"`
}

// RollupBackfillDetail names an unresolved range without exposing fact values.
type RollupBackfillDetail struct {
	Source       string `json:"source"`
	LegacyBefore int64  `json:"legacy_before,omitempty"`
	Hour         int64  `json:"hour,omitempty"`
	Resolution   int64  `json:"resolution,omitempty"`
	State        string `json:"state"`
}

// RollupEvidence captures central input; local and peer fallback are forbidden.
func (r *Reader) RollupEvidence(ctx context.Context, source string, hour int64) (*RollupEvidence, error) {
	if r.c == nil || r.c.opts.Recovery == nil {
		return nil, ErrCentralRead
	}
	return r.c.opts.Recovery.Evidence(ctx, source, r.c.node, hour)
}

// RepairRollup applies only the receipt's exact immutable reader scope.
func (r *Reader) RepairRollup(ctx context.Context, e RollupEvidence, check func(context.Context) error) (*RollupRepairResult, error) {
	if r.c == nil || r.c.opts.Recovery == nil {
		return nil, ErrCentralRead
	}
	if e.Node != r.c.node {
		return nil, ErrRollupStale
	}
	return r.c.opts.Recovery.Repair(ctx, e, check)
}

// VerifyRollup checks a receipt against a fresh central snapshot without mutation.
func (r *Reader) VerifyRollup(ctx context.Context, e RollupEvidence) error {
	if r.c == nil || r.c.opts.Recovery == nil {
		return ErrCentralRead
	}
	if e.Node != r.c.node {
		return ErrRollupStale
	}
	return r.c.opts.Recovery.Verify(ctx, e)
}
