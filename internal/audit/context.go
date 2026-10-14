package audit

import "context"

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
	return context.WithValue(ctx, requestContextKey{}, requestContext{actor: actor, request: request, authz: authz})
}
