package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/dnssec"
)

// DNSSEC configures validation of forwarded answers. Trust anchors are
// managed by the operator or a software upgrade, never by an upstream reply.
type DNSSEC struct {
	Enabled              bool                  `koanf:"enabled" json:"enabled"`
	UseBuiltinAnchors    bool                  `koanf:"useBuiltinAnchors" json:"useBuiltinAnchors"`
	TrustAnchors         []string              `koanf:"trustAnchors" json:"trustAnchors"`
	NegativeTrustAnchors []NegativeTrustAnchor `koanf:"negativeTrustAnchors" json:"negativeTrustAnchors"`
}

// NegativeTrustAnchor is an explicit, temporary exception (RFC 7646).
// ExpiresAt is absolute, so neither reload nor restart extends the exception.
type NegativeTrustAnchor struct {
	Domain    string `koanf:"domain" json:"domain"`
	Reason    string `koanf:"reason" json:"reason"`
	ExpiresAt string `koanf:"expiresAt" json:"expiresAt"`
}

// Validator validates the configuration and compiles an immutable trust plan.
// Even when the global default is disabled it is available to forward zones
// that explicitly opt into validation. Expired exceptions remain valid
// configuration but never bypass validation.
func (c DNSSEC) Validator(now func() time.Time) (*dnssec.Validator, error) {
	if len(c.TrustAnchors) > 64 || len(c.NegativeTrustAnchors) > 128 {
		return nil, fmt.Errorf("dnssec: at most 64 trust anchors and 128 negative trust anchors are supported")
	}
	anchors := make([]dns.RR, 0, len(c.TrustAnchors)+2)
	if c.UseBuiltinAnchors {
		anchors = append(anchors, dnssec.RootAnchors()...)
	}
	for i, text := range c.TrustAnchors {
		rr, err := dns.NewRR(text)
		if err != nil || rr == nil {
			return nil, fmt.Errorf("dnssec.trustAnchors[%d]: expected a DS or DNSKEY record", i)
		}
		anchors = append(anchors, rr)
	}
	ntas := make([]dnssec.NTA, 0, len(c.NegativeTrustAnchors))
	seen := make(map[string]bool, len(c.NegativeTrustAnchors))
	for i, n := range c.NegativeTrustAnchors {
		domain := dns.CanonicalName(strings.TrimSpace(n.Domain))
		if strings.TrimSpace(n.Domain) == "" || strings.TrimSpace(n.Reason) == "" {
			return nil, fmt.Errorf("dnssec.negativeTrustAnchors[%d]: a domain and reason are required", i)
		}
		if seen[domain] {
			return nil, fmt.Errorf("dnssec.negativeTrustAnchors[%d]: duplicate domain %q", i, domain)
		}
		seen[domain] = true
		expires, err := time.Parse(time.RFC3339, n.ExpiresAt)
		if err != nil {
			return nil, fmt.Errorf("dnssec.negativeTrustAnchors[%d].expiresAt: expected an absolute RFC3339 timestamp", i)
		}
		ntas = append(ntas, dnssec.NTA{Domain: domain, Reason: n.Reason, ExpiresAt: expires})
	}
	return dnssec.New(dnssec.Options{Anchors: anchors, NTAs: ntas, Now: now})
}

// NTAs returns private, typed native-recursion exception settings.
func (c Recursion) NTAs() []dnssec.NTA {
	out := make([]dnssec.NTA, 0, len(c.NegativeTrustAnchors))
	for _, n := range c.NegativeTrustAnchors {
		out = append(out, dnssec.NTA{Domain: n.Domain, Reason: n.Reason, ExpiresAt: n.ExpiresAt})
	}
	return out
}
