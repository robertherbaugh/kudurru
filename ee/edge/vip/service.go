package vip

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"
)

// ServiceProvider renders allocation annotations and, when explicitly mounted
// through ServiceObserver, reports bounded provider ownership evidence. It
// never derives an owner from an assigned Service IP or allocation annotation.
type ServiceProvider struct {
	// Kind is "kube-vip" or "metallb".
	Kind        string
	vips        []VIP
	mu          sync.RWMutex
	observation []OwnerObservation
	now         func() time.Time
}

// NewServiceProvider returns the annotation provider for kind.
func NewServiceProvider(kind string, vips []VIP) (*ServiceProvider, error) {
	switch kind {
	case "kube-vip", "metallb":
	default:
		return nil, fmt.Errorf("vip: unknown Service-based provider %q (want kube-vip or metallb)", kind)
	}
	return &ServiceProvider{Kind: kind, vips: slices.Clone(vips), now: time.Now}, nil
}

// Name implements Provider.
func (p *ServiceProvider) Name() string { return p.Kind }

// VIPs implements Provider.
func (p *ServiceProvider) VIPs() []VIP {
	out := slices.Clone(p.vips)
	snapshot := p.Snapshot()
	for i := range out {
		out[i].Owner = ""
		for _, item := range snapshot.VIPs {
			if item.Name == out[i].Name && item.Known {
				out[i].Owner = item.Owner
			}
		}
	}
	return out
}

// Owner reports only unexpired provider evidence from explicit observation.
func (p *ServiceProvider) Owner(name string) (string, bool) {
	for _, item := range p.Snapshot().VIPs {
		if item.Name == name {
			return item.Owner, item.Known
		}
	}
	return "", false
}

// Snapshot copies evidence and marks locally expired acquisition unknown. A
// read never extends either a provider Lease or an acquisition deadline.
func (p *ServiceProvider) Snapshot() Observation {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := Observation{Provider: p.Kind, VIPs: make([]OwnerObservation, 0, len(p.vips))}
	for _, v := range p.vips {
		item := OwnerObservation{Name: v.Name, Address: v.Address, Reason: "not_observed"}
		for _, observed := range p.observation {
			if observed.Name == v.Name {
				item = observed
				break
			}
		}
		now := p.now()
		if item.Known && (now.Before(item.CheckedAt) || !now.Before(item.ExpiresAt)) {
			item.Owner, item.Known, item.Reason = "", false, "stale"
		}
		if item.Evidence != nil {
			cloned := *item.Evidence
			if cloned.RenewedAt != nil {
				v := *cloned.RenewedAt
				cloned.RenewedAt = &v
			}
			if cloned.LivenessAt != nil {
				v := *cloned.LivenessAt
				cloned.LivenessAt = &v
			}
			item.Evidence = &cloned
		}
		out.VIPs = append(out.VIPs, item)
	}
	return out
}

// SetWeight implements Provider: a no-op for Service-based providers.
func (p *ServiceProvider) SetWeight(context.Context, string, Weight) error { return nil }

// Events implements Provider: none yet.
func (p *ServiceProvider) Events() <-chan Event { return nil }

// Annotations returns the Service annotations requesting the VIP address
// from the provider; sharingKey lets MetalLB put the UDP and TCP Services
// of one VIP on the same address.
func (p *ServiceProvider) Annotations(vip, sharingKey string) map[string]string {
	return p.AnnotationsWithOptions(vip, ServiceAnnotationOptions{SharingKey: sharingKey})
}

// ServiceAnnotationOptions describes MetalLB pool and split-Service requests.
// kube-vip ignores these MetalLB-specific options.
type ServiceAnnotationOptions struct {
	Pool       string
	SharingKey string
}

// AnnotationsWithOptions is the provider contract mirrored by the Helm DNS
// Service helper and checked against actual rendered charts in deploy tests.
func (p *ServiceProvider) AnnotationsWithOptions(vip string, options ServiceAnnotationOptions) map[string]string {
	var addr string
	for _, v := range p.vips {
		if v.Name == vip {
			addr = v.Address
		}
	}
	if addr == "" {
		return nil
	}
	switch p.Kind {
	case "kube-vip":
		return map[string]string{"kube-vip.io/loadbalancerIPs": addr}
	default:
		a := map[string]string{"metallb.io/loadBalancerIPs": addr}
		if options.Pool != "" {
			a["metallb.io/address-pool"] = options.Pool
		}
		if options.SharingKey != "" {
			a["metallb.io/allow-shared-ip"] = options.SharingKey
		}
		return a
	}
}
