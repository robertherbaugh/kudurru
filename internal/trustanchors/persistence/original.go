// Package persistence stages managed-anchor history inside an already admitted
// SQLite Command. It opens no command, acquires no writer, and mounts no runtime.
// The caller retains original source/policy/publication/retirement exclusions
// through the ENTIRE outer Command, with its independent final admission gates.
// Every adapter error must be returned from that callback; no fallback/retry is
// provided here. Schema/codec success alone never supplies command authority.
//
// POSTIMAGE CONTRACT: Command does not run complete owner image validation.
// Each mutating entry validates the complete intended owner, stages its writes,
// then registers the driver's section-prefix fence. Unchanged InitializeDormant
// registers the same fence after complete validation. Any later actual mutation
// under runtime/trustanchors/ (including ABA) rejects the whole outer command.
// Independent owners may compose in that command, and genuinely unchanged
// writes remain no-ops; no later fence can disarm an earlier one. This mechanical
// postimage fence does not replace original evidence, authority, or caller pins.
package persistence

import (
	"context"
	"errors"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
)

// Fixed refusal categories carry neither credentials nor received DNS data.
var (
	ErrOriginal = errors.New("trustanchors/persistence: original command changed")
	ErrPhase    = errors.New("trustanchors/persistence: incompatible owner phase")
)

// Original retains the already-carried writer, context, expected complete owner
// and exact opaque clock BEFORE network acquisition. It grants no authority and
// does not replace the caller's captured source/policy/dependency admission.
// Reuse after refusal, deadline extension, and chaining staged progress are not
// supported. Capturing a successor writer cannot rescue an original operation.
type Original struct {
	ctx            context.Context
	writer         sqlite.WriterToken
	expected       accepted.Version
	clock          *trustanchors.SessionClock
	initialization bool
}

// CaptureInitialization retains only already-admitted startup authority; it
// neither samples Store.WriterToken nor creates a clock or owner history.
func CaptureInitialization(ctx context.Context) (Original, error) {
	return capture(ctx, accepted.Version{}, nil, true)
}

// CaptureTransition must run before acquisition, with the version read from an
// accepted durable owner and the very clock used to create the Observation.
// Missing carried tokens refuse; Store.Command's token fallback is insufficient.
func CaptureTransition(ctx context.Context, expected accepted.Version, clock *trustanchors.SessionClock) (Original, error) {
	if expected.Generation == 0 || expected.Digest == "" || clock == nil {
		return Original{}, ErrOriginal
	}
	return capture(ctx, expected, clock, false)
}

func capture(ctx context.Context, expected accepted.Version, clock *trustanchors.SessionClock, initialization bool) (Original, error) {
	if ctx == nil {
		return Original{}, ErrOriginal
	}
	token, ok := sqlite.WriterTokenFromContext(ctx)
	if !ok {
		return Original{}, ErrOriginal
	}
	o := Original{ctx: ctx, writer: token, expected: expected, clock: clock, initialization: initialization}
	if err := o.check(ctx); err != nil {
		return Original{}, err
	}
	return o, nil
}

func (o Original) check(ctx context.Context) error {
	if o.ctx == nil || ctx == nil {
		return ErrOriginal
	}
	if err := o.ctx.Err(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// A deadline is an independent bound even before context's timer fires.
	at := time.Now()
	for _, c := range []context.Context{o.ctx, ctx} {
		if deadline, ok := c.Deadline(); ok && !at.Before(deadline) {
			return context.DeadlineExceeded
		}
		token, ok := sqlite.WriterTokenFromContext(c)
		if !ok || token != o.writer {
			return ErrOriginal
		}
	}
	return nil
}

func (o Original) enter(ctx context.Context, tx *sqlite.CommandTx) error {
	if tx == nil {
		return sqlite.ErrTransactionEnded
	}
	if err := o.check(ctx); err != nil {
		return err
	}
	if !tx.Authority().Same(o.writer.Term()) || tx.Incarnation() != o.writer.Incarnation() {
		return ErrOriginal
	}
	return nil
}

func (o Original) requireFinal(tx *sqlite.CommandTx, observation *trustanchors.Observation) error {
	return tx.RequireCommitCheck(func(ctx context.Context) error {
		if err := o.check(ctx); err != nil {
			return err
		}
		if observation != nil {
			// Only retained immutable evidence/memory and the original actual clock
			// are consulted here. No tx, I/O, parsing, or dependency lock acquisition.
			return observation.CheckLifetime(o.clock.Sample())
		}
		return nil
	})
}
