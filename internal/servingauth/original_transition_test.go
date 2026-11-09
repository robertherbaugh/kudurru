package servingauth

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestOriginalTransitionRetainsVerifiedCredentialLifetime(t *testing.T) {
	for _, kind := range []string{"sa-token", "local-token", "cookie-idle", "cookie-absolute", "jwt", "external-token", "external-cookie-idle", "external-cookie-absolute"} {
		t.Run(kind, func(t *testing.T) {
			f, proof, deadline := clockProof(t, kind)
			now := f.now
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			view := f.view(t)
			_, decision, lifetime, err := view.AuthorizeOriginalTransition(ctx, proof, readAttrs(), func() time.Time { return now })
			if err != nil || !decision.Allowed || lifetime == nil {
				t.Fatal("original proof refused", err)
			}
			now = deadline
			if !errors.Is(lifetime.Check(ctx), ErrCredentials) {
				t.Fatal("original expiry renewed")
			}
			now = f.now
			cancel()
			if !errors.Is(lifetime.Check(ctx), context.Canceled) {
				t.Fatal("original cancellation ignored")
			}
		})
	}
}

func TestOriginalTransitionRejectsConsumableAndUnboundedProof(t *testing.T) {
	f := newFixture(t)
	view := f.view(t)
	for _, proof := range []Credential{{Kind: Password, Value: "unused"}, {Kind: APIToken, Value: f.wire}} {
		if _, _, _, err := view.AuthorizeOriginalTransition(context.Background(), proof, readAttrs(), func() time.Time { return f.now }); err == nil {
			t.Fatal("unbounded or consumable proof admitted")
		}
	}
}
