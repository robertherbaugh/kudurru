package config

import (
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"
)

// The dnstap sink of querylog.sinks (D372): dnstap messages in Frame Streams
// over a Unix socket, TCP or TLS to a collector such as DNS-collector,
// Vector or the dnstap command.

// QuerylogSinkDnstap is the querylog.sinks type that sends dnstap.
const QuerylogSinkDnstap = "dnstap"

// dnstap message types a sink can send (querylog.sinks[].dnstap.types).
const (
	DnstapClientQuery       = "client_query"
	DnstapClientResponse    = "client_response"
	DnstapAuthQuery         = "auth_query"
	DnstapAuthResponse      = "auth_response"
	DnstapForwarderQuery    = "forwarder_query"
	DnstapForwarderResponse = "forwarder_response"
)

// DnstapTypes are the message types in the order the schema lists them.
var DnstapTypes = []string{DnstapClientQuery, DnstapClientResponse, DnstapAuthQuery, DnstapAuthResponse, DnstapForwarderQuery, DnstapForwarderResponse}

// DefaultDnstapTypes are the types of a sink without types: the query and
// the response of every client exchange, as AUTH_* when a local zone
// answered and as CLIENT_* otherwise.
var DefaultDnstapTypes = []string{DnstapClientQuery, DnstapClientResponse, DnstapAuthQuery, DnstapAuthResponse}

// Target schemes of a dnstap sink.
const (
	DnstapSchemeUnix = "unix"
	DnstapSchemeTCP  = "tcp"
	DnstapSchemeTLS  = "tls"
)

// MaxDnstapIdentity bounds the identity a sink sends.
const MaxDnstapIdentity = 255

// QuerylogDnstap is the dnstap object of a querylog.sinks entry.
type QuerylogDnstap struct {
	// Target is where the collector listens: unix:/run/dnstap.sock,
	// tcp:collector.example:6000 or tls:collector.example:6000.
	Target string `koanf:"target" json:"target"`
	// ServerName is the name the TLS certificate must carry; empty uses the
	// host of target.
	ServerName string `koanf:"serverName" json:"serverName,omitempty"`
	// CARef names a PEM bundle that replaces the system trust store; CertRef
	// and KeyRef a client certificate and its key for mutual TLS.
	CARef   *CredentialRef `koanf:"caRef" json:"caRef,omitempty"`
	CertRef *CredentialRef `koanf:"certRef" json:"certRef,omitempty"`
	KeyRef  *CredentialRef `koanf:"keyRef" json:"keyRef,omitempty"`
	// Identity is the dnstap identity; empty sends node.name.
	Identity string `koanf:"identity" json:"identity,omitempty"`
	// Types are the message types sent; empty means DefaultDnstapTypes.
	Types []string `koanf:"types" json:"types,omitempty"`
	// Raw sends the DNS messages as they were received and answered and the
	// real client address, whatever querylog.privacy says. Only a principal
	// with querylog.raw may write a settings document that contains it, and
	// every node that starts such a sink records an audit event first (D372).
	Raw bool `koanf:"raw" json:"raw,omitempty"`
}

// DnstapTarget is a parsed QuerylogDnstap.Target.
type DnstapTarget struct {
	// Scheme is unix, tcp or tls.
	Scheme string
	// Address is the socket path or host:port.
	Address string
}

// Network is the net.Dial network of the target.
func (t DnstapTarget) Network() string {
	if t.Scheme == DnstapSchemeUnix {
		return "unix"
	}
	return "tcp"
}

// Host is the host of a tcp or tls target.
func (t DnstapTarget) Host() string {
	host, _, err := net.SplitHostPort(t.Address)
	if err != nil {
		return ""
	}
	return host
}

// ParseDnstapTarget parses a target. Its errors complete the sentence
// "<path> ...".
func ParseDnstapTarget(raw string) (DnstapTarget, error) {
	scheme, address, ok := strings.Cut(raw, ":")
	if !ok || address == "" {
		return DnstapTarget{}, fmt.Errorf("is not unix:/path, tcp:host:port or tls:host:port")
	}
	if len(raw) > 1024 || strings.ContainsFunc(raw, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return DnstapTarget{}, fmt.Errorf("must be at most 1024 characters without spaces or control characters")
	}
	switch scheme {
	case DnstapSchemeUnix:
		if !filepath.IsAbs(address) || filepath.Clean(address) != address {
			return DnstapTarget{}, fmt.Errorf("must name a clean absolute socket path after unix:")
		}
	case DnstapSchemeTCP, DnstapSchemeTLS:
		host, port, err := net.SplitHostPort(address)
		if err != nil || host == "" {
			return DnstapTarget{}, fmt.Errorf("must be host:port after %s:", scheme)
		}
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return DnstapTarget{}, fmt.Errorf("has an invalid port")
		}
	default:
		return DnstapTarget{}, fmt.Errorf("is not unix:/path, tcp:host:port or tls:host:port")
	}
	return DnstapTarget{Scheme: scheme, Address: address}, nil
}

// MessageTypes returns the types with the default applied.
func (d QuerylogDnstap) MessageTypes() []string {
	if len(d.Types) == 0 {
		return append([]string(nil), DefaultDnstapTypes...)
	}
	return append([]string(nil), d.Types...)
}

// Validate reports everything that keeps the sink from being built; at is
// the path of the object, e.g. querylog.sinks[0].dnstap.
func (d *QuerylogDnstap) Validate(at string) []error {
	var errs []error
	target, err := ParseDnstapTarget(d.Target)
	if d.Target != "" && err != nil { // the shape reports a missing target
		errs = append(errs, fmt.Errorf("%s.target %w", at, err))
	}
	tlsTarget := err == nil && target.Scheme == DnstapSchemeTLS
	if !tlsTarget {
		for _, f := range []struct {
			name string
			set  bool
		}{{"serverName", d.ServerName != ""}, {"caRef", d.CARef != nil}, {"certRef", d.CertRef != nil}, {"keyRef", d.KeyRef != nil}} {
			if f.set && err == nil {
				errs = append(errs, fmt.Errorf("%s.%s applies to a tls: target only", at, f.name))
			}
		}
	}
	if d.ServerName != "" && (len(d.ServerName) > 253 || strings.ContainsFunc(d.ServerName, func(r rune) bool { return r <= ' ' || r == 0x7f || r == '/' || r == ':' })) {
		errs = append(errs, fmt.Errorf("%s.serverName: %q is not a host name", at, d.ServerName))
	}
	if (d.CertRef == nil) != (d.KeyRef == nil) {
		errs = append(errs, fmt.Errorf("%s: set certRef and keyRef together", at))
	}
	for _, r := range []struct {
		name string
		ref  *CredentialRef
	}{{"caRef", d.CARef}, {"certRef", d.CertRef}, {"keyRef", d.KeyRef}} {
		if r.ref != nil {
			errs = append(errs, r.ref.validate(at+"."+r.name)...)
		}
	}
	if len(d.Identity) > MaxDnstapIdentity || strings.ContainsFunc(d.Identity, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		errs = append(errs, fmt.Errorf("%s.identity: use at most %d characters without control characters", at, MaxDnstapIdentity))
	}
	seen := map[string]bool{}
	for _, typ := range d.Types {
		known := false
		for _, t := range DnstapTypes {
			known = known || t == typ
		}
		switch {
		case !known:
			errs = append(errs, fmt.Errorf("%s.types: %q is not one of %s", at, typ, strings.Join(DnstapTypes, ", ")))
		case seen[typ]:
			errs = append(errs, fmt.Errorf("%s.types: %q is listed twice", at, typ))
		}
		seen[typ] = true
	}
	return errs
}

// RawQuerylogSinks lists the names of the entries that send raw dnstap
// (querylog.sinks[].dnstap.raw), in configuration order.
func RawQuerylogSinks(q Querylog) []string {
	var out []string
	for _, s := range q.Sinks {
		if s.Type == QuerylogSinkDnstap && s.Dnstap != nil && s.Dnstap.Raw {
			out = append(out, s.Name)
		}
	}
	return out
}

// QuerylogSinksKey is the settings key of the additional sinks.
const QuerylogSinksKey = "querylog.sinks"
