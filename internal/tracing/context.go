package tracing

import "context"

type contextKey struct{}

// traceContext carries the trace in a context with one allocation (a
// context.WithValue of a pointer would take two).
type traceContext struct {
	context.Context
	trace *Trace
	span  SpanRef
}

func (c *traceContext) Value(key any) any {
	if key == (contextKey{}) {
		return c
	}
	return c.Context.Value(key)
}

// NewContext returns ctx carrying the trace and the span that work started
// under ctx should be children of. A nil trace returns ctx unchanged, so
// an untraced query allocates nothing.
func NewContext(ctx context.Context, t *Trace, span SpanRef) context.Context {
	if t == nil {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return &traceContext{Context: ctx, trace: t, span: span}
}

// FromContext returns the trace and parent span NewContext stored, or nil.
func FromContext(ctx context.Context) (*Trace, SpanRef) {
	if ctx == nil {
		return nil, 0
	}
	c, _ := ctx.Value(contextKey{}).(*traceContext)
	if c == nil {
		return nil, 0
	}
	return c.trace, c.span
}
