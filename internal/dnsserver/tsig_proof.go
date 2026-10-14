package dnsserver

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/netip"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// TSIGIdentity names the immutable key and provider generation that verified
// a request. Versions are private equality tokens, never authorization grants.
type TSIGIdentity struct {
	Key, Algorithm, CredentialVersion, ProviderVersion string
}

func (k *StaticKeyring) sealVersions() {
	provider := sha256.New()
	_, _ = provider.Write([]byte("kudurru/tsig/provider/v1\x00"))
	for _, name := range k.Names() {
		key := k.keys[name]
		mac := hmac.New(sha256.New, key.Secret)
		_, _ = mac.Write([]byte("kudurru/tsig/credential/v1\x00" + name + "\x00" + key.Algorithm))
		version := hex.EncodeToString(mac.Sum(nil))
		k.versions[name] = version
		_, _ = provider.Write([]byte(name + "\x00" + version + "\x00"))
	}
	k.version = hex.EncodeToString(provider.Sum(nil))
}

// Identity returns version metadata from this exact immutable provider. It
// must not be used to infer which version a different provider verified.
func (k *StaticKeyring) Identity(name string) (TSIGIdentity, bool) {
	if k == nil {
		return TSIGIdentity{}, false
	}
	name = TSIGKeyName(name)
	key, ok := k.keys[name]
	if !ok || k.versions[name] == "" || k.version == "" {
		return TSIGIdentity{}, false
	}
	return TSIGIdentity{Key: name, Algorithm: key.Algorithm, CredentialVersion: k.versions[name], ProviderVersion: k.version}, true
}

type verifiedTSIGKey struct{}

// VerifiedTSIG is an opaque proof emitted only by successful native listener
// verification of UPDATE against an immutable provider. It pins the detached
// parsed request that miekg verified; it is not a re-created original wire MAC
// proof suitable for forwarding to another node. A zero value proves nothing.
// Current credential, domain, membership and HA admission are still required
// at the mutation boundary, including for a no-op or receipt replay.
type VerifiedTSIG struct {
	identity             TSIGIdentity
	message              *dns.Msg
	digest               [sha256.Size]byte
	proto                pipeline.Proto
	addr                 netip.AddrPort
	proxy                netip.Addr
	verifiedAt, deadline time.Time
}

// Identity returns detached verification-time identity metadata.
func (p *VerifiedTSIG) Identity() TSIGIdentity {
	if p == nil {
		return TSIGIdentity{}
	}
	return p.identity
}

// Message returns a private copy of the signed parsed UPDATE message.
func (p *VerifiedTSIG) Message() *dns.Msg {
	if p == nil || p.message == nil {
		return nil
	}
	return p.message.Copy()
}

// Deadline is the last instant allowed by the original signature's time fudge.
func (p *VerifiedTSIG) Deadline() time.Time {
	if p == nil {
		return time.Time{}
	}
	return p.deadline
}

func (p *VerifiedTSIG) String() string { return "private verified TSIG request" }

// GoString keeps request contents and credential versions out of diagnostics.
func (p *VerifiedTSIG) GoString() string { return p.String() }

// MarshalJSON prevents accidental publication of private verification data.
func (*VerifiedTSIG) MarshalJSON() ([]byte, error) { return []byte(`{"private":true}`), nil }

var _ json.Marshaler = (*VerifiedTSIG)(nil)

// VerifiedTSIGFor retrieves a transport proof only when this exact UPDATE,
// client and supported transport match it and its signature is still timely.
// A synthetic request, a target subquery or a mutable-provider lookup cannot
// manufacture this context capability. No store/keyring/network read occurs.
func VerifiedTSIGFor(req *pipeline.Request, now time.Time) (*VerifiedTSIG, bool) {
	if req == nil || req.Ctx == nil || req.Client.Synthetic {
		return nil, false
	}
	p, ok := req.Ctx.Value(verifiedTSIGKey{}).(*VerifiedTSIG)
	if !ok || p == nil || p.message == nil || p.identity.CredentialVersion == "" || now.Before(p.verifiedAt) || !now.Before(p.deadline) || req.Client.Proto != p.proto || req.Client.Addr != p.addr || req.Client.ProxiedFrom != p.proxy || req.Client.TSIGKey != p.identity.Key {
		return nil, false
	}
	digest, ok := updateMessageDigest(req.Msg)
	if !ok || digest != p.digest {
		return nil, false
	}
	return p, true
}

// This is called only AFTER miekg has verified MAC, algorithm and TSIG time.
// The type assertion is deliberate: an arbitrary Lookup implementation may
// rotate between Verify and ServeDNS, so cannot supply a truthful pinned proof.
func captureVerifiedTSIG(e *env, msg *dns.Msg, client pipeline.Client, now time.Time) *VerifiedTSIG {
	if e == nil || client.Synthetic || (client.Proto != pipeline.ProtoUDP && client.Proto != pipeline.ProtoTCP && client.Proto != pipeline.ProtoDoT) || msg == nil || msg.Opcode != dns.OpcodeUpdate {
		return nil
	}
	provider, ok := e.tsig.(keyringProvider)
	if !ok {
		return nil
	}
	keys, ok := provider.k.(*StaticKeyring)
	if !ok {
		return nil
	}
	identity, ok := keys.Identity(client.TSIGKey)
	if !ok {
		return nil
	}
	t := msg.IsTsig()
	if t == nil || dns.CanonicalName(t.Algorithm) != identity.Algorithm || t.TimeSigned > math.MaxInt64-uint64(t.Fudge) {
		return nil
	}
	deadline := time.Unix(int64(t.TimeSigned)+int64(t.Fudge), 0) //nolint:gosec // G115: bounded above before conversion.
	if !now.Before(deadline) {
		return nil
	}
	digest, ok := updateMessageDigest(msg)
	if !ok {
		return nil
	}
	return &VerifiedTSIG{identity: identity, message: msg.Copy(), digest: digest, proto: client.Proto, addr: client.Addr, proxy: client.ProxiedFrom, verifiedAt: now, deadline: deadline}
}

func updateMessageDigest(msg *dns.Msg) ([sha256.Size]byte, bool) {
	if msg == nil || msg.Opcode != dns.OpcodeUpdate || msg.Response {
		return [sha256.Size]byte{}, false
	}
	// Compression is a wire encoding choice, not signed UPDATE semantics. Cap
	// expanded message size before copying to bound proof workspace.
	copyHeader := *msg
	copyHeader.Compress = false
	if copyHeader.Len() > maxDNSMessage {
		return [sha256.Size]byte{}, false
	}
	raw, err := copyHeader.Pack()
	if err != nil || len(raw) > maxDNSMessage {
		return [sha256.Size]byte{}, false
	}
	return sha256.Sum256(raw), true
}
