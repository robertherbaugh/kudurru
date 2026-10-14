package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func TestCommandTransactionReadsWritesStatusSectionsAndOneRevision(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	before := imageOf(t, s)
	var retained *CommandTx
	err := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
		retained = tx
		out, err := tx.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI})
		if err != nil {
			return err
		}
		z := out.(*v1alpha1.DNSZone)
		z.Status.Serial = 2026101401
		if err := tx.UpdateStatus(ctx, z); err != nil {
			return err
		}
		var current v1alpha1.DNSZone
		if err := tx.Get(ctx, store.KeyOf(z), &current); err != nil {
			return err
		}
		if current.Status.Serial != 2026101401 {
			t.Fatal("tx read missed staged status")
		}
		listed, err := tx.List(ctx, "DNSZone", store.ListOptions{Limit: 1})
		if err != nil {
			return err
		}
		if len(listed.Items) != 1 {
			t.Fatal("tx List missed staged object")
		}
		if err := tx.PutSection(ctx, "dedup", replication.Entry{Key: "command", Data: json.RawMessage(`{"result":"sealed"}`)}); err != nil {
			return err
		}
		rows, err := tx.ListSections(ctx, "dedup")
		if err != nil {
			return err
		}
		if len(rows) != 1 {
			t.Fatal("tx section read missed staged dedup")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	image := imageOf(t, s)
	if image.Revision != before.Revision+1 || image.ObjectResourceVersion != 2 || len(image.Payload.Dedup) != 1 {
		t.Fatal("command did not own one complete revision")
	}
	if _, err := retained.List(ctx, "DNSZone", store.ListOptions{}); !errors.Is(err, ErrTransactionEnded) {
		t.Fatalf("retained tx was usable: %v", err)
	}
	err = s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
		return tx.Delete(ctx, store.Key{Kind: "DNSZone", Name: "home"}, store.DeleteOptions{Manager: store.ManagerAPI})
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(imageOf(t, s).Payload.Objects) != 0 {
		t.Fatal("tx delete did not use normal delete path")
	}
}

func TestCommandRollbackIgnoredMutationErrorAndReentry(t *testing.T) {
	for _, kind := range []string{"callback", "ignored", "reentry", "ignored-reentry"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			s := openTemp(t)
			before := imageOf(t, s)
			err := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
				if _, err := tx.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
					return err
				}
				if err := tx.PutSection(ctx, "dedup", replication.Entry{Key: "command", Data: json.RawMessage(`{}`)}); err != nil {
					return err
				}
				switch kind {
				case "callback":
					return errors.New("domain failed")
				case "ignored":
					_ = tx.PutSection(ctx, "settings", replication.Entry{Key: "bad", Data: json.RawMessage(`{}`)})
					return nil
				default:
					_, err := s.Apply(ctx, zone("reentered", "reentered.example."), store.ApplyOptions{Manager: store.ManagerAPI})
					if kind == "ignored-reentry" {
						return nil
					}
					return err
				}
			})
			if err == nil {
				t.Fatal("failed command committed")
			}
			if !imageOf(t, s).Generation().Same(before.Generation()) {
				t.Fatal("partial object/dedup transaction committed")
			}
			audit, err := s.ReadAudit(ctx, AuditQuery{})
			if err != nil {
				t.Fatal(err)
			}
			if len(audit) != 0 {
				t.Fatal("rolled-back mutation audit leaked")
			}
		})
	}
}

func TestCommandFencesNoopsAndChecksTermBeforeCallback(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	token, err := s.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	term := s.Authority().Term
	wrong := term
	wrong.Epoch++
	called := false
	if err := s.Command(ctx, wrong, func(context.Context, *CommandTx) error { called = true; return nil }); !errors.Is(err, replication.ErrCAS) || called {
		t.Fatalf("term CAS: %v called=%v", err, called)
	}
	if _, err := s.EnablePrimary(ctx, imageOf(t, s).Generation()); err != nil {
		t.Fatal(err)
	}
	if err := s.Command(WithWriterToken(ctx, token), term, func(context.Context, *CommandTx) error { called = true; return nil }); !errors.Is(err, replication.ErrFenced) || called {
		t.Fatalf("stale incarnation no-op: %v called=%v", err, called)
	}
	fresh, err := s.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	term.Epoch++
	term.PrimaryID = "other"
	term.PromotionID = "other-term"
	if _, err := s.ObserveAuthority(ctx, term); err != nil {
		t.Fatal(err)
	}
	if err := s.Command(WithWriterToken(ctx, fresh), term, func(context.Context, *CommandTx) error { called = true; return nil }); !errors.Is(err, replication.ErrFenced) || called {
		t.Fatalf("fenced no-op: %v called=%v", err, called)
	}
}

func TestCommandDedupStatementFailureRollsBackObjectAuditAndCounters(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	before := imageOf(t, s)
	if _, e := s.w.Exec(`CREATE TRIGGER fail_dedup BEFORE INSERT ON replication_sections WHEN NEW.section='dedup' BEGIN SELECT RAISE(ABORT,'injected'); END`); e != nil {
		t.Fatal(e)
	}
	e := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
		if _, e := tx.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI}); e != nil {
			return e
		}
		return tx.PutSection(ctx, "dedup", replication.Entry{Key: "command", Data: json.RawMessage(`{"done":true}`)})
	})
	if e == nil || !imageOf(t, s).Generation().Same(before.Generation()) {
		t.Fatal("dedup statement failure committed domain data/counters")
	}
	audit, e := s.ReadAudit(ctx, AuditQuery{})
	if e != nil {
		t.Fatal(e)
	}
	if len(audit) != 0 {
		t.Fatal("dedup failure leaked audit")
	}
}
