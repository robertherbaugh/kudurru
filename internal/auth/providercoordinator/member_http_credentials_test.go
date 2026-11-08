package providercoordinator

import (
	"context"
	"strings"
	"testing"
	"time"

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// Setup creates actual current verifier inputs. Only the scoped HTTP command's
// received proof can authorize its domain transaction and retained outcome.
func memberHTTPProofFixture(t *testing.T, kind string) (memberOutputFixture, servingauth.Credential, func(context.Context, *sqlite.CommandTx) error) {
	t.Helper()
	if kind == "jwt" {
		server := newProviderServer(t)
		f := newMemberOutputFixture(t, server)
		accept(t, f.fixture, candidates(t, f.fixture, 1))
		ready(t, f.fixture, 1)
		raw := servingauth.Credential{Kind: servingauth.OIDCJWT, Value: server.token(t, f.host.now().Add(-time.Minute))}
		return f, raw, func(ctx context.Context, tx *sqlite.CommandTx) error {
			// The signed token is unchanged; current grant removal must still
			// deny a retained dedup result after actual signature verification.
			return tx.Delete(ctx, store.Key{Kind: "RoleBinding", Namespace: "apps", Name: "view"}, store.DeleteOptions{})
		}
	}
	f := newMemberOutputFixture(t)
	if kind == "api-token" {
		id, ok := tokens.Parse(f.wire)
		if !ok {
			t.Fatal("fixture token has no identifier")
		}
		return f, servingauth.Credential{Kind: servingauth.APIToken, Value: f.wire}, func(ctx context.Context, tx *sqlite.CommandTx) error {
			return tx.Delete(ctx, store.Key{Kind: "APIToken", Name: id}, store.DeleteOptions{})
		}
	}
	if kind != "cookie" {
		t.Fatal("unsupported HTTP proof fixture")
	}
	const sessionID = "member-cookie-session-original"
	csrf := strings.Repeat("c", 32)
	command(t, f.store, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var sa publicv1.ServiceAccount
		if err := tx.Get(ctx, store.Key{Kind: "ServiceAccount", Namespace: "apps", Name: "ci"}, &sa); err != nil {
			return err
		}
		rows, err := tx.ListSections(ctx, "security")
		if err != nil {
			return err
		}
		state, err := servingauth.DecodeSecurity(rows)
		if err != nil {
			return err
		}
		state.Epoch++
		issued := f.host.now()
		state.Sessions = append(state.Sessions, servingauth.SessionRecord{FormatVersion: 1, ID: sessionID,
			Account:  servingauth.AccountRef{Kind: "ServiceAccount", Namespace: sa.Namespace, Name: sa.Name, UID: string(sa.UID)},
			Provider: "kudurru", Epoch: state.Epoch, IssuedAt: issued, ExpiresAt: issued.Add(time.Hour), IdleExpiresAt: issued.Add(10 * time.Minute),
			Grants: []string{"viewer"}, CSRFHash: servingauth.CSRFHash(csrf)})
		return writeMemberHTTPSecurity(ctx, tx, state)
	})
	image, err := f.store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wait(t, func() bool {
		status := f.publisher.Status()
		return status.ProtectedReady && status.Installed.Same(image.Generation())
	})
	cookie, err := f.view(t).SealSession(sessionID, csrf)
	if err != nil {
		t.Fatal(err)
	}
	return f, servingauth.Credential{Kind: servingauth.Cookie, Value: cookie}, func(ctx context.Context, tx *sqlite.CommandTx) error {
		rows, err := tx.ListSections(ctx, "security")
		if err != nil {
			return err
		}
		state, err := servingauth.DecodeSecurity(rows)
		if err != nil {
			return err
		}
		found := false
		for i := range state.Sessions {
			if state.Sessions[i].ID == sessionID {
				state.Sessions[i].Revoked = true
				found = true
			}
		}
		if !found {
			return servingauth.ErrCredentials
		}
		state.Epoch++
		return writeMemberHTTPSecurity(ctx, tx, state)
	}
}

func writeMemberHTTPSecurity(ctx context.Context, tx *sqlite.CommandTx, state servingauth.Security) error {
	rows, err := servingauth.EncodeSecurity(state)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err := tx.PutSection(ctx, "security", row); err != nil {
			return err
		}
	}
	return nil
}
