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

// Serve implements pipeline.Stage.
func (s *Stage) Serve(req *pipeline.Request, next pipeline.Next) (*pipeline.Response, error) {
	at := req.Received
	if at.IsZero() {
		at = s.now()
	}
	if (s.Inventory != nil || s.CaptureInventory != nil || s.Hostnames != nil || req.Serving != nil) && !req.Client.IdentityCaptured {
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
		return next(req)
	}
	m := ix.IdentifyAt(req.Client, at)
	req.Client.GroupIDs = m.GroupIDs
	req.Policy = ix.Select(m, at)
	return next(req)
}
