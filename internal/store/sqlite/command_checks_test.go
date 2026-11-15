package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func TestCommitChecksRunAfterCallbackForWritesAndNoops(t *testing.T) {
	for _, change := range []bool{false, true} {
		t.Run(map[bool]string{false: "noop", true: "write"}[change], func(t *testing.T) {
			s := openTemp(t)
			before := imageOf(t, s)
			var events []string
			var retained *CommandTx
			err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
				retained = tx
				if err := tx.RequireCommitCheck(func(ctx context.Context) error {
					if _, err := tx.List(ctx, "DNSZone", store.ListOptions{}); !errors.Is(err, ErrTransactionEnded) {
						t.Error("final check could access transaction data")
					}
					events = append(events, "check1")
					return nil
				}); err != nil {
					return err
				}
				if change {
					if _, err := tx.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
						return err
					}
				}
				if err := tx.RequireCommitCheck(func(context.Context) error { events = append(events, "check2"); return nil }); err != nil {
					return err
				}
				events = append(events, "callback")
				return nil
			})
			if err != nil || !slices.Equal(events, []string{"callback", "check1", "check2"}) {
				t.Fatal("incorrect final check order", events, err)
			}
			wantRevision := before.Revision
			if change {
				wantRevision++
			}
			if imageOf(t, s).Revision != wantRevision {
				t.Fatal("checks changed revision semantics")
			}
			if err := retained.RequireCommitCheck(func(context.Context) error { t.Error("late check ran"); return nil }); !errors.Is(err, ErrTransactionEnded) {
				t.Fatal("retained transaction accepted a check", err)
			}
		})
	}
}

func TestCommitCheckDenialRollsBackWholeCommand(t *testing.T) {
	denied := errors.New("proof expired after callback work")
	for _, reason := range []string{"expired", "noop-expired", "nil", "capacity", "register-in-check", "reenter-in-check", "cancel-in-check", "callback-error"} {
		t.Run(reason, func(t *testing.T) {
			s := openTemp(t)
			before := imageOf(t, s)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			valid := true
			err := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
				if reason != "noop-expired" {
					if _, err := tx.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
						return err
					}
					if err := tx.PutSection(ctx, "dedup", replication.Entry{Key: "command", Data: json.RawMessage(`{"result":"ready"}`)}); err != nil {
						return err
					}
				}
				if err := tx.RequireCommitCheck(func(ctx context.Context) error {
					calls++
					switch reason {
					case "register-in-check":
						_ = tx.RequireCommitCheck(func(context.Context) error { return nil })
					case "reenter-in-check":
						_, _ = s.Apply(ctx, zone("forbidden", "forbidden.example."), store.ApplyOptions{Manager: store.ManagerAPI})
					case "cancel-in-check":
						cancel()
					default:
						if !valid {
							return denied
						}
					}
					return nil
				}); err != nil {
					return err
				}
				switch reason {
				case "nil":
					_ = tx.RequireCommitCheck(nil)
				case "capacity":
					for range MaxCommitChecks {
						_ = tx.RequireCommitCheck(func(context.Context) error { return nil })
					}
				case "callback-error":
					return denied
				}
				valid = false
				return nil
			})
			if err == nil {
				t.Fatal("denied final check committed")
			}
			wantCalls := 1
			if reason == "nil" || reason == "capacity" || reason == "callback-error" {
				wantCalls = 0
			}
			if calls != wantCalls {
				t.Fatal("check ran outside its commit boundary", calls)
			}
			if !imageOf(t, s).Generation().Same(before.Generation()) {
				t.Fatal("denial leaked object, receipt or revision")
			}
			audit, err := s.ReadAudit(t.Context(), AuditQuery{})
			if err != nil || len(audit) != 0 {
				t.Fatal("denial leaked audit rows", err)
			}
		})
	}
}
