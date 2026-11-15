package sqlite

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// conflictedPrimary promotes a mirror and then records an authenticated
// equal-epoch term from another member, leaving durable Conflict authority.
func conflictedPrimary(t *testing.T) (*Store, replication.Term) {
	t.Helper()
	p := openTemp(t, internalOptions)
	m := mirror(t, p)
	base := copyReplica(t, m, p)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := m.Promote(ctx, replication.PromoteRequest{ExpectedTerm: base.Term, ExpectedInstalled: base, RequestID: "first-branch"}); err != nil {
		t.Fatal(err)
	}
	other := m.Authority().Term
	other.PrimaryID, other.PromotionID, other.PrimaryName = uuid.NewString(), uuid.NewString(), "other"
	if _, err := m.ObserveAuthority(ctx, other); !errors.Is(err, replication.ErrPromotionConflict) {
		t.Fatal("fixture did not enter durable conflict", err)
	}
	if a := m.Authority(); a.Mode != replication.Conflict || a.Conflict == nil || !a.Conflict.Same(other) || a.Writable() {
		t.Fatal("conflict authority not recorded", a)
	}
	return m, other
}

func resolutionInputs(t *testing.T, m *Store) (context.Context, *replication.Validated, replication.ResolveRequest) {
	t.Helper()
	ctx := captureReplica(t, m)
	image, err := m.ReadGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	chosen, err := m.ValidateImage(ctx, *image)
	if err != nil {
		t.Fatal(err)
	}
	a := m.Authority()
	return ctx, chosen, replication.ResolveRequest{ExpectedTerm: a.Term, ExpectedInstalled: image.Generation(), ExpectedConflict: a.Conflict, RequestID: "keep-local-branch"}
}

func TestResolutionAuditAtomicCompletionAndRestart(t *testing.T) {
	for _, failure := range []string{"none", "audit", "final-proof", "invalid"} {
		t.Run(failure, func(t *testing.T) {
			m, other := conflictedPrimary(t)
			before := m.Authority()
			kept := imageOf(t, m).Generation()
			if failure == "audit" {
				if _, err := m.w.Exec(`CREATE TRIGGER reject_resolution_audit BEFORE INSERT ON audit BEGIN SELECT RAISE(ABORT,'resolution audit test'); END`); err != nil {
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
			ctx, chosen, req := resolutionInputs(t, m)
			attribution := AuditEntry{Actor: "User:operator", Authz: "admin", Reason: "keep reviewed branch", Action: "forged", Result: "forged"}
			if failure == "invalid" {
				attribution.Reason = " "
			}
			result, err := m.ResolveAudited(ctx, chosen, req, check, attribution)
			if failure != "none" {
				if err == nil || !reflect.DeepEqual(before, m.Authority()) || !imageOf(t, m).Generation().Same(kept) {
					t.Fatal("failed resolution changed durable state", err)
				}
				if _, qerr := m.ReadQuarantined(t.Context(), kept); !errors.Is(qerr, store.ErrNotFound) {
					t.Fatal("refused resolution leaked quarantine", qerr)
				}
				switch failure {
				case "final-proof":
					if !errors.Is(err, refusal) || checks != 2 {
						t.Fatal("lost original final refusal", err, checks)
					}
				case "audit":
					var code interface{ Code() int }
					if !errors.As(err, &code) || code.Code() != 1811 || checks != 1 {
						t.Fatal("audit target not reached", err, checks)
					}
				case "invalid":
					if !errors.Is(err, replication.ErrInvalid) || checks != 0 {
						t.Fatal("invalid attribution reached the transaction", err, checks)
					}
				}
			} else {
				if err != nil || result.Mode != replication.Primary || !result.Writable() || result.Conflict != nil ||
					result.Term.Epoch != other.Epoch+1 || result.Term.PrimaryID != before.MemberID || result.Parent == nil || !result.Parent.Same(kept) {
					t.Fatal("resolution did not allocate a new epoch for the kept branch", err, result)
				}
				if _, err := m.ReadQuarantined(t.Context(), kept); err != nil {
					t.Fatal("prior branch not retained as private recovery evidence", err)
				}
				// The store-level duplicate rechecks authority without a second audit.
				replay := captureReplica(t, m)
				req.ExpectedInstalled = replay.Value(anchorInstallKey{}).(*anchorInstall).installed
				again, err := m.ResolveAudited(replay, chosen, req, check, attribution)
				if err != nil || !reflect.DeepEqual(again, result) {
					t.Fatal("duplicate changed original result", err)
				}
			}
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := openAt(t, m.path, internalOptions)
			entries, err := reopened.ReadAudit(t.Context(), AuditQuery{})
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
				return
			}
			if len(found) != 1 || found[0].Action != "cluster.resolve" || found[0].Actor != attribution.Actor || found[0].Reason != attribution.Reason ||
				found[0].Result != AuditSuccess || !reflect.DeepEqual(result, reopened.Authority()) {
				t.Fatal("missing canonical atomic audit after restart", found)
			}
			if err := reopened.VerifyAudit(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestResolutionAuditCommitErrorNeverClaimsCompletion(t *testing.T) {
	m, _ := conflictedPrimary(t)
	before := m.Authority()
	kept := imageOf(t, m).Generation()
	for _, statement := range []string{
		`CREATE TEMP TABLE resolution_parent(id INTEGER PRIMARY KEY)`,
		`CREATE TEMP TABLE resolution_child(parent INTEGER REFERENCES resolution_parent(id) DEFERRABLE INITIALLY DEFERRED)`,
		`CREATE TEMP TRIGGER resolution_unknown AFTER INSERT ON main.audit BEGIN INSERT INTO resolution_child(parent) VALUES (1); END`,
	} {
		if _, err := m.w.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	ctx, chosen, req := resolutionInputs(t, m)
	checks := 0
	_, err := m.ResolveAudited(ctx, chosen, req, func(context.Context) error { checks++; return nil }, AuditEntry{Actor: "User:operator", Authz: "admin", Reason: "reviewed"})
	var failure interface{ Code() int }
	if !errors.Is(err, ErrResolutionCompletionUnknown) || !errors.As(err, &failure) || failure.Code() != 787 || checks != 2 {
		t.Fatal("actual commit refusal lost unknown outcome", err, checks)
	}
	if !reflect.DeepEqual(m.Authority(), before) {
		t.Fatal("unknown completion published authority")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openAt(t, m.path, internalOptions)
	if !reflect.DeepEqual(reopened.Authority(), before) || !imageOf(t, reopened).Generation().Same(kept) {
		t.Fatal("failed driver commit exposed a partial transition after reopen")
	}
	entries, err := reopened.ReadAudit(t.Context(), AuditQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.RequestID == req.RequestID {
			t.Fatal("failed commit exposed resolution audit")
		}
	}
}

func TestResolutionAuditRefusesMismatchedConflictOperand(t *testing.T) {
	m, _ := conflictedPrimary(t)
	before := m.Authority()
	ctx, chosen, req := resolutionInputs(t, m)
	wrong := *req.ExpectedConflict
	wrong.PromotionID = uuid.NewString()
	req.ExpectedConflict = &wrong
	if _, err := m.ResolveAudited(ctx, chosen, req, func(context.Context) error { return nil }, AuditEntry{Actor: "User:operator", Authz: "admin", Reason: "reviewed"}); !errors.Is(err, replication.ErrCAS) {
		t.Fatal("stale conflict review resolved", err)
	}
	if !reflect.DeepEqual(before, m.Authority()) {
		t.Fatal("refused resolution changed authority")
	}
}
