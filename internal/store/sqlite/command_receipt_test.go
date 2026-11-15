package sqlite

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// The receipt is the Store's statement of the SQL disposition: only a commit
// that changed the canonical image yields an authority, and it is exactly the
// authority this commit installed, even when another writer commits afterwards.
func TestCommandReceiptStatesExactDisposition(t *testing.T) {
	s := openTemp(t)
	term := s.Authority().Term
	put := func(key string) func(context.Context, *CommandTx) error {
		return func(ctx context.Context, tx *CommandTx) error {
			return tx.PutSection(ctx, "dedup", replication.Entry{Key: key, Data: []byte(`{}`)})
		}
	}

	changed := new(CommandReceipt)
	if a, ok := changed.Changed(); ok || changed.Unchanged() || changed.Unknown() || !reflect.DeepEqual(a, replication.Authority{}) {
		t.Fatal("a fresh receipt claimed a completion")
	}
	before := s.Authority()
	if err := s.Command(WithCommandReceipt(t.Context(), changed), term, put("first")); err != nil {
		t.Fatal(err)
	}
	installed, ok := changed.Changed()
	if !ok || changed.Unchanged() || changed.Unknown() || !reflect.DeepEqual(installed, s.Authority()) || installed.Installed.Revision != before.Installed.Revision+1 {
		t.Fatalf("a committed change did not report the authority it installed: %+v", installed)
	}
	// A later commit by another writer does not move an issued receipt.
	if err := s.Command(t.Context(), term, put("second")); err != nil {
		t.Fatal(err)
	}
	if again, ok := changed.Changed(); !ok || !reflect.DeepEqual(again, installed) || reflect.DeepEqual(again, s.Authority()) {
		t.Fatal("an issued receipt followed a later commit")
	}

	noop := new(CommandReceipt)
	if err := s.Command(WithCommandReceipt(t.Context(), noop), term, func(context.Context, *CommandTx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, ok := noop.Changed(); ok || !noop.Unchanged() || noop.Unknown() {
		t.Fatal("a no-op did not report a confirmed completion without a change")
	}

	refusal := errors.New("fixture refusal")
	rolledBack := new(CommandReceipt)
	generation := imageOf(t, s).Generation()
	err := s.Command(WithCommandReceipt(t.Context(), rolledBack), term, func(ctx context.Context, tx *CommandTx) error {
		if err := put("refused")(ctx, tx); err != nil {
			return err
		}
		return refusal
	})
	if !errors.Is(err, refusal) {
		t.Fatal(err)
	}
	if _, ok := rolledBack.Changed(); ok || rolledBack.Unchanged() || rolledBack.Unknown() || !imageOf(t, s).Generation().Same(generation) {
		t.Fatal("a refused command reported a change, an unknown outcome or committed")
	}

	checked := new(CommandReceipt)
	err = s.Command(WithCommandReceipt(t.Context(), checked), term, func(ctx context.Context, tx *CommandTx) error {
		if err := tx.RequireCommitCheck(func(context.Context) error { return refusal }); err != nil {
			return err
		}
		return put("checked")(ctx, tx)
	})
	if !errors.Is(err, refusal) {
		t.Fatal(err)
	}
	if _, ok := checked.Changed(); ok || checked.Unchanged() || checked.Unknown() || !imageOf(t, s).Generation().Same(generation) {
		t.Fatal("a final-check refusal reported a change, an unknown outcome or committed")
	}
}

// A receipt serves one command. A used receipt refuses the next command before
// it opens a transaction, and keeps what the first command reported.
func TestCommandReceiptServesOneCommand(t *testing.T) {
	s := openTemp(t)
	term := s.Authority().Term
	receipt := new(CommandReceipt)
	ctx := WithCommandReceipt(t.Context(), receipt)
	if err := s.Command(ctx, term, func(ctx context.Context, tx *CommandTx) error {
		return tx.PutSection(ctx, "dedup", replication.Entry{Key: "once", Data: []byte(`{}`)})
	}); err != nil {
		t.Fatal(err)
	}
	first, ok := receipt.Changed()
	if !ok {
		t.Fatal("the first command did not report its change")
	}
	generation := imageOf(t, s).Generation()
	called := false
	err := s.Command(ctx, term, func(ctx context.Context, tx *CommandTx) error {
		called = true
		return tx.PutSection(ctx, "dedup", replication.Entry{Key: "twice", Data: []byte(`{}`)})
	})
	if !errors.Is(err, replication.ErrInvalid) || called || !imageOf(t, s).Generation().Same(generation) {
		t.Fatal("a used receipt admitted a second command", err)
	}
	if again, ok := receipt.Changed(); !ok || !reflect.DeepEqual(again, first) {
		t.Fatal("a refused reuse changed the receipt")
	}
	// A command that is refused at entry reports a confirmed rollback.
	stale := s.Authority().Term
	stale.Epoch++
	unopened := new(CommandReceipt)
	if err := s.Command(WithCommandReceipt(t.Context(), unopened), stale, func(context.Context, *CommandTx) error { return nil }); err == nil {
		t.Fatal("a stale term was admitted")
	}
	if _, ok := unopened.Changed(); ok || unopened.Unchanged() || unopened.Unknown() {
		t.Fatal("a refused entry reported a change or an unknown outcome")
	}
}
