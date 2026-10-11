package config

import (
	"fmt"
	"net/netip"
	"strings"
)

// APIListener is the HTTP API and UI listener (docs/architecture.md §6.2,
// §10). Every key is hostOnly: a listener address or certificate path set
// through the API would be a privilege escalation.
type APIListener struct {
	Address string `koanf:"address" json:"address"`
	TLS     APITLS `koanf:"tls" json:"tls"`
	// PathPrefix mounts the API and UI below a reverse proxy path
	// ("/kudurru"); "" serves them at the root.
	PathPrefix string `koanf:"pathPrefix" json:"pathPrefix"`
	// TrustedProxies are the proxy addresses or CIDRs whose
	// X-Forwarded-For and X-Forwarded-Proto headers are believed.
	TrustedProxies []string `koanf:"trustedProxies" json:"trustedProxies"`
}

// APITLS configures TLS on the API listener.
type APITLS struct {
	// Auto is "selfsigned" (generate a certificate once into
	// <node.dataDir>/keys/) or "off"; ignored when CertFile is set.
	Auto     string `koanf:"auto" json:"auto"`
	CertFile string `koanf:"certFile" json:"certFile"`
	KeyFile  string `koanf:"keyFile" json:"keyFile"`
}

// API TLS modes accepted by listeners.api.tls.auto.
const (
	APITLSSelfSigned = "selfsigned"
	APITLSOff        = "off"
)

// Enabled reports whether the API listener terminates TLS.
func (t APITLS) Enabled() bool { return t.CertFile != "" || t.Auto == APITLSSelfSigned }

// validate checks the API listener keys.
func (l *APIListener) validate() []error {
	var errs []error
	switch l.TLS.Auto {
	case "", APITLSSelfSigned, APITLSOff:
	default:
		errs = append(errs, fmt.Errorf("listeners.api.tls.auto: unknown mode %q (selfsigned, off)", l.TLS.Auto))
	}
	if (l.TLS.CertFile == "") != (l.TLS.KeyFile == "") {
		errs = append(errs, fmt.Errorf("listeners.api.tls: certFile and keyFile must be set together"))
	}
	if p := l.PathPrefix; p != "" && (!strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") || strings.ContainsAny(p, "?#{} ")) {
		errs = append(errs, fmt.Errorf("listeners.api.pathPrefix: %q must start with / and not end with / (e.g. /kudurru)", p))
	}
	for _, e := range l.TrustedProxies {
		if _, err := ParsePrefixOrAddr(e); err != nil {
			errs = append(errs, fmt.Errorf("listeners.api.trustedProxies: %w", err))
		}
	}
	return errs
}

// ParsePrefixOrAddr parses "10.0.0.0/8" or "10.0.0.1" (returned as a
// single-address prefix).
func ParsePrefixOrAddr(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if p, err := netip.ParsePrefix(s); err == nil {
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%q is neither an address nor a CIDR", s)
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// ExternalDNS configures the external-dns integration (§5.6, §9.6).
type ExternalDNS struct {
	Webhook ExternalDNSWebhook `koanf:"webhook" json:"webhook"`
}

// ExternalDNSWebhook is the in-process external-dns webhook provider.
type ExternalDNSWebhook struct {
	Enabled bool   `koanf:"enabled" json:"enabled"`
	Listen  string `koanf:"listen" json:"listen"`
	// Namespace receives the DNSRecordSets written for external-dns.
	Namespace string `koanf:"namespace" json:"namespace"`
	// ZoneSelector is a label selector on DNSZones; empty selects every
	// primary zone.
	ZoneSelector map[string]string `koanf:"zoneSelector" json:"zoneSelector"`
}

// validate checks the webhook keys.
func (e *ExternalDNS) validate() []error {
	w := e.Webhook
	if !w.Enabled {
		return nil
	}
	var errs []error
	if w.Listen == "" {
		errs = append(errs, fmt.Errorf("externalDNS.webhook.listen is required when the webhook is enabled"))
	}
	if w.Namespace == "" {
		errs = append(errs, fmt.Errorf("externalDNS.webhook.namespace is required when the webhook is enabled"))
	}
	return errs
}
