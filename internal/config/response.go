package config

import (
	"fmt"

	"github.com/robertherbaugh/kudurru/internal/responsepolicy"
)

// ResponseProcessing controls transformations of forwarded DNS responses.
type ResponseProcessing struct {
	DNS64         DNS64     `koanf:"dns64" json:"dns64"`
	FilterAAAA    bool      `koanf:"filterAAAA" json:"filterAAAA"`
	BogusNXDOMAIN []string  `koanf:"bogusNXDOMAIN" json:"bogusNXDOMAIN"`
	NXDomainIPv4  []string  `koanf:"nxdomainIPv4" json:"nxdomainIPv4"`
	NXDomainIPv6  []string  `koanf:"nxdomainIPv6" json:"nxdomainIPv6"`
	TTL           int       `koanf:"ttl" json:"ttl"`
	Rebinding     Rebinding `koanf:"rebinding" json:"rebinding"`
}

// DNS64 configures IPv4 embedding for clients on a NAT64 network.
type DNS64 struct {
	Prefix         string   `koanf:"prefix" json:"prefix"`
	ExcludeDomains []string `koanf:"excludeDomains" json:"excludeDomains"`
	ExcludeIPv4    []string `koanf:"excludeIPv4" json:"excludeIPv4"`
}

// Rebinding protects forwarded responses from unexpected private addresses.
type Rebinding struct {
	Enabled        bool     `koanf:"enabled" json:"enabled"`
	PrivateDomains []string `koanf:"privateDomains" json:"privateDomains"`
	BypassNetworks []string `koanf:"bypassNetworks" json:"bypassNetworks"`
}

// Compile produces a private immutable response-processing plan.
func (c ResponseProcessing) Compile() (*responsepolicy.Settings, error) {
	if c.TTL < 0 || c.TTL > 86400 {
		return nil, fmt.Errorf("response.ttl: expected 1 to 86400 seconds")
	}
	return responsepolicy.Compile(responsepolicy.Options{
		DNS64Prefix: c.DNS64.Prefix, DNS64ExcludeDomains: c.DNS64.ExcludeDomains, DNS64ExcludeIPv4: c.DNS64.ExcludeIPv4,
		FilterAAAA: c.FilterAAAA, BogusNXDOMAIN: c.BogusNXDOMAIN, NXDomainIPv4: c.NXDomainIPv4, NXDomainIPv6: c.NXDomainIPv6,
		TTL:       uint32(c.TTL),
		Rebinding: c.Rebinding.Enabled, PrivateDomains: c.Rebinding.PrivateDomains, BypassNetworks: c.Rebinding.BypassNetworks,
	})
}
