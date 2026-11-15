package driver

import (
	"context"
	"encoding/json"

	"github.com/miekg/dns"

	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

type roster struct {
	zones    map[string]*public.DNSZone
	byApex   map[string]*public.DNSZone
	ledgers  map[string]*zk.ZoneLifecycle
	prepared *pr.PreparedImage
}

func (d *Driver) read(ctx context.Context) (roster, *pr.PreparedImage, sqlite.GenerationToken, error) {
	out := roster{zones: map[string]*public.DNSZone{}, byApex: map[string]*public.DNSZone{}, ledgers: map[string]*zk.ZoneLifecycle{}}
	image, token, err := d.o.Store.ReadServingCandidate(ctx)
	if err != nil {
		return out, nil, token, err
	}
	opts := d.o.Image
	opts.Now = time.Now()
	p, err := pr.PrepareImage(ctx, *image, opts)
	if err != nil {
		return out, nil, token, err
	}
	// Preparation strictly validated every carrier/zero manifest. Decode this
	// SAME detached candidate; no per-object read or source-absence inference.
	for _, raw := range image.Payload.Objects {
		if err := ctx.Err(); err != nil {
			return out, nil, token, err
		}
		var header struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(raw, &header); err != nil {
			return out, nil, token, err
		}
		switch header.Kind {
		case "DNSZone":
			var z public.DNSZone
			if err := json.Unmarshal(raw, &z); err != nil {
				return out, nil, token, err
			}
			out.zones[z.Name] = &z
			out.byApex[dns.CanonicalName(z.Spec.ZoneName)] = &z
		case zk.KindZoneLifecycle:
			var l zk.ZoneLifecycle
			if err := json.Unmarshal(raw, &l); err != nil {
				return out, nil, token, err
			}
			out.ledgers[l.Spec.Zone.ObjectName] = &l
		}
	}
	if len(out.zones) > d.o.MaxZones || len(out.ledgers) > d.o.MaxZones {
		return out, nil, token, pr.ErrCapacity
	}
	out.prepared = p
	return out, p, token, nil
}
