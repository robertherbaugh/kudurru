package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strings"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/recursor"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
)

// RootAnchorMode never treats an empty effective key set as static fallback.
type RootAnchorMode string

// RootAnchorsStatic and RootAnchorsManaged select configured or managed roots.
const (
	RootAnchorsStatic  RootAnchorMode = "static"
	RootAnchorsManaged RootAnchorMode = "managed"
)

// RootAnchorPolicy is detached configuration, not accepted history or serving
// authority. Its identity is stable across seed ordering and presentation TTLs.
type RootAnchorPolicy struct {
	Mode       RootAnchorMode
	PolicyID   string
	SeedDigest string
	Seeds      []string
}

// ValidateManagedRootTopology checks the host deployment declaration. It cannot
// discover undeclared proxies and supplies no accepted trust or writer authority.
func (c *Config) ValidateManagedRootTopology() error {
	if c == nil {
		return errors.New("upstreams.recursion.managedTopology: missing configuration")
	}
	switch c.Upstreams.Recursion.ManagedTopology {
	case "", "disabled":
		return nil
	case "direct":
		if c.Store.Driver != "sqlite" || len(c.Listeners.ProxyProtocolFrom) != 0 || c.Listeners.ProxyProtocolSecretFile != "" {
			return errors.New("upstreams.recursion.managedTopology: direct requires SQLite listeners without PROXYv2 ingress")
		}
		return nil
	default:
		return errors.New("upstreams.recursion.managedTopology: expected disabled or direct")
	}
}

// RootAnchorPolicy requires explicit, bounded supported DS seeds for managed
// mode. Shipped roots are never substituted here, including on empty input.
func (r Recursion) RootAnchorPolicy() (RootAnchorPolicy, error) {
	switch r.AnchorMode {
	case "", RootAnchorsStatic:
		return RootAnchorPolicy{Mode: RootAnchorsStatic}, nil
	case RootAnchorsManaged:
	default:
		return RootAnchorPolicy{}, errors.New("upstreams.recursion.anchorMode: expected static or managed")
	}
	if !r.DNSSEC {
		return RootAnchorPolicy{}, errors.New("upstreams.recursion.anchorMode: managed roots require DNSSEC")
	}
	if len(r.TrustAnchors) == 0 || len(r.TrustAnchors) > trustanchors.MaxLiveKeys {
		return RootAnchorPolicy{}, errors.New("upstreams.recursion.trustAnchors: managed roots require bounded explicit DS seeds")
	}
	for _, text := range r.TrustAnchors {
		if len(text) > 2048 || strings.TrimSpace(text) == "" {
			return RootAnchorPolicy{}, errors.New("upstreams.recursion.trustAnchors: invalid managed DS seed")
		}
	}
	parsed, err := recursor.ParseTrustAnchors(r.TrustAnchors)
	if err != nil {
		return RootAnchorPolicy{}, errors.New("upstreams.recursion.trustAnchors: invalid managed DS seed")
	}
	policy := RootAnchorPolicy{Mode: RootAnchorsManaged, Seeds: make([]string, 0, len(parsed))}
	for _, seed := range parsed {
		switch seed.Algorithm {
		case dns.RSASHA1, dns.RSASHA1NSEC3SHA1, dns.RSASHA256, dns.RSASHA512, dns.ECDSAP256SHA256, dns.ECDSAP384SHA384, dns.ED25519:
		default:
			return RootAnchorPolicy{}, errors.New("upstreams.recursion.trustAnchors: unsupported managed DS algorithm")
		}
		length := 0
		switch seed.DigestType {
		case dns.SHA1:
			length = 20
		case dns.SHA256:
			length = 32
		case dns.SHA384:
			length = 48
		}
		digest, err := hex.DecodeString(seed.Digest)
		if seed.Hdr.Class != dns.ClassINET || length == 0 || err != nil || len(digest) != length {
			return RootAnchorPolicy{}, errors.New("upstreams.recursion.trustAnchors: invalid managed DS digest")
		}
		seed.Hdr.Ttl = 0
		seed.Digest = strings.ToUpper(seed.Digest)
		policy.Seeds = append(policy.Seeds, seed.String())
	}
	slices.Sort(policy.Seeds)
	for i := 1; i < len(policy.Seeds); i++ {
		if policy.Seeds[i-1] == policy.Seeds[i] {
			return RootAnchorPolicy{}, errors.New("upstreams.recursion.trustAnchors: duplicate managed DS seed")
		}
	}
	digest := sha256.Sum256([]byte(strings.Join(policy.Seeds, "\n")))
	policy.SeedDigest = hex.EncodeToString(digest[:])
	policy.PolicyID = "recursive-root-rfc5011-v1:" + policy.SeedDigest
	return policy, nil
}
