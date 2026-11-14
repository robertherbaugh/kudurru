package store

import "context"

// AuditRequest is transport attribution carried to the driver without an audit
// recorder dependency. Authz is evaluated at write time, after authorization.
type AuditRequest struct {
	ID, IP, UA, Method, Path string
}

type auditContext struct {
	request AuditRequest
	authz   func(context.Context) string
}

type auditContextKey struct{}

// WithAuditRequest carries the original request to ordinary driver writes.
func WithAuditRequest(ctx context.Context, request AuditRequest, authz func(context.Context) string) context.Context {
	return context.WithValue(ctx, auditContextKey{}, auditContext{request: request, authz: authz})
}

// AuditRequestFrom reads the request and the authorization path at recording.
func AuditRequestFrom(ctx context.Context) (AuditRequest, string) {
	v, _ := ctx.Value(auditContextKey{}).(auditContext)
	if v.authz == nil {
		return v.request, ""
	}
	return v.request, v.authz(ctx)
}
