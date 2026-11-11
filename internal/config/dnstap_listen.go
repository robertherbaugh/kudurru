package config

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// dnstap listen (D372): listeners.dnstap receives dnstap messages in Frame
// Streams from a front end such as dnsdist or Unbound and records the
// client queries they describe in this node's query log. The listeners
// bind at startup, so the settings are host-only and need a restart.

// Bounds of listeners.dnstap.
const (
	// MaxDnstapListeners bounds the list.
	MaxDnstapListeners = 8
	// DefaultDnstapListenConnections and MaxDnstapListenConnections bound
	// the senders one listener serves at a time.
	DefaultDnstapListenConnections = 16
	MaxDnstapListenConnections     = 256
	// MaxDnstapListenEntries bounds allowFrom, identities and ignoreClients.
	MaxDnstapListenEntries = 64
)

// DnstapListener is one entry of listeners.dnstap.
type DnstapListener struct {
	// Address is where the listener binds: unix:/run/kudurru/dnstap.sock,
	// tcp:127.0.0.1:6000 or tls:0.0.0.0:6000.
	Address string `koanf:"address" json:"address"`
	// TLS is the certificate and key of a tls: listener.
	TLS TLSFiles `koanf:"tls" json:"tls"`
	// ClientCAFile requires a client certificate issued by this CA bundle
	// on a tls: listener.
	ClientCAFile string `koanf:"clientCAFile" json:"clientCAFile,omitempty"`
	// AllowFrom are the peers, as addresses or CIDRs, that may connect to a
	// tcp: or tls: listener. Required for tcp:, and for tls: without
	// clientCAFile.
	AllowFrom []string `koanf:"allowFrom" json:"allowFrom"`
	// Identities, when set, are the only dnstap identities accepted;
	// messages with another identity are counted and dropped.
	Identities []string `koanf:"identities" json:"identities"`
	// IgnoreClients drops messages whose client address lies in one of
	// these prefixes, e.g. this node's own address when the sender is a
	// resolver kudurru forwards to.
	IgnoreClients []string `koanf:"ignoreClients" json:"ignoreClients"`
	// MaxConnections bounds the senders served at a time; 0 means
	// DefaultDnstapListenConnections.
	MaxConnections int `koanf:"maxConnections" json:"maxConnections,omitempty"`
}

// Target returns the parsed address.
func (l DnstapListener) Target() (DnstapTarget, error) { return ParseDnstapTarget(l.Address) }

// Connections returns MaxConnections with the default applied.
func (l DnstapListener) Connections() int {
	if l.MaxConnections <= 0 {
		return DefaultDnstapListenConnections
	}
	return l.MaxConnections
}

// AllowedPeers parses AllowFrom.
func (l DnstapListener) AllowedPeers() ([]netip.Prefix, error) { return parsePrefixes(l.AllowFrom) }

// IgnoredClients parses IgnoreClients.
func (l DnstapListener) IgnoredClients() ([]netip.Prefix, error) {
	return parsePrefixes(l.IgnoreClients)
}

// parsePrefixes reads addresses and CIDRs; an address is a host prefix.
func parsePrefixes(in []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(in))
	for _, raw := range in {
		raw = strings.TrimSpace(raw)
		if p, err := netip.ParsePrefix(raw); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(raw)
		if err != nil {
			return nil, fmt.Errorf("%q is neither an address nor a CIDR", raw)
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// validateDnstap checks listeners.dnstap.
func (l *Listeners) validateDnstap() []error {
	var errs []error
	if len(l.Dnstap) > MaxDnstapListeners {
		errs = append(errs, fmt.Errorf("listeners.dnstap: at most %d listeners", MaxDnstapListeners))
	}
	seen := map[string]bool{}
	for i, d := range l.Dnstap {
		at := fmt.Sprintf("listeners.dnstap[%d]", i)
		target, err := d.Target()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s.address %w", at, err))
			continue
		}
		if seen[target.Address] {
			errs = append(errs, fmt.Errorf("%s.address: %s is listed twice", at, target.Address))
		}
		seen[target.Address] = true
		if target.Scheme != DnstapSchemeUnix {
			if _, err := netip.ParseAddr(strings.Trim(target.Host(), "[]")); err != nil {
				errs = append(errs, fmt.Errorf("%s.address: listen on an IP address, e.g. tcp:127.0.0.1:6000", at))
			}
		}
		tlsSet := d.TLS.CertFile != "" || d.TLS.KeyFile != "" || d.ClientCAFile != ""
		switch target.Scheme {
		case DnstapSchemeTLS:
			if d.TLS.CertFile == "" || d.TLS.KeyFile == "" {
				errs = append(errs, fmt.Errorf("%s.tls: a tls: listener needs certFile and keyFile", at))
			}
			if d.ClientCAFile == "" && len(d.AllowFrom) == 0 {
				errs = append(errs, fmt.Errorf("%s: a tls: listener needs clientCAFile or allowFrom, so that only your front ends can write to the query log", at))
			}
		case DnstapSchemeTCP:
			if tlsSet {
				errs = append(errs, fmt.Errorf("%s: tls and clientCAFile apply to a tls: listener only", at))
			}
			if len(d.AllowFrom) == 0 {
				errs = append(errs, fmt.Errorf("%s.allowFrom: a tcp: listener needs the addresses of the front ends that may connect", at))
			}
		case DnstapSchemeUnix:
			if tlsSet {
				errs = append(errs, fmt.Errorf("%s: tls and clientCAFile apply to a tls: listener only", at))
			}
			if len(d.AllowFrom) > 0 {
				errs = append(errs, fmt.Errorf("%s.allowFrom applies to a tcp: or tls: listener only; a Unix socket is guarded by its file permissions", at))
			}
		}
		for _, list := range []struct {
			name   string
			values []string
		}{{"allowFrom", d.AllowFrom}, {"identities", d.Identities}, {"ignoreClients", d.IgnoreClients}} {
			if len(list.values) > MaxDnstapListenEntries {
				errs = append(errs, fmt.Errorf("%s.%s: at most %d entries", at, list.name, MaxDnstapListenEntries))
			}
		}
		if _, err := d.AllowedPeers(); err != nil {
			errs = append(errs, fmt.Errorf("%s.allowFrom: %w", at, err))
		}
		if _, err := d.IgnoredClients(); err != nil {
			errs = append(errs, fmt.Errorf("%s.ignoreClients: %w", at, err))
		}
		for _, id := range d.Identities {
			if id == "" || len(id) > MaxDnstapIdentity || strings.ContainsFunc(id, func(r rune) bool { return r < ' ' || r == 0x7f }) {
				errs = append(errs, fmt.Errorf("%s.identities: use 1 to %d characters without control characters", at, MaxDnstapIdentity))
				break
			}
		}
		if d.MaxConnections < 0 || d.MaxConnections > MaxDnstapListenConnections {
			errs = append(errs, fmt.Errorf("%s.maxConnections: use 0 (default %d) to %d", at, DefaultDnstapListenConnections, MaxDnstapListenConnections))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return []error{errors.Join(errs...)}
}
