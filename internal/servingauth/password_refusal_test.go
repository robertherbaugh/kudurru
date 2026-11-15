package servingauth

import (
	"context"
	"errors"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestPasswordRefusalBelongsOnlyToOriginalCommand(t *testing.T) {
	f := newFixture(t)
	before := f.image(t).Generation()
	var refused error
	err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, refused = ResolveSessionCurrent(ctx, tx, f.deps, Credential{Kind: Password, Username: "alice", Value: "wrong-password"}, f.now)
		if !errors.Is(refused, ErrCredentials) || !IsReadOnlyPasswordRefusal(refused, tx) || IsReadOnlyPasswordRefusal(ErrCredentials, tx) {
			t.Fatal("incorrect refusal classification", refused)
		}
		return nil
	})
	if err != nil || !f.image(t).Generation().Same(before) {
		t.Fatal("read-only rejection mutated authentication state", err)
	}
	err = f.st.Command(t.Context(), f.st.Authority().Term, func(_ context.Context, tx *sqlite.CommandTx) error {
		if IsReadOnlyPasswordRefusal(refused, tx) {
			t.Fatal("refusal transferred to a new command")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
