package servingbuild

import (
	"context"
	"time"

	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	blockingstage "github.com/robertherbaugh/kudurru/internal/stages/blocking"
)

// BlockingReads projects the original HTTP request's immutable blocking owner.
// It has no live toggle, settings source, or publisher to recapture.
type BlockingReads struct{}

// NewBlockingReads constructs the stateless original-request blocking reader.
func NewBlockingReads() *BlockingReads { return &BlockingReads{} }

// BlockingState evaluates one observation time against the original settings
// and deadline. A valid held lease remains readable after a newer publication.
func (r *BlockingReads) BlockingState(ctx context.Context, now time.Time) (configured, enabled bool, disabledUntil time.Time, err error) {
	if r == nil || ctx == nil {
		return false, false, time.Time{}, serving.ErrComponents
	}
	if _, err := servinghttp.Identity(ctx); err != nil {
		return false, false, time.Time{}, err
	}
	view, err := servinghttp.Component[*blockingstage.View](ctx, "blocking")
	if err != nil {
		return false, false, time.Time{}, err
	}
	configured, enabled, disabledUntil, err = view.StateAt(now)
	if ctx.Err() != nil {
		return false, false, time.Time{}, ctx.Err()
	}
	return configured, enabled, disabledUntil, err
}
