//go:build unix

package persistence_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// A member command's acknowledgment leaves under the writer exclusion of its
// own commit. A writer queued behind the command commits only after the output
// has returned, so it cannot turn a committed command into an unanswered one.
func TestMemberCommandWithOutputExcludesWritersThroughAcknowledgment(t *testing.T) {
	f := newMemberFixture(t)
	ctx := finite(t)
	r := f.capture(ctx, t)
	copyRequest := *r
	before := image(t, f.s)
	otherCtx, term := carried(t, f.s), f.s.Authority().Term
	other := make(chan error, 1)
	var emitted atomic.Bool
	sawEmitted := false
	committed, err := f.s.MemberCommandWithOutput(ctx, r, func(ctx context.Context, tx *sqlite.CommandTx) error {
		// Like a status reconciler: another writer arrives while this command
		// holds the writer exclusion.
		go func() {
			other <- f.s.Command(otherCtx, term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				sawEmitted = emitted.Load()
				return tx.PutSection(ctx, "runtime", replication.Entry{Key: "other", Data: []byte(`{}`)})
			})
		}()
		return tx.PutSection(ctx, "dedup", replication.Entry{Key: "acknowledged-member-command", Data: []byte(`{"ok":true}`)})
	}, func(scope context.Context) error {
		if err := r.CheckCommandOutput(scope); err != nil {
			return err
		}
		// The command's own commit is the installed generation, and stays it.
		completed := f.s.Authority().Installed
		if completed.Revision != before.Revision+1 {
			return errors.New("output entered before the command's own completion")
		}
		// Give the queued writer every chance to interpose.
		select {
		case err := <-other:
			t.Error("another writer completed before the acknowledgment returned", err)
		case <-time.After(100 * time.Millisecond):
		}
		deadline, finite := scope.Deadline()
		bound, _ := ctx.Deadline()
		if !finite || deadline.After(bound) {
			return errors.New("output renewed original bound")
		}
		if err := r.CheckCommandOutput(scope); err != nil {
			return err
		}
		if !f.s.Authority().Installed.Same(completed) {
			return errors.New("generation advanced under the acknowledgment")
		}
		emitted.Store(true)
		return nil
	})
	if err != nil || !committed || !emitted.Load() {
		t.Fatal("committed member command was not acknowledged", committed, err)
	}
	select {
	case err := <-other:
		if err != nil || !sawEmitted {
			t.Fatal("queued writer did not follow the acknowledgment", sawEmitted, err)
		}
	case <-ctx.Done():
		t.Fatal("queued writer never proceeded after the acknowledgment")
	}
	if image(t, f.s).Revision != before.Revision+2 {
		t.Fatal("wrong durable outcome")
	}
	called := false
	if err := r.WithCommandOutput(func(context.Context) error { called = true; return nil }); err == nil || called {
		t.Fatal("acknowledged request emitted again", err)
	}
	if ok, err := f.s.MemberCommandWithOutput(ctx, &copyRequest, func(context.Context, *sqlite.CommandTx) error { called = true; return nil }, func(context.Context) error { called = true; return nil }); err == nil || ok || called {
		t.Fatal("copied handle repeated the command or its output", ok, err)
	}
}

// The two-phase form releases the writer between completion and output, and
// keeps refusing an output that follows any other commit.
func TestMemberCommandThenOutputStillRefusesAfterAnotherCommit(t *testing.T) {
	f := newMemberFixture(t)
	ctx := finite(t)
	r := f.capture(ctx, t)
	if err := f.s.MemberCommand(ctx, r, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return tx.PutSection(ctx, "dedup", replication.Entry{Key: "member-command", Data: []byte(`{"ok":true}`)})
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Command(carried(t, f.s), f.s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return tx.PutSection(ctx, "runtime", replication.Entry{Key: "other", Data: []byte(`{}`)})
	}); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := r.WithCommandOutput(func(context.Context) error { called = true; return nil }); !errors.Is(err, replication.ErrCAS) || called {
		t.Fatal("output followed another commit", err)
	}
}

func TestMemberCommandWithOutputSeparatesCompletionFromOutput(t *testing.T) {
	failed := errors.New("transport failed")
	refusal := errors.New("domain proof expired")
	for _, mode := range []string{"noop", "output-error", "output-cancel", "refusal", "final-original-cancel", "image-request", "revoked", "nil-output"} {
		t.Run(mode, func(t *testing.T) {
			f := newMemberFixture(t)
			originalCtx, cancel := context.WithCancel(finite(t))
			defer cancel()
			r := f.capture(originalCtx, t)
			before := image(t, f.s)
			switch mode {
			case "image-request":
				if _, err := r.ReadImage(); err != nil {
					t.Fatal(err)
				}
			case "revoked":
				if err := f.revoke(t); err != nil {
					t.Fatal(err)
				}
				before = image(t, f.s)
			}
			entered, emitted := false, false
			emit := func(scope context.Context) error {
				emitted = true
				switch mode {
				case "output-error":
					return failed
				case "output-cancel":
					cancel()
					<-scope.Done()
					return scope.Err()
				}
				return r.CheckCommandOutput(scope)
			}
			if mode == "nil-output" {
				emit = nil
			}
			committed, err := f.s.MemberCommandWithOutput(finite(t), r, func(ctx context.Context, tx *sqlite.CommandTx) error {
				entered = true
				switch mode {
				case "noop":
					return nil
				case "refusal":
					if err := tx.PutSection(ctx, "dedup", replication.Entry{Key: "member-result", Data: []byte(`{}`)}); err != nil {
						return err
					}
					return tx.RequireCommitCheck(func(context.Context) error { return refusal })
				case "final-original-cancel":
					if err := tx.PutSection(ctx, "dedup", replication.Entry{Key: "member-result", Data: []byte(`{}`)}); err != nil {
						return err
					}
					return tx.RequireCommitCheck(func(context.Context) error { cancel(); return nil })
				}
				return tx.PutSection(ctx, "dedup", replication.Entry{Key: "member-result", Data: []byte(`{}`)})
			}, emit)
			changed := !image(t, f.s).Generation().Same(before.Generation())
			switch mode {
			case "noop":
				if err != nil || !committed || !emitted || changed {
					t.Fatal("actual no-op completion was not acknowledged", committed, emitted, changed, err)
				}
			case "output-error":
				if !errors.Is(err, failed) || !committed || !emitted || !changed {
					t.Fatal("transport failure replaced completion", committed, emitted, changed, err)
				}
			case "output-cancel":
				if !errors.Is(err, context.Canceled) || !committed || !emitted || !changed {
					t.Fatal("canceled output replaced completion", committed, emitted, changed, err)
				}
			case "refusal":
				if !errors.Is(err, refusal) || committed || emitted || changed {
					t.Fatal("refused command completed or emitted", committed, emitted, changed, err)
				}
			case "final-original-cancel":
				if !errors.Is(err, context.Canceled) || committed || emitted || changed {
					t.Fatal("canceled command completed or emitted", committed, emitted, changed, err)
				}
			default:
				if err == nil || committed || entered || emitted || changed {
					t.Fatal("inadmissible request entered a command or its output", mode, committed, entered, emitted, err)
				}
			}
			called := false
			if err := r.WithCommandOutput(func(context.Context) error { called = true; return nil }); err == nil || called {
				t.Fatal("consumed request issued another output", err)
			}
			if err := r.WithOutput(func(context.Context) error { called = true; return nil }); mode != "image-request" && (err == nil || called) {
				t.Fatal("consumed request issued an image output", err)
			}
		})
	}
}
