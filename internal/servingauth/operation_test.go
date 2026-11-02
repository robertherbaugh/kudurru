package servingauth

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestOperationOriginalProofEveryCredentialFinalExpiry(t *testing.T) {
	for _, kind := range []string{"sa-token", "local-token", "cookie-idle", "cookie-absolute", "jwt", "external-token", "external-cookie-idle", "external-cookie-absolute"} {
		t.Run(kind, func(t *testing.T) {
			f, raw, deadline := clockProof(t, kind)
			original := f.view(t)
			proof, err := CaptureOperation(t.Context(), original, raw, readAttrs(), f.now)
			if err != nil {
				t.Fatal(err)
			}
			if proof.dependency == f.deps.Identity {
				t.Fatal("fixture did not distinguish aggregate identity from host cursor")
			}
			// The same unchanged source must pass in successive distinct commands.
			for range 2 {
				if err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
					_, err := ReauthorizeOperationCurrent(ctx, tx, f.deps, proof, func() time.Time { return f.now })
					return err
				}); err != nil {
					t.Fatal("unchanged original authority refused", err)
				}
			}
			before := f.image(t)
			now := f.now
			err = f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				if err := tx.RequireCommitCheck(func(context.Context) error { now = deadline; return nil }); err != nil {
					return err
				}
				if _, err := ReauthorizeOperationCurrent(ctx, tx, f.deps, proof, func() time.Time { return now }); err != nil {
					return err
				}
				changed := f.user.DeepCopy()
				changed.Labels = map[string]string{"operation": "must-rollback"}
				_, err := tx.Apply(ctx, changed, store.ApplyOptions{Manager: store.ManagerSystem})
				return err
			})
			after := f.image(t)
			if !errors.Is(err, ErrCredentials) || !before.Generation().Same(after.Generation()) || !reflect.DeepEqual(before.Payload, after.Payload) {
				t.Fatal("final original expiry did not roll back the operation", err)
			}
		})
	}
}

func TestOperationOriginalTokenExpiryCannotBeExtendedByCurrentRow(t *testing.T) {
	f, raw, deadline := clockProof(t, "local-token")
	proof, err := CaptureOperation(t.Context(), f.view(t), raw, readAttrs(), f.now)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := tokens.Parse(raw.Value)
	var current internalv1.APIToken
	if err := f.st.Get(t.Context(), store.Key{Kind: "APIToken", Name: id}, &current); err != nil {
		t.Fatal(err)
	}
	current.Spec.ExpiresAt = &metav1.Time{Time: deadline.Add(time.Hour)}
	apply(t, f.st, &current)
	if err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, _, err := ReauthorizeCurrentWithClock(ctx, tx, f.deps, raw, readAttrs(), func() time.Time { return deadline })
		return err
	}); err != nil {
		t.Fatal("fixture did not establish the longer current-row lifetime", err)
	}
	err = f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := ReauthorizeOperationCurrent(ctx, tx, f.deps, proof, func() time.Time { return deadline })
		return err
	})
	if !errors.Is(err, ErrCredentials) {
		t.Fatal("current row extended the original operation deadline", err)
	}
}

func TestOperationStillRequiresCurrentProofAndExactDependency(t *testing.T) {
	for _, change := range []string{"dependency", "credential", "original-cancel", "clock-regression"} {
		t.Run(change, func(t *testing.T) {
			f, raw, _ := clockProof(t, "local-token")
			origin, cancel := context.WithCancel(t.Context())
			defer cancel()
			proof, err := CaptureOperation(origin, f.view(t), raw, readAttrs(), f.now)
			if err != nil {
				t.Fatal(err)
			}
			deps, now := f.deps, f.now
			want := ErrCredentials
			switch change {
			case "dependency":
				deps.Identity, deps.ExpectedIdentity = "replacement", "replacement"
				want = ErrDependency
			case "credential":
				id, _ := tokens.Parse(raw.Value)
				var token internalv1.APIToken
				if err := f.st.Get(t.Context(), store.Key{Kind: "APIToken", Name: id}, &token); err != nil {
					t.Fatal(err)
				}
				other, _ := tokens.Generate()
				token.Spec.SecretHash = tokens.Hash(other)
				apply(t, f.st, &token)
				if err := proof.CheckOriginal(t.Context(), now); err != nil {
					t.Fatal("original bound unexpectedly changed", err)
				}
			case "original-cancel":
				cancel()
				want = context.Canceled
			case "clock-regression":
				now = now.Add(-time.Nanosecond)
			}
			err = f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				_, err := ReauthorizeOperationCurrent(ctx, tx, deps, proof, func() time.Time { return now })
				return err
			})
			if !errors.Is(err, want) {
				t.Fatalf("changed original authority accepted: %v", err)
			}
		})
	}
}

func TestOperationRefusesPasswordOrForgedEvidence(t *testing.T) {
	f := newFixture(t)
	if _, err := CaptureOperation(t.Context(), f.view(t), Credential{Kind: Password, Username: f.user.Name, Value: "correct-password", TOTPCode: "123456"}, readAttrs(), f.now); !errors.Is(err, ErrUnsupported) {
		t.Fatal("operation accepted consumable enrollment proof")
	}
	if err := new(OperationProof).CheckOriginal(t.Context(), f.now); !errors.Is(err, ErrInvalid) {
		t.Fatal("empty proof acquired authority")
	}
}
