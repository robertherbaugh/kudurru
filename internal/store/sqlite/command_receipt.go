package sqlite

import (
	"context"
	"sync/atomic"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// CommandReceipt is the Store's own statement of how one Command ended. The
// Store fills it in the command's finalizer, after the SQL disposition is
// resolved and while the writer lock is still held, so no callback return or
// caller claim can manufacture it. It grants nothing: a caller compares it,
// for example to learn whether a later command of the same request follows its
// own last commit. The zero value is ready; a receipt serves one Command.
type CommandReceipt struct {
	state     atomic.Uint32
	authority atomic.Pointer[replication.Authority]
}

const (
	receiptFresh uint32 = iota
	receiptActive
	receiptRolledBack
	receiptUnchanged
	receiptChanged
	receiptUnknown
)

type commandReceiptKey struct{}

// WithCommandReceipt asks the next Command that runs under ctx to report its
// completion to r. A receipt that already served a command makes that Command
// refuse with replication.ErrInvalid before it opens a transaction.
func WithCommandReceipt(ctx context.Context, r *CommandReceipt) context.Context {
	return context.WithValue(ctx, commandReceiptKey{}, r)
}

// Changed reports that the command committed a change of the canonical image,
// with the exact authority that this commit installed. A no-op, a commit of
// node-local metadata alone and a rollback all report false.
func (r *CommandReceipt) Changed() (replication.Authority, bool) {
	if r == nil || r.state.Load() != receiptChanged {
		return replication.Authority{}, false
	}
	a := r.authority.Load()
	if a == nil {
		return replication.Authority{}, false
	}
	return *a, true
}

// Unchanged reports a confirmed successful completion that left the canonical
// image as it was: a no-op, or a commit of node-local metadata alone. A
// rollback reports false here and in Changed.
func (r *CommandReceipt) Unchanged() bool {
	return r != nil && r.state.Load() == receiptUnchanged
}

// Unknown reports that a transaction was opened and the SQL driver could not
// confirm whether it committed or rolled back. A receipt whose command never
// opened a transaction reports false, as do all confirmed outcomes.
func (r *CommandReceipt) Unknown() bool {
	if r == nil {
		return false
	}
	state := r.state.Load()
	return state == receiptUnknown || state == receiptActive
}

// claim binds the receipt to the one command that is about to open its
// transaction. The caller holds the writer lock.
func (r *CommandReceipt) claim() bool {
	return r != nil && r.state.CompareAndSwap(receiptFresh, receiptActive)
}

// complete records the resolved disposition. The caller holds the writer lock
// and has already published the authority of a committed change.
func (r *CommandReceipt) complete(s *Store, boundary anchorSQL, changed bool) {
	switch {
	case boundary.outcome == AnchorCommitted && changed:
		a := s.Authority()
		r.authority.Store(&a)
		r.state.Store(receiptChanged)
	case boundary.outcome == AnchorCommitted, boundary.outcome == AnchorUnchanged:
		r.state.Store(receiptUnchanged)
	case boundary.outcome == AnchorRolledBack:
		r.state.Store(receiptRolledBack)
	default:
		r.state.Store(receiptUnknown)
	}
}
