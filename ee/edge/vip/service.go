package vip

import (
	"context"
	"fmt"
	"slices"
)

// ServiceProvider is the stub for `vip.provider: kube-vip|metallb` (D19):
// the VIP is a LoadBalancer Service address announced by kube-vip or
// MetalLB (both Apache-2.0, Service-aware). kudurru only adds annotations
// to the Service the chart renders; weights do not exist and ownership is
// in the Service status / the announcing node, which this stub does not
// read yet (Owner reports unknown; follow-up in docs/followups.md).
type ServiceProvider struct {
	// Kind is "kube-vip" or "metallb".
	Kind string
	vips []VIP
}

// NewServiceProvider returns the annotation provider for kind.
func NewServiceProvider(kind string, vips []VIP) (*ServiceProvider, error) {
	switch kind {
	case "kube-vip", "metallb":
	default:
		return nil, fmt.Errorf("vip: unknown Service-based provider %q (want kube-vip or metallb)", kind)
	}
	return &ServiceProvider{Kind: kind, vips: slices.Clone(vips)}, nil
}

// Name implements Provider.
func (p *ServiceProvider) Name() string { return p.Kind }

// VIPs implements Provider.
func (p *ServiceProvider) VIPs() []VIP { return slices.Clone(p.vips) }

// Owner implements Provider: unknown until the Service status reader
// lands.
func (p *ServiceProvider) Owner(string) (string, bool) { return "", false }

// SetWeight implements Provider: a no-op for Service-based providers.
func (p *ServiceProvider) SetWeight(context.Context, string, Weight) error { return nil }

// Events implements Provider: none yet.
func (p *ServiceProvider) Events() <-chan Event { return nil }

// Annotations returns the Service annotations requesting the VIP address
// from the provider; sharingKey lets MetalLB put the UDP and TCP Services
// of one VIP on the same address.
func (p *ServiceProvider) Annotations(vip, sharingKey string) map[string]string {
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
		if sharingKey != "" {
			a["metallb.io/allow-shared-ip"] = sharingKey
		}
		return a
	}
}
