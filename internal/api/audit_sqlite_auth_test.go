package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"path/filepath"
	"testing"

	internalkinds "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/auth/keys"
	"github.com/robertherbaugh/kudurru/internal/auth/stack"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestSQLiteForwardedIdentityAuditAttribution(t *testing.T) {
	ctx := context.Background()
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	private := []store.Kind{}
	for _, kind := range runtimekinds.Private() {
		private = append(private, store.Kind(kind))
	}
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"), sqlite.Options{Scheme: scheme, InternalKinds: private, Validator: testValidator(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var bearer string
	h := newHarness(t, func(o *Options) {
		o.Store = st
		ids, err := stack.New(ctx, stack.Options{Config: o.Config, Store: st, Keys: keys.Keys{Current: bytes.Repeat([]byte{1}, 32)}, Logger: o.Logger, Out: io.Discard, Getenv: func(key string) string {
			if key == users.EnvAdminPassword {
				return "test-admin-password"
			}
			return ""
		}})
		if err != nil {
			t.Fatal(err)
		}
		bearer, _, err = ids.Tokens().Create(ctx, tokens.CreateOptions{Owner: internalkinds.TokenOwner{Kind: "User", Name: "admin", ID: "local:admin", Provider: "local"}, Description: "test setup"})
		if err != nil {
			t.Fatal(err)
		}
		mux := http.NewServeMux()
		ids.Register(mux)
		o.Authenticate, o.Authorizer, o.AuthHandler = ids.Middleware(), ids.Authorizer(), mux
	})
	var created CreatedToken
	h.do(http.MethodPost, "/api/v1/auth/tokens", "", CreateTokenRequest{Description: "safe deployment label", ExpiresIn: "1h"}, "Authorization", "Bearer "+bearer, "User-Agent", "forwarded-audit-fixture").want(http.StatusCreated).json(&created)
	rows, err := st.ReadAudit(ctx, sqlite.AuditQuery{Kind: "APIToken", Name: created.ID})
	if err != nil || len(rows) != 1 {
		t.Fatal("token audit missing")
	}
	row := rows[0]
	if row.Authz != "local" || row.RequestID == "" || row.ClientIP != "192.0.2.10" || row.UserAgent != "forwarded-audit-fixture" || row.RequestMethod != http.MethodPost || row.RequestPath != "/api/v1/auth/tokens" || row.Actor != "User:admin" {
		t.Fatal("forwarded identity decision lost in driver attribution")
	}
	if err := st.VerifyAudit(ctx); err != nil {
		t.Fatal(err)
	}
}
