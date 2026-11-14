package sqlite

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func TestPromotionFinalCheckRollsBackAndSurvivesRestart(t *testing.T) {
	for _, cancelAtCheck := range []bool{false, true} {
		t.Run(map[bool]string{false: "revoked", true: "canceled"}[cancelAtCheck], func(t *testing.T) {
			p := openTemp(t, internalOptions)
			mustApply(t, p, zone("before", "before.example."), store.ManagerAPI)
			m := mirror(t, p)
			base := copyReplica(t, m, p)
			before := m.Authority()
			beforeImage := imageOf(t, m)
			beforeCompacted := m.compacted.Load()
			generationChanged := m.GenerationChanges()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			refusal := errors.New("original operator proof revoked")
			checks := 0
			req := replication.PromoteRequest{ExpectedTerm: base.Term, ExpectedInstalled: base, RequestID: "original-promotion"}
			a, err := m.PromoteChecked(ctx, req, func(original context.Context) error {
				if original != ctx {
					t.Error("promotion replaced the original context")
				}
				checks++
				if checks == 2 {
					if cancelAtCheck {
						cancel()
						return nil // cancellation cannot be hidden by callback success
					}
					return refusal
				}
				return nil
			})
			want := refusal
			if cancelAtCheck {
				want = context.Canceled
			}
			if !errors.Is(err, want) || checks != 2 || !reflect.DeepEqual(a, before) {
				t.Fatal("final refusal was not preserved", err, checks, a)
			}
			if !reflect.DeepEqual(m.Authority(), before) || !samePromotionImage(imageOf(t, m), beforeImage) || m.compacted.Load() != beforeCompacted {
				t.Fatal("refused promotion changed authority, image or feed")
			}
			select {
			case <-generationChanged:
				t.Fatal("rollback published a generation event")
			default:
			}
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := openAt(t, m.path, internalOptions)
			if !reflect.DeepEqual(reopened.Authority(), before) || !samePromotionImage(imageOf(t, reopened), beforeImage) {
				t.Fatal("reopen exposed a partially promoted authority or image")
			}
		})
	}
}

func TestPromotionChecksIdempotentSuccessAndFiniteInput(t *testing.T) {
	p := openTemp(t, internalOptions)
	m := mirror(t, p)
	base := copyReplica(t, m, p)
	req := replication.PromoteRequest{ExpectedTerm: base.Term, ExpectedInstalled: base, RequestID: "one-term"}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	allowed := func(ctx context.Context) error { return ctx.Err() }
	for _, bad := range []struct {
		ctx   context.Context
		check func(context.Context) error
	}{{nil, allowed}, {context.Background(), allowed}, {ctx, nil}} {
		if _, err := m.PromoteChecked(bad.ctx, req, bad.check); !errors.Is(err, replication.ErrInvalid) {
			t.Fatal("missing finite context or check admitted", err)
		}
	}
	a, err := m.PromoteChecked(ctx, req, allowed)
	if err != nil || a.Mode != replication.Primary || a.Term.Epoch != base.Term.Epoch+1 {
		t.Fatal("valid authorization failed promotion", err, a)
	}
	checks := 0
	refusal := errors.New("expired during idempotent check")
	again, err := m.PromoteChecked(ctx, req, func(context.Context) error {
		checks++
		if checks == 2 {
			return refusal
		}
		return nil
	})
	if !errors.Is(err, refusal) || checks != 2 || !reflect.DeepEqual(again, a) || !reflect.DeepEqual(m.Authority(), a) {
		t.Fatal("idempotent success bypassed final authorization", err, checks)
	}
	again, err = m.PromoteChecked(ctx, req, allowed)
	if err != nil || !reflect.DeepEqual(again, a) {
		t.Fatal("authorized idempotent retry allocated another term", err)
	}
}

func TestPromotionSQLFailurePrecedesFinalAuthorization(t *testing.T) {
	p := openTemp(t, internalOptions)
	m := mirror(t, p)
	base := copyReplica(t, m, p)
	before := m.Authority()
	// This is the last actual mutation, after authority and feed deletion. A
	// rejected statement must never call the final success authorization check.
	if _, err := m.w.Exec(`CREATE TRIGGER reject_promotion_feed BEFORE UPDATE ON meta WHEN NEW.key='feed_compacted_through' BEGIN SELECT RAISE(ABORT,'promotion test'); END`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	checks := 0
	_, err := m.PromoteChecked(ctx, replication.PromoteRequest{ExpectedTerm: base.Term, ExpectedInstalled: base, RequestID: "sql-fault"}, func(context.Context) error { checks++; return nil })
	var constraint interface{ Code() int }
	if !errors.As(err, &constraint) || constraint.Code() != 1811 || checks != 1 {
		t.Fatal("did not reach final SQL mutation before authorization", err, checks)
	}
	if !reflect.DeepEqual(m.Authority(), before) || !imageOf(t, m).Generation().Same(base) {
		t.Fatal("SQL refusal left a partial promotion")
	}
}

func samePromotionImage(a, b *replication.Image) bool {
	// ReadGeneration gives each transport envelope its own creation time.
	a.CreatedAt, b.CreatedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(a, b)
}
