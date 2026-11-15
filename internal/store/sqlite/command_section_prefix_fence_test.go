package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func prefixFenceRow(key string, changed bool) replication.Entry {
	data := json.RawMessage(`{"version":1}`)
	if changed {
		data = json.RawMessage(`{"version":2}`)
	}
	return replication.Entry{Key: key, Data: data}
}

func TestSectionPrefixFencePermitsNoopsAndIndependentWork(t *testing.T) {
	for _, mode := range []string{"empty", "identical put", "canonical identical put", "absent delete", "other prefix", "same key other section", "object write", "intended before fence"} {
		t.Run(mode, func(t *testing.T) {
			s := openTemp(t)
			if mode != "empty" {
				if err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
					return tx.PutSection(ctx, "runtime", prefixFenceRow("protected/root", false))
				}); err != nil {
					t.Fatal(err)
				}
			}
			before := imageOf(t, s)
			err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
				if mode == "intended before fence" {
					if err := tx.PutSection(ctx, "runtime", prefixFenceRow("protected/root", true)); err != nil {
						return err
					}
				}
				if err := tx.RequireSectionPrefixUnchanged(ctx, "runtime", "protected/"); err != nil {
					return err
				}
				switch mode {
				case "identical put":
					return tx.PutSection(ctx, "runtime", prefixFenceRow("protected/root", false))
				case "canonical identical put":
					return tx.PutSection(ctx, "runtime", replication.Entry{Key: "protected/root", Data: json.RawMessage(" { \"version\" : 1 } ")})
				case "absent delete":
					return tx.DeleteSection(ctx, "runtime", "protected/missing")
				case "other prefix":
					return tx.PutSection(ctx, "runtime", prefixFenceRow("independent/root", true))
				case "same key other section":
					return tx.PutSection(ctx, "dedup", prefixFenceRow("protected/root", true))
				case "object write":
					_, err := tx.Apply(ctx, zone("independent", "independent.example."), store.ApplyOptions{Manager: store.ManagerAPI})
					return err
				}
				return nil
			})
			if err != nil {
				t.Fatal("valid postimage or independent work refused", err)
			}
			after := imageOf(t, s)
			switch mode {
			case "empty", "identical put", "canonical identical put", "absent delete":
				if !before.Generation().Same(after.Generation()) {
					t.Fatal("prefix fence/no-op wrote a revision")
				}
			default:
				if after.Revision != before.Revision+1 {
					t.Fatal("independent/intended work changed revision semantics")
				}
			}
		})
	}
}

func TestSectionPrefixFenceRejectsActualMutationABAAndLaterRegistration(t *testing.T) {
	for _, mode := range []string{"insert empty", "replace", "other row", "delete", "put ABA", "delete ABA", "insert delete ABA", "second registration", "registration growth", "overlapping prefixes"} {
		t.Run(mode, func(t *testing.T) {
			s := openTemp(t)
			if mode != "insert empty" {
				if err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
					return tx.PutSection(ctx, "runtime", prefixFenceRow("protected/root", false))
				}); err != nil {
					t.Fatal(err)
				}
			}
			before := imageOf(t, s)
			err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
				if err := tx.RequireSectionPrefixUnchanged(ctx, "runtime", "protected/"); err != nil {
					return err
				}
				// All unrelated work must also roll back on the final owner refusal.
				if _, err := tx.Apply(ctx, zone("rolled-back", "rolled-back.example."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
					return err
				}
				if err := tx.PutSection(ctx, "dedup", prefixFenceRow("independent/receipt", false)); err != nil {
					return err
				}
				if mode == "registration growth" {
					for i := 1; i < MaxCommitChecks; i++ {
						if err := tx.RequireSectionPrefixUnchanged(ctx, "runtime", fmt.Sprintf("independent-%d/", i)); err != nil {
							return err
						}
					}
				}
				if mode == "overlapping prefixes" {
					if err := tx.RequireSectionPrefixUnchanged(ctx, "runtime", "protected/root"); err != nil {
						return err
					}
				}
				switch mode {
				case "insert empty":
					return tx.PutSection(ctx, "runtime", prefixFenceRow("protected/root", false))
				case "other row":
					return tx.PutSection(ctx, "runtime", prefixFenceRow("protected/other", false))
				case "delete":
					return tx.DeleteSection(ctx, "runtime", "protected/root")
				case "delete ABA":
					if err := tx.DeleteSection(ctx, "runtime", "protected/root"); err != nil {
						return err
					}
					return tx.PutSection(ctx, "runtime", prefixFenceRow("protected/root", false))
				case "insert delete ABA":
					if err := tx.PutSection(ctx, "runtime", prefixFenceRow("protected/temporary", false)); err != nil {
						return err
					}
					return tx.DeleteSection(ctx, "runtime", "protected/temporary")
				}
				if err := tx.PutSection(ctx, "runtime", prefixFenceRow("protected/root", true)); err != nil {
					return err
				}
				if mode == "put ABA" || mode == "second registration" {
					if err := tx.PutSection(ctx, "runtime", prefixFenceRow("protected/root", false)); err != nil {
						return err
					}
				}
				if mode == "second registration" {
					return tx.RequireSectionPrefixUnchanged(ctx, "runtime", "protected/")
				}
				return nil
			})
			if !errors.Is(err, replication.ErrCAS) {
				t.Fatal("late prefix mutation escaped original fence", err)
			}
			if !before.Generation().Same(imageOf(t, s).Generation()) {
				t.Fatal("prefix refusal leaked owner/object/receipt/revision")
			}
			audit, err := s.ReadAudit(t.Context(), AuditQuery{})
			if err != nil || len(audit) != 0 {
				t.Fatal("prefix refusal leaked object audit", err)
			}
		})
	}
}

func TestSectionPrefixFenceLiteralUTF8Matching(t *testing.T) {
	for _, tc := range []struct {
		name, prefix, key string
		refused           bool
	}{
		{"literal matching", `owner%_\/`, `owner%_\/root`, true},
		{"percent is not wildcard", `owner%_\/`, `ownerX_\/root`, false},
		{"underscore is not wildcard", `owner%_\/`, `owner%X\/root`, false},
		{"backslash is literal", `owner%_\/`, `owner%_X/root`, false},
		{"unicode matching", "锚/", "锚/root", true},
		{"unicode independent", "锚/", "锚点/root", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTemp(t)
			err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
				if err := tx.RequireSectionPrefixUnchanged(ctx, "runtime", tc.prefix); err != nil {
					return err
				}
				return tx.PutSection(ctx, "runtime", prefixFenceRow(tc.key, false))
			})
			if tc.refused && !errors.Is(err, replication.ErrCAS) {
				t.Fatal("literal matching prefix escaped fence", err)
			}
			if !tc.refused && err != nil {
				t.Fatal("literal independent key refused", err)
			}
		})
	}
}

func TestSectionPrefixFenceInvalidRegistrationPoisonsWholeCommand(t *testing.T) {
	for _, mode := range []string{"nil context", "invalid section", "empty prefix", "long prefix", "invalid UTF8", "canceled context", "full budget", "mixed budget", "from final check"} {
		t.Run(mode, func(t *testing.T) {
			s := openTemp(t)
			before := imageOf(t, s)
			var retained *CommandTx
			err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
				retained = tx
				if err := tx.PutSection(ctx, "dedup", prefixFenceRow("pending/receipt", false)); err != nil {
					return err
				}
				switch mode {
				case "nil context":
					_ = tx.RequireSectionPrefixUnchanged(nil, "runtime", "protected/") //nolint:staticcheck // An ignored nil-context registration must poison the command.
				case "invalid section":
					_ = tx.RequireSectionPrefixUnchanged(ctx, "unknown", "protected/")
				case "empty prefix":
					_ = tx.RequireSectionPrefixUnchanged(ctx, "runtime", "")
				case "long prefix":
					_ = tx.RequireSectionPrefixUnchanged(ctx, "runtime", strings.Repeat("x", 513))
				case "invalid UTF8":
					_ = tx.RequireSectionPrefixUnchanged(ctx, "runtime", string([]byte{0xff}))
				case "canceled context":
					canceled, cancel := context.WithCancel(ctx)
					cancel()
					_ = tx.RequireSectionPrefixUnchanged(canceled, "runtime", "protected/")
				case "full budget":
					for range MaxCommitChecks {
						if err := tx.RequireCommitCheck(func(context.Context) error { return nil }); err != nil {
							return err
						}
					}
					_ = tx.RequireSectionPrefixUnchanged(ctx, "runtime", "protected/")
				case "mixed budget":
					for range MaxCommitChecks - 1 {
						if err := tx.RequireSectionPrefixUnchanged(ctx, "runtime", "protected/"); err != nil {
							return err
						}
					}
					if err := tx.RequireCommitCheck(func(context.Context) error { return nil }); err != nil {
						return err
					}
					_ = tx.RequireSectionPrefixUnchanged(ctx, "runtime", "protected/")
				case "from final check":
					return tx.RequireCommitCheck(func(ctx context.Context) error {
						_ = tx.RequireSectionPrefixUnchanged(ctx, "runtime", "protected/")
						return nil
					})
				}
				return nil
			})
			if err == nil || !before.Generation().Same(imageOf(t, s).Generation()) {
				t.Fatal("ignored registration failure committed")
			}
			if err := retained.RequireSectionPrefixUnchanged(t.Context(), "runtime", "protected/"); !errors.Is(err, ErrTransactionEnded) {
				t.Fatal("ended tx registered prefix fence", err)
			}
		})
	}
	var absent *CommandTx
	if err := absent.RequireSectionPrefixUnchanged(context.Background(), "runtime", "protected/"); !errors.Is(err, ErrTransactionEnded) {
		t.Fatal("nil tx registered prefix fence", err)
	}
}

func TestSectionPrefixFenceRequiresOriginalWriterForUnchangedCommand(t *testing.T) {
	s := openTemp(t)
	token, err := s.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	term := token.Term()
	if _, err := s.EnablePrimary(t.Context(), imageOf(t, s).Generation()); err != nil {
		t.Fatal(err)
	}
	called := false
	err = s.Command(WithWriterToken(t.Context(), token), term, func(ctx context.Context, tx *CommandTx) error {
		called = true
		return tx.RequireSectionPrefixUnchanged(ctx, "runtime", "protected/")
	})
	if !errors.Is(err, replication.ErrFenced) || called {
		t.Fatal("prefix fence substituted for original writer gate", err)
	}
}
