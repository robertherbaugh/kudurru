package servingbuild

import (
	"time"

	"github.com/robertherbaugh/kudurru/internal/inventory"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
	"github.com/robertherbaugh/kudurru/internal/responsepolicy"
	"github.com/robertherbaugh/kudurru/internal/rpz"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
	"github.com/robertherbaugh/kudurru/internal/servingzones"
	"github.com/robertherbaugh/kudurru/internal/stages/authoritative"
	"github.com/robertherbaugh/kudurru/internal/stages/blocking"
	"github.com/robertherbaugh/kudurru/internal/stages/cache"
	"github.com/robertherbaugh/kudurru/internal/stages/control"
	"github.com/robertherbaugh/kudurru/internal/stages/identify"
	"github.com/robertherbaugh/kudurru/internal/stages/postprocess"
	"github.com/robertherbaugh/kudurru/internal/stages/upstream"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

// DNSStages are the concrete per-process stages. Configure their captures once,
// before starting any listeners. Synthetic subqueries reuse these same stages.
type DNSStages struct {
	Chain      *pipeline.Chain
	Control    *control.Stage
	Identify   *identify.Stage
	Authority  *authoritative.Stage
	Blocking   *blocking.Stage
	Cache      *cache.Stage
	Response   *postprocess.Stage
	Resolution *upstream.Stage
}

// BindDNS makes every serving stage use the outer request's complete lease.
// No callback reads an independent config pointer, runtime manager or store.
func BindDNS(publisher *serving.Publisher, stages DNSStages, now func() time.Time) error {
	if publisher == nil || stages.Chain == nil || stages.Identify == nil || stages.Control == nil || stages.Authority == nil || stages.Blocking == nil || stages.Cache == nil || stages.Response == nil || stages.Resolution == nil {
		return serving.ErrComponents
	}
	if now == nil {
		now = time.Now
	}
	stages.Chain.SetCapture(servingdns.Capture(publisher))
	stages.Control.Capture = func(req *pipeline.Request) (*control.View, error) {
		return servingdns.Component[*control.View](req, "control")
	}
	stages.Identify.Capture = func(req *pipeline.Request) (*policy.Index, error) {
		return servingdns.Component[*policy.Index](req, "policy")
	}
	stages.Identify.CaptureInventory = func(req *pipeline.Request) (*inventory.Snapshot, error) {
		owner, err := servingdns.Component[*InventoryOwner](req, "inventory")
		if err != nil {
			return nil, err
		}
		return owner.Snapshot, nil
	}
	stages.Authority.Capture = func(req *pipeline.Request) zone.Set {
		owner, err := servingdns.Component[*servingzones.Prepared](req, "zones")
		if err != nil {
			return nil
		}
		return owner.Zones(now())
	}
	stages.Blocking.Capture = func(req *pipeline.Request) (*blocking.View, error) {
		return servingdns.Component[*blocking.View](req, "blocking")
	}
	stages.Blocking.SetRPZCapture(func(req *pipeline.Request) (*rpz.Selection, error) {
		owner, err := servingdns.Component[*servingzones.Prepared](req, "zones")
		if err != nil {
			return nil, err
		}
		var names []string
		if decision := policy.FromRequest(req); decision != nil && decision.Selection != nil {
			names = decision.Selection.BlockLists()
		}
		return owner.SelectRPZ(names)
	})
	stages.Cache.Capture = func(req *pipeline.Request) (*cache.View, error) {
		return servingdns.Component[*cache.View](req, "cache")
	}
	stages.Response.Capture = func(req *pipeline.Request) (*responsepolicy.Settings, error) {
		return servingdns.Component[*responsepolicy.Settings](req, "response")
	}
	stages.Resolution.Capture = func(req *pipeline.Request) (*upstream.View, error) {
		return servingdns.Component[*upstream.View](req, "resolution")
	}
	return nil
}
