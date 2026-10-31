package api

import (
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/rbac"
)

func TestCredentialExpiryPrecedesCachedObjectPermission(t *testing.T) {
	issued := time.Now().UTC().Truncate(time.Second)
	now := issued
	p := auth.WithCredentialTimeBounds(auth.Principal{Kind: auth.KindKubernetesServiceAccount}, issued, issued, issued.Add(10*time.Minute), func() time.Time { return now })
	ctx := withState(auth.WithPrincipal(t.Context(), p), &reqState{clusterAllowed: true, allowedResource: "zones", allowedVerb: "read"})
	s := new(Server)
	scope := rbac.Scope{Kind: rbac.ScopeZone, Name: "example"}
	if !s.allowed(ctx, "zones", "read", scope) || s.require(ctx, "zones", "read", scope, "example") != nil {
		t.Fatal("valid cached permission rejected")
	}
	now = issued.Add(10 * time.Minute)
	if s.allowed(ctx, "zones", "read", scope) {
		t.Fatal("cached object permission outlived original credential")
	}
	err := s.require(ctx, "zones", "read", scope, "example")
	if e, ok := err.(*Error); !ok || e.Body.Key != KeyUnauthenticated {
		t.Fatal("expired cached permission did not require authentication")
	}
}
