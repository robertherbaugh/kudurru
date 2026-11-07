package audit

import (
	"context"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/store"
)

type requestContextKey struct{}

type requestContext struct {
	actor   Actor
	request Request
	authz   func() string
}

// WithRequest carries HTTP context to the store recorder, which otherwise
// sees only the object being written and the store actor's name. Authz is
// read at recording time because authorization follows actor middleware.
func WithRequest(ctx context.Context, actor Actor, request Request, authz func() string) context.Context {
	ctx = store.WithAuditRequest(ctx, store.AuditRequest{ID: request.ID, IP: request.IP, UA: request.UA, Method: request.Method, Path: request.Path}, func(current context.Context) string {
		// Forwarded identity routes authorize after the API actor middleware.
		if dec, ok := auth.DecisionFrom(current); ok && dec.Allowed {
			return dec.Path
		}
		if authz != nil {
			return authz()
		}
		return ""
	})
	return context.WithValue(ctx, requestContextKey{}, requestContext{actor: actor, request: request, authz: authz})
}
