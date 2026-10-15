package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"

	internalkinds "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/auth/authtest"
	"github.com/robertherbaugh/kudurru/internal/auth/keys"
	"github.com/robertherbaugh/kudurru/internal/auth/stack"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
)

func TestTokenAPIAuditsAuthenticatedRequestAndAuthorization(t *testing.T) {
	ctx := context.Background()
	scheme, err := internalkinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	rec := &audit.Memory{}
	var bearer string
	h := newHarness(t, func(o *Options) {
		o.Store = audit.WrapStore(authtest.Memory(t), rec, audit.WrapOptions{Scheme: scheme, Logger: o.Logger})
		ids, err := stack.New(ctx, stack.Options{
			Config: o.Config, Store: o.Store, Keys: keys.Keys{Current: bytes.Repeat([]byte{1}, 32)}, Logger: o.Logger, Out: io.Discard,
			Getenv: func(key string) string {
				if key == users.EnvAdminPassword {
					return "test-admin-password"
				}
				return ""
			},
		})
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
	h.do(http.MethodPost, "/api/v1/auth/tokens", "", CreateTokenRequest{Description: "deployment check", ExpiresIn: "1h"}, "Authorization", "Bearer "+bearer, "User-Agent", "audit-attribution-test").want(http.StatusCreated).json(&created)
	for _, ev := range rec.Events() {
		if ev.Action != "apitoken.create" || ev.Resource.Name != created.ID {
			continue
		}
		if ev.Actor.Kind != "User" || ev.Actor.ID != "local:admin" || ev.Actor.Name != "admin" || ev.Actor.Auth != "token" {
			t.Fatalf("actor attribution: %+v", ev.Actor)
		}
		if ev.Request.IP != "192.0.2.10" || ev.Request.Method != http.MethodPost || ev.Request.Path != "/api/v1/auth/tokens" || ev.Request.UA != "audit-attribution-test" || ev.Request.ID == "" {
			t.Fatalf("request attribution: %+v", ev.Request)
		}
		if ev.Authz != "local" {
			t.Fatalf("authorization path = %q, want local", ev.Authz)
		}
		return
	}
	t.Fatal("token creation event not recorded")
}
