package blocking

import (
	"context"
	"fmt"
	"time"

	"github.com/robertherbaugh/kudurru/internal/listdata/catalogue"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// View captures response defaults, the canonical absolute disable deadline and
// exact safe-search catalogue. Its contents and catalogue remain immutable.
type View struct {
	defaults      *defaults
	disabledUntil time.Time
	catalogue     *catalogue.Catalogue
}

// NewView copies response settings without reading live Toggle/Index/catalogue
// holders. Root supplies Kudurru/default's captured deadline and exact catalogue.
// Options callbacks, metrics and other stage infrastructure are not retained.
func NewView(opts Options, disabledUntil time.Time, cat *catalogue.Catalogue) (*View, error) {
	if cat == nil {
		return nil, fmt.Errorf("blocking: view requires captured catalogue")
	}
	return &View{defaults: defaultsOf(opts), disabledUntil: disabledUntil, catalogue: cat}, nil
}

func (v *View) enabled(now time.Time) bool {
	return !v.defaults.disabled && (v.disabledUntil.IsZero() || v.disabledUntil.Before(Forever) && !now.Before(v.disabledUntil))
}

func (s *Stage) pin(req *pipeline.Request) (*View, error) {
	if req.Ctx != nil {
		if v, ok := req.Ctx.Value(s).(*View); ok {
			return v, nil
		}
	}
	var v *View
	if s.Capture != nil {
		var err error
		v, err = s.Capture(req)
		if err != nil {
			return nil, err
		}
		if v == nil {
			return nil, pipeline.ErrServingCapture
		}
	} else if req.Serving != nil {
		return nil, pipeline.ErrServingCapture
	} else {
		v = &View{defaults: s.live.Load(), disabledUntil: s.toggle.DisabledUntil(), catalogue: s.catalogue()}
	}
	ctx := req.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	req.Ctx = context.WithValue(ctx, s, v)
	return v, nil
}

func (s *Stage) view(req *pipeline.Request) *View {
	if req.Ctx != nil {
		if v, ok := req.Ctx.Value(s).(*View); ok {
			return v
		}
	}
	return &View{defaults: s.live.Load(), disabledUntil: s.toggle.DisabledUntil(), catalogue: s.catalogue()}
}
