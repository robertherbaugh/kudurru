// Package identify is the client identification stage (docs/architecture.md
// §7.1 step 3): it finds the client's ClientGroups and the policy that
// applies to it now, from the policy Index of the current zonecache
// snapshot, and hands both to the later stages.
//
// After this stage:
//
//   - Request.Client.GroupIDs holds the client's groups ("namespace/name",
//     sorted). The slice is shared with the Index: read it, never modify it.
//   - Request.Policy holds the *policy.Decision; read it with
//     policy.FromRequest. It is never nil once an Index exists (a Decision
//     that applies no policy has Applies() == false).
//
// Before the first Index is built (zonecache not yet synced) the stage
// leaves both unset and the query is not filtered.
package identify

import (
	"time"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
)

// Source supplies the current policy Index; policy.Source reads it from
// zonecache and policy.Static wraps a fixed one.
type Source interface {
	Index() *policy.Index
}

// Stage implements pipeline.Stage.
type Stage struct {
	src Source
	now func() time.Time
}

// New returns the stage. now is the clock for schedules when a request
// has no Received time; nil means time.Now.
func New(src Source, now func() time.Time) *Stage {
	if now == nil {
		now = time.Now
	}
	return &Stage{src: src, now: now}
}

// Name implements pipeline.Stage.
func (s *Stage) Name() string { return "identify" }

// Serve implements pipeline.Stage.
func (s *Stage) Serve(req *pipeline.Request, next pipeline.Next) (*pipeline.Response, error) {
	var ix *policy.Index
	if s.src != nil {
		ix = s.src.Index()
	}
	if ix == nil {
		return next(req)
	}
	at := req.Received
	if at.IsZero() {
		at = s.now()
	}
	m := ix.Identify(req.Client)
	req.Client.GroupIDs = m.GroupIDs
	req.Policy = ix.Select(m, at)
	return next(req)
}
