package cache

import (
	"github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/upstream"
)

// Inspect observes one raw response under the request's already-bound resolver
// plan. It never resolves a miss, changes LRU/hit/prefetch state, or rebinds a
// restored entry. The caller owns the request lease and must not modify Wire.
// cacheable describes raw caching, not whether policy would serve the answer.
func (s *Stage) Inspect(req *pipeline.Request) (value cache.Lookup, found, eligible bool, err error) {
	if req == nil || req.Ctx == nil || req.Ctx.Err() != nil {
		return value, false, false, pipeline.ErrServingCapture
	}
	view, err := s.pin(req)
	if err != nil {
		return value, false, false, err
	}
	plan, ok := req.Resolution.(*upstream.QueryPlan)
	if !ok {
		return value, false, false, pipeline.ErrServingCapture
	}
	if view.disabled || !cacheable(req.Msg) || !plan.Cacheable() || (req.UpstreamGroup != "" && req.UpstreamGroup != upstream.DefaultGroup) {
		return value, false, false, nil
	}
	// Inspection cannot create new scope mappings. A missing mapping means no
	// DNS request in this cache incarnation has used this resolution scope.
	if view.epoch != 0 {
		if _, ok := view.scopes.Load(plan.Scope()); !ok {
			return value, false, true, nil
		}
	}
	key, _ := s.key(req)
	value, found = s.c.Peek(key)
	if found {
		parts, valid := key.Parts()
		if !valid || value.Metadata.Scope != parts.Scope {
			return cache.Lookup{}, false, true, nil
		}
	}
	if err := req.Ctx.Err(); err != nil {
		return cache.Lookup{}, false, false, err
	}
	return value, found, true, nil
}
