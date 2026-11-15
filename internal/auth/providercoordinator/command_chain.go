package providercoordinator

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// ErrForeign refuses a chained command because a command of another caller
// changed canonical state, or ended with an unknown outcome, after the chain's
// previous command. Nothing of the refused command ran. It is never a reason
// to capture a newer source for the same request or to send the command again.
var ErrForeign = errors.New("providercoordinator: another command changed canonical state")

// commandMark is the Store's receipt of one command that changed canonical
// state through this coordinator, or the marker of one whose outcome is not
// known. Marks are compared by identity: each such command installs a new one.
type commandMark struct {
	authority replication.Authority
	unknown   bool
}

// Chain binds the commands of ONE request that carries several items (D378).
// The request's first item is admitted on its original source by root, like a
// single command. When one of its commands has completed, the chain is
// anchored: every later command of the request is admitted only while the last
// command that changed canonical state through this coordinator is still the
// one it was when the chain's previous command ended, which is the chain's own
// Store-issued commit receipt whenever that command changed state. The compare
// runs under the publication gate, so it is exact for every command that goes
// through this coordinator: public API mutations of any credential, member
// commands forwarded from followers, operator-started integration operations
// and background program steps. Writers that take only the Store lock or the
// primary pin, such as status reconciliation and the zone lifecycle, are the
// node's own follow-up work and do not move the mark.
//
// A Chain grants nothing. Root still carries the original writer, re-verifies
// the raw credential in every command's transaction and keeps the request's
// lifetime. A Chain is used by one goroutine, one command at a time.
type Chain struct {
	owner    *Coordinator
	busy     atomic.Bool
	anchored atomic.Bool
	at       atomic.Pointer[commandMark] // the mark every later command must still find
	mine     atomic.Pointer[commandMark] // the chain's own last state-changing command
}

type chainKey struct{}

// NewChain returns the unanchored chain of one request.
func (c *Coordinator) NewChain() *Chain { return &Chain{owner: c} }

// WithChain makes the commands that run under ctx members of chain.
func WithChain(ctx context.Context, chain *Chain) context.Context {
	return context.WithValue(ctx, chainKey{}, chain)
}

// Anchored reports that a command of this chain has completed. From then on
// root must not require the request's original lease to be the current
// publication: the chain's own commits retire it, and the exact compare in the
// coordinator takes its place.
func (ch *Chain) Anchored() bool { return ch != nil && ch.anchored.Load() }

// Receipt returns the authority that the chain's own last state-changing
// command installed, as the Store reported it. It is false while the chain is
// unanchored and while it is anchored on a command of the request that changed
// nothing.
func (ch *Chain) Receipt() (replication.Authority, bool) {
	if ch == nil || !ch.anchored.Load() {
		return replication.Authority{}, false
	}
	mark := ch.at.Load()
	if mark == nil || mark.unknown || !ch.own(mark) {
		return replication.Authority{}, false
	}
	return mark.authority, true
}

// own reports whether mark was installed by a command of this chain. Only such
// a mark carries the chain's own receipt; an anchor taken at a no-op carries
// the mark of whichever command changed state last before it.
func (ch *Chain) own(mark *commandMark) bool {
	return mark != nil && ch.mine.Load() == mark
}

// track opens one Store command of this coordinator. The caller holds the
// publication gate and must call done before releasing it, after the Store
// call has returned. track refuses a chained command that does not follow its
// chain's anchor, and returns the context that asks the Store for the
// command's own receipt; done installs the resulting mark. The gate makes the
// compare and the installation exact against every other tracked command.
func (c *Coordinator) track(ctx context.Context) (context.Context, func(), error) {
	chain, _ := ctx.Value(chainKey{}).(*Chain)
	if chain != nil {
		if chain.owner != c || !chain.busy.CompareAndSwap(false, true) {
			return nil, nil, ErrInvalid
		}
		if chain.anchored.Load() && chain.at.Load() != c.last.Load() {
			chain.busy.Store(false)
			return nil, nil, ErrForeign
		}
	}
	receipt := new(sqlite.CommandReceipt)
	done := func() {
		if authority, changed := receipt.Changed(); changed {
			mark := &commandMark{authority: authority}
			c.last.Store(mark)
			if chain != nil {
				chain.mine.Store(mark)
				chain.at.Store(mark)
				chain.anchored.Store(true)
			}
		} else if receipt.Unknown() {
			// Nobody may continue across a command that may or may not have
			// committed: this mark equals no chain's anchor.
			c.last.Store(&commandMark{unknown: true})
		} else if chain != nil && receipt.Unchanged() && !chain.anchored.Load() {
			// The request's first completed command changed nothing. Root
			// admitted it on the original source under this same gate, so the
			// chain is anchored on the state that source already contained.
			chain.at.Store(c.last.Load())
			chain.anchored.Store(true)
		}
		if chain != nil {
			chain.busy.Store(false)
		}
	}
	return sqlite.WithCommandReceipt(ctx, receipt), done, nil
}
