package servingbuild

import (
	"context"

	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	upstreamstage "github.com/robertherbaugh/kudurru/internal/stages/upstream"
	"github.com/robertherbaugh/kudurru/internal/upstream"
)

// UpstreamReads samples local operational health on the original request's
// retained resolution routes. It has no live stage or publisher to recapture.
type UpstreamReads struct{}

// NewUpstreamReads constructs the stateless original-request upstream reader.
func NewUpstreamReads() *UpstreamReads { return &UpstreamReads{} }

// UpstreamStates preserves original route membership while sampling counters
// that may advance during the lease. Results are detached scalar records.
func (r *UpstreamReads) UpstreamStates(ctx context.Context) ([]upstream.ServerStatus, error) {
	if r == nil || ctx == nil {
		return nil, serving.ErrComponents
	}
	if _, err := servinghttp.Identity(ctx); err != nil {
		return nil, err
	}
	view, err := servinghttp.Component[*upstreamstage.View](ctx, "resolution")
	if err != nil {
		return nil, err
	}
	states, err := view.Status()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return states, err
}
