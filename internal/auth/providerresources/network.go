package providerresources

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/oidccore"
	"github.com/robertherbaugh/kudurru/internal/credentials"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
)

type networkPolicy struct {
	origins, local map[string]bool
	algorithms     []string
	roots          *x509.CertPool
	identity       string
}
type originGrantKey struct{}
type originGrant struct{ local, loopbackOnly bool }

var hostSerial atomic.Uint64

// NewHost validates and detaches actual host root/endpoint policy. Roots are
// explicit PEM inputs so trust identity binds certificate bytes, not pool names.
func NewHost(opts HostOptions) (*HostCut, error) {
	for _, s := range []string{opts.Identity, opts.CredentialPolicy, opts.EntitlementIdentity, opts.FingerprintKeyID} {
		if !safe(s, 512, false) {
			return nil, ErrInvalid
		}
	}
	if opts.ConfigSources == nil || len(opts.ConfigSources) > 64 || opts.AllowedOrigins == nil || opts.LocalOrigins == nil || len(opts.AllowedOrigins) > 128 || len(opts.LocalOrigins) > 128 || len(opts.Algorithms) > 10 || len(opts.ConfigCredentials) > 64 || len(opts.TrustPEM) > MaxTrustBytes {
		return nil, ErrBounds
	}
	n := &networkPolicy{origins: map[string]bool{}, local: map[string]bool{}, roots: x509.NewCertPool(), algorithms: sortedStrings(opts.Algorithms)}
	if len(n.algorithms) == 0 {
		n.algorithms = []string{"RS256"}
	}
	for i, a := range n.algorithms {
		if !slices.Contains([]string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512", "EdDSA"}, a) || i > 0 && n.algorithms[i-1] == a {
			return nil, ErrInvalid
		}
	}
	for _, s := range opts.AllowedOrigins {
		origin, err := canonicalOrigin(s)
		if err != nil || origin != s || n.origins[s] {
			return nil, ErrInvalid
		}
		n.origins[s] = true
	}
	for _, s := range opts.LocalOrigins {
		if !n.origins[s] || n.local[s] {
			return nil, ErrInvalid
		}
		n.local[s] = true
	}
	certificates := []string{}
	rest := opts.TrustPEM
	for len(strings.TrimSpace(string(rest))) > 0 {
		if !strings.HasPrefix(strings.TrimSpace(string(rest)), "-----BEGIN CERTIFICATE-----") {
			return nil, ErrInvalid
		}
		block, next := pem.Decode(rest)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return nil, ErrInvalid
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil || !cert.IsCA || !cert.BasicConstraintsValid {
			return nil, ErrInvalid
		}
		if len(certificates) >= MaxTrustCertificates {
			return nil, ErrBounds
		}
		identity := hash(block.Bytes)
		if slices.Contains(certificates, identity) {
			return nil, ErrInvalid
		}
		certificates = append(certificates, identity)
		n.roots.AddCert(cert)
		rest = next
	}
	if len(n.origins) > 0 && len(certificates) == 0 {
		return nil, ErrInvalid
	}
	slices.Sort(certificates)
	n.identity = hash(struct {
		Profile                           string
		Origins, Local, Algorithms, Roots []string
	}{"provider-network/v1-tls12-no-proxy-no-redirect", sortedStrings(opts.AllowedOrigins), sortedStrings(opts.LocalOrigins), n.algorithms, certificates})
	serial := uint64(0)
	for {
		old := hostSerial.Load()
		if old == math.MaxUint64 {
			return nil, ErrBounds
		}
		if hostSerial.CompareAndSwap(old, old+1) {
			serial = old + 1
			break
		}
	}
	h := &HostCut{serial: serial, configSpecs: map[string]string{}, identity: opts.Identity, credentialPolicy: opts.CredentialPolicy, entitlement: opts.EntitlementIdentity, fingerprintKey: opts.FingerprintKeyID, allowed: opts.ProvidersAllowed, objects: opts.ObjectCredentials, configs: map[string]*credentials.Resolver{}, network: n}
	budget := 0
	for _, s := range opts.ConfigSources {
		if s.Ref.Origin != servingauth.ProviderConfig || h.configSpecs[s.Ref.ID] != "" || !safe(s.Ref.UID, 512, false) || s.CreatedAt.IsZero() {
			return nil, ErrInvalid
		}
		if err := checkBudget(reflect.ValueOf(s.Spec), &budget); err != nil {
			return nil, err
		}
		if _, err := oidccore.Compile(s.Spec, s.Ref.ID); err != nil {
			return nil, ErrInvalid
		}
		h.configSpecs[s.Ref.ID] = hash(struct {
			Profile   string
			Ref       servingauth.ProviderRef
			CreatedAt time.Time
			Spec      any
		}{"servingauth/provider/v1", s.Ref, s.CreatedAt.UTC(), s.Spec})
	}
	for id, r := range opts.ConfigCredentials {
		if !safe(id, 63, false) || r == nil {
			return nil, ErrInvalid
		}
		h.configs[id] = r
	}
	return h, nil
}

// NetworkIdentity binds actual captured roots, origins, exceptions, and algorithms.
func (h *HostCut) NetworkIdentity() string {
	if h == nil || h.network == nil {
		return ""
	}
	return h.network.identity
}

func canonicalOrigin(raw string) (string, error) {
	if len(raw) > 2048 {
		return "", ErrBounds
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.Path != "" {
		return "", ErrInvalid
	}
	return u.Scheme + "://" + u.Host, nil
}
func (n *networkPolicy) endpoint(raw string) (*url.URL, error) {
	if !safe(raw, 2048, false) {
		return nil, ErrInvalid
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || u.Scheme != "https" && u.Scheme != "http" {
		return nil, ErrInvalid
	}
	origin := u.Scheme + "://" + u.Host
	if !n.origins[origin] || u.Scheme == "http" && !n.local[origin] {
		return nil, ErrUnavailable
	}
	if a, err := netip.ParseAddr(u.Hostname()); err == nil && (neverAllowed(a) || forbidden(a) && !n.local[origin]) {
		return nil, ErrUnavailable
	}
	return u, nil
}
func neverAllowed(a netip.Addr) bool {
	a = a.Unmap()
	return !a.IsValid() || a.IsUnspecified() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsMulticast() || netip.MustParsePrefix("0.0.0.0/8").Contains(a) || a == netip.MustParseAddr("fd00:ec2::254") || a == netip.MustParseAddr("100.100.100.200")
}
func forbidden(a netip.Addr) bool {
	a = a.Unmap()
	return !a.IsValid() || a.IsLoopback() || a.IsUnspecified() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsMulticast() || netip.MustParsePrefix("0.0.0.0/8").Contains(a) || a == netip.MustParseAddr("fd00:ec2::254") || a == netip.MustParseAddr("100.100.100.200")
}

type boundedTransport struct {
	policy *networkPolicy
	base   *http.Transport
}

func (t boundedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := t.policy.endpoint(req.URL.String())
	if err != nil {
		return nil, err
	}
	local := t.policy.local[u.Scheme+"://"+u.Host]
	return t.base.RoundTrip(req.Clone(context.WithValue(req.Context(), originGrantKey{}, originGrant{local, u.Scheme == "http"})))
}
func (n *networkPolicy) client(timeout time.Duration) (*http.Client, func()) {
	d := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second, ControlContext: func(ctx context.Context, _ string, address string, _ syscall.RawConn) error {
		addr, err := netip.ParseAddrPort(address)
		if err != nil {
			return ErrUnavailable
		}
		grant, _ := ctx.Value(originGrantKey{}).(originGrant)
		if neverAllowed(addr.Addr()) || forbidden(addr.Addr()) && !grant.local || grant.loopbackOnly && !addr.Addr().IsLoopback() {
			return ErrUnavailable
		}
		return nil
	}}
	tr := &http.Transport{Proxy: nil, DialContext: d.DialContext, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: n.roots.Clone()}, ForceAttemptHTTP2: true, MaxResponseHeaderBytes: 16 << 10, ResponseHeaderTimeout: timeout, MaxIdleConns: 8, MaxIdleConnsPerHost: 2, IdleConnTimeout: time.Minute}
	return &http.Client{Timeout: timeout, Transport: boundedTransport{n, tr}, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrUnavailable }}, tr.CloseIdleConnections
}
