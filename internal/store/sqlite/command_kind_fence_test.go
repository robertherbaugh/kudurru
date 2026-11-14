package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func TestKindFencePreservesValidatedPostimageAndPermitsUnrelatedWork(t *testing.T) {
	for _, mode := range []string{"empty", "intended before fence", "unchanged apply", "unrelated kind and section"} {
		t.Run(mode, func(t *testing.T) {
			s := openTemp(t)
			before := imageOf(t, s)
			err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
				var intended store.Object
				if mode == "intended before fence" || mode == "unchanged apply" {
					var err error
					intended, err = tx.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI})
					if err != nil {
						return err
					}
					// The typed owner would validate its complete intended rows here.
					var checked public.DNSZone
					if err := tx.Get(ctx, store.KeyOf(intended), &checked); err != nil {
						return err
					}
				}
				if err := tx.RequireKindUnchanged(ctx, "DNSZone"); err != nil {
					return err
				}
				if mode == "unchanged apply" {
					if _, err := tx.Apply(ctx, intended, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
						return err
					}
				}
				if mode == "unrelated kind and section" {
					if _, err := tx.Apply(ctx, clientGroup("default", "clients"), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
						return err
					}
					return tx.PutSection(ctx, "dedup", replication.Entry{Key: "receipt", Data: json.RawMessage(`{"persisted":true}`)})
				}
				return nil
			})
			if err != nil {
				t.Fatal("valid kind postimage was refused", err)
			}
			after := imageOf(t, s)
			if mode == "empty" {
				if !after.Generation().Same(before.Generation()) {
					t.Fatal("empty fence wrote a generation")
				}
			} else if after.Revision != before.Revision+1 {
				t.Fatal("fence changed revision semantics")
			}
		})
	}
}

func TestKindFenceLateMutationRollsBackWholeOriginalCommand(t *testing.T) {
	for _, mode := range []string{"create from empty journal", "change", "other row", "delete", "status", "finalizer", "ABA", "second fence cannot disarm", "journal reallocation"} {
		t.Run(mode, func(t *testing.T) {
			s := openTemp(t)
			before := imageOf(t, s)
			err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
				var intended *public.DNSZone
				if mode != "create from empty journal" {
					obj, err := tx.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI})
					if err != nil {
						return err
					}
					intended = obj.(*public.DNSZone)
				}
				if err := tx.RequireKindUnchanged(ctx, "DNSZone"); err != nil {
					return err
				}
				if err := tx.PutSection(ctx, "dedup", replication.Entry{Key: "receipt", Data: json.RawMessage(`{"persisted":true}`)}); err != nil {
					return err
				}
				switch mode {
				case "create from empty journal", "other row":
					_, err := tx.Apply(ctx, zone("other", "other.example."), store.ApplyOptions{Manager: store.ManagerAPI})
					return err
				case "delete":
					return tx.Delete(ctx, store.KeyOf(intended), store.DeleteOptions{Manager: store.ManagerAPI})
				case "status":
					intended.Status.Serial = 42
					return tx.UpdateStatus(ctx, intended)
				case "finalizer":
					intended.Finalizers = []string{"kudurru.io/test"}
					_, err := tx.Apply(ctx, intended, store.ApplyOptions{Manager: store.ManagerAPI})
					return err
				case "journal reallocation":
					for i := range 64 {
						if _, err := tx.Apply(ctx, clientGroup("default", fmt.Sprintf("group-%d", i)), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
							return err
						}
					}
				}
				intended.Labels = map[string]string{"changed": "true"}
				changed, err := tx.Apply(ctx, intended, store.ApplyOptions{Manager: store.ManagerAPI})
				if err != nil {
					return err
				}
				if mode == "ABA" {
					restored := changed.(*public.DNSZone)
					restored.Labels = nil
					_, err = tx.Apply(ctx, restored, store.ApplyOptions{Manager: store.ManagerAPI})
					return err
				}
				if mode == "second fence cannot disarm" {
					return tx.RequireKindUnchanged(ctx, "DNSZone")
				}
				return nil
			})
			if !errors.Is(err, replication.ErrCAS) {
				t.Fatal("later protected mutation escaped its original fence", err)
			}
			if !imageOf(t, s).Generation().Same(before.Generation()) {
				t.Fatal("fence failure leaked rows, sections or revision")
			}
			audit, err := s.ReadAudit(t.Context(), AuditQuery{})
			if err != nil || len(audit) != 0 {
				t.Fatal("fence failure leaked audit", err)
			}
		})
	}
}

func TestKindFenceRegistrationRefusesAndPoisonsIgnoredFailure(t *testing.T) {
	for _, mode := range []string{"nil context", "unknown kind", "canceled context", "capacity", "ended in check"} {
		t.Run(mode, func(t *testing.T) {
			s := openTemp(t)
			before := imageOf(t, s)
			var retained *CommandTx
			err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
				retained = tx
				if _, err := tx.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
					return err
				}
				switch mode {
				case "nil context":
					_ = tx.RequireKindUnchanged(nil, "DNSZone") //nolint:staticcheck // Intentionally prove that an ignored nil-context registration poisons the command.
				case "unknown kind":
					_ = tx.RequireKindUnchanged(ctx, "UnknownKind")
				case "canceled context":
					canceled, cancel := context.WithCancel(ctx)
					cancel()
					_ = tx.RequireKindUnchanged(canceled, "DNSZone")
				case "capacity":
					for range MaxCommitChecks {
						if err := tx.RequireCommitCheck(func(context.Context) error { return nil }); err != nil {
							return err
						}
					}
					_ = tx.RequireKindUnchanged(ctx, "DNSZone")
				case "ended in check":
					return tx.RequireCommitCheck(func(ctx context.Context) error { _ = tx.RequireKindUnchanged(ctx, "DNSZone"); return nil })
				}
				return nil
			})
			if err == nil || !imageOf(t, s).Generation().Same(before.Generation()) {
				t.Fatal("ignored invalid fence registration committed", err)
			}
			if err := retained.RequireKindUnchanged(t.Context(), "DNSZone"); !errors.Is(err, ErrTransactionEnded) {
				t.Fatal("ended command registered a fence", err)
			}
		})
	}
	var absent *CommandTx
	if err := absent.RequireKindUnchanged(context.Background(), "DNSZone"); !errors.Is(err, ErrTransactionEnded) {
		t.Fatal("nil command registered a fence", err)
	}
}
