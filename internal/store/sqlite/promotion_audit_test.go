package sqlite

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func TestPromotionAuditAtomicCompletionAndRestart(t *testing.T) {
	for _, failure := range []string{"none", "audit", "final-proof"} {
		t.Run(failure, func(t *testing.T) {
			p := openTemp(t, internalOptions)
			m := mirror(t, p)
			base := copyReplica(t, m, p)
			before := m.Authority()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if failure == "audit" {
				if _, err := m.w.Exec(`CREATE TRIGGER reject_promotion_audit BEFORE INSERT ON audit BEGIN SELECT RAISE(ABORT,'promotion audit test'); END`); err != nil {
					t.Fatal(err)
				}
			}
			checks := 0
			refusal := errors.New("original proof expired")
			check := func(context.Context) error {
				checks++
				if failure == "final-proof" && checks == 2 {
					return refusal
				}
				return nil
			}
			req := replication.PromoteRequest{ExpectedTerm: base.Term, ExpectedInstalled: base, RequestID: "audited-promotion"}
			attribution := AuditEntry{Actor: "User:operator", Authz: "admin", Reason: "reviewed primary loss", Action: "forged", Result: "forged"}
			result, err := m.PromoteAudited(ctx, req, check, attribution)
			if failure != "none" {
				if err == nil || !reflect.DeepEqual(before, m.Authority()) || !imageOf(t, m).Generation().Same(base) {
					t.Fatal("failed promotion changed durable state", err)
				}
				if failure == "final-proof" && !errors.Is(err, refusal) {
					t.Fatal("lost original final refusal", err)
				}
				if failure == "audit" {
					var code interface{ Code() int }
					if !errors.As(err, &code) || code.Code() != 1811 || checks != 1 {
						t.Fatal("audit target not reached", err, checks)
					}
				}
			} else {
				if err != nil || result.Mode != replication.Primary {
					t.Fatal("promotion failed", err)
				}
				again, err := m.PromoteAudited(ctx, req, check, attribution)
				if err != nil || !reflect.DeepEqual(again, result) {
					t.Fatal("duplicate changed original result", err)
				}
			}
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := openAt(t, m.path, internalOptions)
			entries, err := reopened.ReadAudit(ctx, AuditQuery{})
			if err != nil {
				t.Fatal(err)
			}
			var found []AuditEntry
			for _, e := range entries {
				if e.RequestID == req.RequestID {
					found = append(found, e)
				}
			}
			if failure != "none" {
				if len(found) != 0 || !reflect.DeepEqual(before, reopened.Authority()) {
					t.Fatal("rollback left audit/authority after restart")
				}
			} else {
				if len(found) != 1 || found[0].Action != "cluster.promote" || found[0].Actor != attribution.Actor || found[0].Reason != attribution.Reason || found[0].Result != AuditSuccess || !reflect.DeepEqual(result, reopened.Authority()) {
					t.Fatal("missing canonical atomic audit after restart", found)
				}
				if err := reopened.VerifyAudit(ctx); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestPromotionAuditCommitErrorNeverClaimsCompletion(t *testing.T) {
	p := openTemp(t, internalOptions)
	m := mirror(t, p)
	base := copyReplica(t, m, p)
	before := m.Authority()
	for _, statement := range []string{
		`CREATE TEMP TABLE promotion_parent(id INTEGER PRIMARY KEY)`,
		`CREATE TEMP TABLE promotion_child(parent INTEGER REFERENCES promotion_parent(id) DEFERRABLE INITIALLY DEFERRED)`,
		`CREATE TEMP TRIGGER promotion_unknown AFTER INSERT ON main.audit BEGIN INSERT INTO promotion_child(parent) VALUES (1); END`,
	} {
		if _, err := m.w.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	checks := 0
	_, err := m.PromoteAudited(ctx, replication.PromoteRequest{ExpectedTerm: base.Term, ExpectedInstalled: base, RequestID: "unknown-commit"}, func(context.Context) error { checks++; return nil }, AuditEntry{Actor: "User:operator", Authz: "admin", Reason: "reviewed"})
	var failure interface{ Code() int }
	if !errors.Is(err, ErrPromotionCompletionUnknown) || !errors.As(err, &failure) || failure.Code() != 787 || checks != 2 {
		t.Fatal("actual commit refusal lost unknown outcome", err, checks)
	}
	if !reflect.DeepEqual(m.Authority(), before) {
		t.Fatal("unknown completion published authority")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openAt(t, m.path, internalOptions)
	if !reflect.DeepEqual(reopened.Authority(), before) || !imageOf(t, reopened).Generation().Same(base) {
		t.Fatal("failed driver commit exposed a partial transition after reopen")
	}
	entries, err := reopened.ReadAudit(ctx, AuditQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.RequestID == "unknown-commit" {
			t.Fatal("failed commit exposed promotion audit")
		}
	}
}
