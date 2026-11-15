// Package iamgate keeps Kubernetes migration admission separate from DNS.
// Every management capture checks uncached private provenance; no stale success
// survives an API outage, changed journal or incomplete offline cutover.
package iamgate

import (
	"context"
	"errors"
	"net/http"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/iammigrate"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// ErrClosed is intentionally free of credential, document and API error details.
var ErrClosed = errors.New("IAM admission requires a verified native installation or completed offline migration")

// Source returns status and immutable credential mappings from one verified
// private journal. Native installations have no mappings.
type Source interface {
	Capture(context.Context) (iammigrate.Status, *iammigrate.Bindings, error)
}

// Gate bounds uncached admission work independently of API traffic and DNS.
type Gate struct {
	source Source
	slots  chan struct{}
}

// New constructs a bounded gate; a nil source keeps admission closed.
func New(source Source) *Gate { return &Gate{source: source, slots: make(chan struct{}, 8)} }

type viewKey struct{}
type view struct{ bindings *iammigrate.Bindings }

// Capture never reuses an older admission result, including one in ctx.
// The returned context inherits the caller's lifetime, not the check deadline.
func (g *Gate) Capture(ctx context.Context) (context.Context, iammigrate.Status, error) {
	closed := iammigrate.Status{State: iammigrate.Blocked}
	if g == nil || g.source == nil {
		return ctx, closed, ErrClosed
	}
	select {
	case g.slots <- struct{}{}:
		defer func() { <-g.slots }()
	default:
		return ctx, closed, ErrClosed
	}
	check, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	status, bindings, err := g.source.Capture(check)
	if err != nil || check.Err() != nil || !status.RemoteAllowed {
		status.RemoteAllowed = false
		return ctx, status, ErrClosed
	}
	switch status.State {
	case iammigrate.Native:
		if bindings != nil || status.JournalResourceVersion != "" || status.MigrationID != "" {
			return ctx, closed, ErrClosed
		}
	case iammigrate.Complete:
		if bindings == nil || status.JournalResourceVersion == "" || status.MigrationID == "" {
			return ctx, closed, ErrClosed
		}
	default:
		return ctx, closed, ErrClosed
	}
	return context.WithValue(ctx, viewKey{}, view{bindings}), status, nil
}

// ResolveToken uses only the exact migration proof captured for this request.
// It does not load accounts, tokens, roles or an alternative live journal.
func ResolveToken(ctx context.Context, token *internalv1.APIToken, account store.Object) (tokens.MigratedIdentity, bool) {
	v, ok := ctx.Value(viewKey{}).(view)
	if !ok || v.bindings == nil {
		return tokens.MigratedIdentity{}, false
	}
	binding, ok := v.bindings.ResolveToken(token, account)
	if !ok {
		return tokens.MigratedIdentity{}, false
	}
	return tokens.MigratedIdentity{AccountUID: string(binding.Owner.UID), TokenUID: string(binding.TokenUID), OriginalCreatedAt: binding.OriginalAccountCreatedAt}, true
}

// Middleware belongs outside authentication so login, callbacks, sessions,
// tokens and public auth routes all share the same admission boundary.
func (g *Gate) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, _, err := g.Capture(r.Context())
		if err != nil {
			w.Header().Set("Cache-Control", "no-store")
			auth.WriteError(w, r, &auth.Error{Status: http.StatusServiceUnavailable, Key: "iam_migration_required", Message: "identity admission is unavailable", Hint: "inspect kudurru iam migrate status using the release namespace", RetryAfter: 5 * time.Second})
			return
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
