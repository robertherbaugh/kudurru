package servingauth

import (
	"context"
	"errors"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestProviderSourcesCurrentSeesStagedSpecAndRollsBack(t *testing.T) {
	f := newExternalFixture(t)
	original := f.proof(t).SpecIdentity
	abort := errors.New("abort provider edit")
	err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		changed := f.provider.DeepCopy()
		changed.Spec.ClientID = "replacement-client"
		if _, err := tx.Apply(ctx, changed, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		got, err := ProviderSourcesCurrent(ctx, tx, []ProviderSource{})
		if err != nil {
			return err
		}
		for _, d := range got {
			if d.Ref.ID == "corp" {
				if d.SpecIdentity == original || d.Spec.ClientID != "replacement-client" || d.Ref.UID != string(f.provider.UID) {
					t.Fatal("transaction source ignored staged exact UID/spec")
				}
				d.Spec.ClientID = "detached-output"
			}
		}
		again, err := ProviderSourcesCurrent(ctx, tx, []ProviderSource{})
		if err != nil {
			return err
		}
		for _, d := range again {
			if d.Ref.ID == "corp" && d.Spec.ClientID != "replacement-client" {
				t.Fatal("aliased source")
			}
		}
		return abort
	})
	if !errors.Is(err, abort) || f.proof(t).SpecIdentity != original {
		t.Fatal("source rollback", err)
	}
	if _, err := ProviderSourcesCurrent(t.Context(), nil, []ProviderSource{}); !errors.Is(err, ErrInvalid) {
		t.Fatal("nil tx accepted", err)
	}
}
