// Package identify is the client identification stage (dev-docs/design-record.md
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
	"context"
	"sync"
	"time"

	"github.com/robertherbaugh/kudurru/internal/inventory"
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
	// Inventory supplies complete immutable snapshots. It is optional for
	// embedders; production always installs the shared inventory holder.
	Inventory interface{ Snapshot() *inventory.Snapshot }
	// Hostnames supplies local DHCP names, sampled once alongside inventory.
	// Alias/dependency requests retain the original client identity.
	Hostnames policy.HostnameResolver
	// FallbackHostnames supplies untrusted asynchronous observations only when
	// neither captured inventory nor local DHCP supplies a client name.
	FallbackHostnames policy.HostnameResolver
	// Capture and CaptureInventory read typed owners from Request.Serving.
	// A captured query never falls back to src or the live inventory holder.
	Capture          func(*pipeline.Request) (*policy.Index, error)
	CaptureInventory func(*pipeline.Request) (*inventory.Snapshot, error)
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

// identification pins both membership and policy for the original request.
// Dependent requests inherit this context and never re-identify against a newer
// policy source or a different inventory observation.
type identification struct {
	membership *policy.Membership
	index      *policy.Index
	at         time.Time
	once       sync.Once
	decision   *policy.Decision
}

// Serve implements pipeline.Stage.
func (s *Stage) Serve(req *pipeline.Request, next pipeline.Next) (*pipeline.Response, error) {
	selected, err := s.identify(req)
	if err != nil {
		return nil, err
	}
	if selected != nil {
		// Selection stays at the normal stage: over-limit UDP needs only the
		// bounded membership lookup, never schedule or blocking-policy work.
		selected.once.Do(func() { selected.decision = selected.index.Select(selected.membership, selected.at) })
		req.Policy = selected.decision
	}
	return next(req)
}

// RateLimitExempt identifies an otherwise rate-limited UDP client early. The
// normal identify position reuses this exact result; no GroupIDs supplied by a
// transport or caller are trusted to grant an exemption.
func (s *Stage) RateLimitExempt(req *pipeline.Request) (bool, error) {
	selected, err := s.identify(req)
	if err != nil || selected == nil {
		return false, err
	}
	return selected.membership.RateLimitExempt(), nil
}

func (s *Stage) identify(req *pipeline.Request) (*identification, error) {
	if req.Serving != nil && s.Capture == nil {
		return nil, pipeline.ErrServingCapture
	}
	if req.Ctx != nil {
		if selected, ok := req.Ctx.Value(s).(*identification); ok {
			req.Client.GroupIDs = selected.membership.GroupIDs
			return selected, nil
		}
	}
	at := req.Received
	if at.IsZero() {
		at = s.now()
	}
	if (s.Inventory != nil || s.CaptureInventory != nil || s.Hostnames != nil || s.FallbackHostnames != nil || req.Serving != nil) && !req.Client.IdentityCaptured {
		var snapshot *inventory.Snapshot
		if s.CaptureInventory != nil {
			var err error
			snapshot, err = s.CaptureInventory(req)
			if err != nil {
				return nil, err
			}
		} else if req.Serving != nil {
			return nil, pipeline.ErrServingCapture
		} else if s.Inventory != nil {
			snapshot = s.Inventory.Snapshot()
		}
		req.Client.IdentityCaptured = true
		req.Client.MACs, req.Client.Names, req.Client.Tags = nil, nil, nil
		req.Client.InventorySource = ""
		if c := snapshot.Lookup(req.Client.Addr.Addr(), at); c != nil {
			req.Client.MACs, req.Client.Names, req.Client.Tags = c.MACs, c.Names(), c.Tags
			req.Client.InventorySource = c.Source
		}
		if s.Hostnames != nil {
			names := append([]string(nil), req.Client.Names...)
			req.Client.Names = append(names, s.Hostnames.Hostnames(req.Client.Addr.Addr())...)
		}
		if len(req.Client.Names) == 0 && s.FallbackHostnames != nil {
			req.Client.Names = s.FallbackHostnames.Hostnames(req.Client.Addr.Addr())
		}
	}
	var ix *policy.Index
	if s.Capture != nil {
		var err error
		ix, err = s.Capture(req)
		if err != nil {
			return nil, err
		}
	} else if req.Serving != nil {
		return nil, pipeline.ErrServingCapture
	} else if s.src != nil {
		ix = s.src.Index()
	}
	if ix == nil {
		if req.Serving != nil {
			return nil, pipeline.ErrServingCapture
		}
		return nil, nil
	}
	m := ix.IdentifyAt(req.Client, at)
	req.Client.GroupIDs = m.GroupIDs
	selected := &identification{membership: m, index: ix, at: at}
	ctx := req.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	req.Ctx = context.WithValue(ctx, s, selected)
	return selected, nil
}
