package tracing

import "context"

type contextKey struct{}

type current struct {
	trace *Trace
	span  SpanRef
}

// NewContext returns ctx carrying the trace and the span that work started
// under ctx should be children of. A nil trace returns ctx unchanged, so
// an untraced query allocates nothing.
func NewContext(ctx context.Context, t *Trace, span SpanRef) context.Context {
	if t == nil {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, &current{trace: t, span: span})
}

// FromContext returns the trace and parent span NewContext stored, or nil.
func FromContext(ctx context.Context) (*Trace, SpanRef) {
	if ctx == nil {
		return nil, 0
	}
	c, _ := ctx.Value(contextKey{}).(*current)
	if c == nil {
		return nil, 0
	}
	return c.trace, c.span
}
