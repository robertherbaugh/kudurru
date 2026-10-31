package cache

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

var nextView atomic.Uint64
var nextScope atomic.Uint64

// View is one immutable cache settings/incarnation cut. Root may reuse a still
// current view for changes irrelevant to cache semantics; a retired incarnation
// must never be revived, including settings A -> B -> A.
type View struct {
	settings Settings
	disabled bool
	epoch    uint64
	current  func() bool
	identity [32]byte
	scopes   sync.Map // derived CacheScope -> scope; no mutable configuration
}

// NewView prepares settings without mutating/flushing a live cache. current is a
// required cheap atomic check for the captured semantic dependency cursor.
func NewView(settings Settings, disabled bool, current func() bool) (*View, error) {
	if current == nil {
		return nil, fmt.Errorf("cache: view requires current cursor")
	}
	settings = settings.withDefaults()
	// Fixed struct field order gives the semantic identity stable persistence.
	raw, err := json.Marshal(struct {
		Settings Settings
		Disabled bool
	}{settings, disabled})
	if err != nil {
		return nil, err
	}
	return &View{settings: settings, disabled: disabled, epoch: nextView.Add(1), current: current, identity: sha256.Sum256(raw)}, nil
}

func (v *View) scope(base pipeline.CacheScope) pipeline.CacheScope {
	if v.epoch == 0 { // preserve legacy/persisted cache keys outside capture mode
		return base
	}
	if scope, ok := v.scopes.Load(base); ok {
		return scope.(pipeline.CacheScope)
	}
	var raw [64]byte
	copy(raw[:32], base.ID[:])
	copy(raw[32:], v.identity[:])
	scope := pipeline.CacheScope{ID: sha256.Sum256(raw[:]), Epoch: nextScope.Add(1)}
	actual, _ := v.scopes.LoadOrStore(base, scope)
	return actual.(pipeline.CacheScope)
}

func (s *Stage) pin(req *pipeline.Request) (*View, error) {
	if req.Ctx != nil {
		if view, ok := req.Ctx.Value(s).(*View); ok {
			return view, nil
		}
	}
	var view *View
	if s.Capture != nil {
		var err error
		view, err = s.Capture(req)
		if err != nil {
			return nil, err
		}
		if view == nil {
			return nil, pipeline.ErrServingCapture
		}
	} else if req.Serving != nil {
		return nil, pipeline.ErrServingCapture
	} else {
		view = &View{settings: *s.settings(), disabled: s.disabled.Load(), current: func() bool { return true }}
	}
	ctx := req.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	req.Ctx = context.WithValue(ctx, s, view)
	return view, nil
}

func (s *Stage) view(req *pipeline.Request) *View {
	if req.Ctx != nil {
		if view, ok := req.Ctx.Value(s).(*View); ok {
			return view
		}
	}
	return &View{settings: *s.settings(), disabled: s.disabled.Load(), current: func() bool { return true }}
}
