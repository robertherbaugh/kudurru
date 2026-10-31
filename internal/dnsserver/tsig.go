package dnsserver

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"iter"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// TSIG (RFC 8945) on all DNS listeners.
//
// Every miekg server gets a TsigProvider backed by the node's TSIGKeyring,
// so the signature of every signed request is verified before the
// pipeline runs (with no keyring every key is unknown). A request whose
// signature fails is answered here, NOTAUTH with the TSIG error BADKEY,
// BADSIG or BADTIME (RFC 8945 §5.2), and never reaches the pipeline. A
// verified request carries the key name in pipeline.Client.TSIGKey, and
// every message of its reply is signed with the same key: the first with
// the request MAC, later messages of a transfer stream with the previous
// MAC and the timers only (§5.3.1). DoH, DoQ and DoH3 verify original wire
// bytes and sign one reply using encrypted_tsig.go. Their transport-local
// signer never grants native UPDATE or NOTIFY mutation proof.

// TSIGKeyring is the TSIG key provider seam: it returns the secret and the
// algorithm of a key by name. Names are compared in canonical form
// (lower-case, absolute). v0.1 implements it with StaticKeyring loaded
// from the tsigKeys setting (KeyringFromConfig); the store-backed internal
// TSIGKey kind (dev-docs/design-record.md §4.1, encrypted at rest) is a later
// implementation of the same interface.
type TSIGKeyring interface {
	// Lookup returns the raw secret and the algorithm (a TSIG algorithm
	// name such as "hmac-sha256.") of the named key.
	Lookup(name string) (secret []byte, algorithm string, ok bool)
}

// TSIG algorithms accepted in tsigKeys. HMAC-SHA256 is mandatory to
// implement (RFC 8945 §6); HMAC-SHA512 is the stronger common choice.
const (
	TSIGHMACSHA256 = "hmac-sha256"
	TSIGHMACSHA512 = "hmac-sha512"
)

// TSIGAlgorithm returns the wire name ("hmac-sha256.") of a supported
// algorithm given in any case, with or without the trailing dot.
func TSIGAlgorithm(name string) (string, bool) {
	switch strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".") {
	case TSIGHMACSHA256:
		return dns.HmacSHA256, true
	case TSIGHMACSHA512:
		return dns.HmacSHA512, true
	}
	return "", false
}

// TSIGKeyName returns the canonical form of a key name.
func TSIGKeyName(name string) string { return dns.CanonicalName(strings.TrimSpace(name)) }

// TSIGKey is one shared secret.
type TSIGKey struct {
	// Name is the key name, a domain name such as "transfer-key.".
	Name string
	// Algorithm is hmac-sha256 or hmac-sha512.
	Algorithm string
	// Secret is the raw (decoded) secret.
	Secret []byte
}

// StaticKeyring is an immutable TSIGKeyring over a fixed set of keys.
type StaticKeyring struct {
	keys     map[string]TSIGKey
	versions map[string]string
	version  string
}

var _ TSIGKeyring = (*StaticKeyring)(nil)

// NewStaticKeyring validates keys and returns a keyring. Names must be
// unique (case-insensitively), algorithms supported and secrets non-empty.
func NewStaticKeyring(keys ...TSIGKey) (*StaticKeyring, error) {
	k := &StaticKeyring{keys: make(map[string]TSIGKey, len(keys)), versions: make(map[string]string, len(keys))}
	var errs []error
	for i, key := range keys {
		name := TSIGKeyName(key.Name)
		if _, ok := dns.IsDomainName(name); !ok || name == "." {
			errs = append(errs, fmt.Errorf("tsig key %d: name %q is not a domain name", i, key.Name))
			continue
		}
		alg, ok := TSIGAlgorithm(key.Algorithm)
		if !ok {
			errs = append(errs, fmt.Errorf("tsig key %s: algorithm %q is not supported (%s, %s)", name, key.Algorithm, TSIGHMACSHA256, TSIGHMACSHA512))
			continue
		}
		if len(key.Secret) == 0 {
			errs = append(errs, fmt.Errorf("tsig key %s: empty secret", name))
			continue
		}
		if _, dup := k.keys[name]; dup {
			errs = append(errs, fmt.Errorf("tsig key %s: defined twice", name))
			continue
		}
		k.keys[name] = TSIGKey{Name: name, Algorithm: alg, Secret: append([]byte(nil), key.Secret...)}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	k.sealVersions()
	return k, nil
}

// Lookup implements TSIGKeyring. The returned secret is a detached copy;
// callers cannot mutate the provider generation used to verify requests.
func (k *StaticKeyring) Lookup(name string) ([]byte, string, bool) {
	if k == nil {
		return nil, "", false
	}
	key, ok := k.keys[TSIGKeyName(name)]
	return append([]byte(nil), key.Secret...), key.Algorithm, ok
}

// Names returns the key names, sorted.
func (k *StaticKeyring) Names() []string {
	if k == nil {
		return nil
	}
	out := make([]string, 0, len(k.keys))
	for n := range k.keys {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// KeyringFromConfig loads the tsigKeys setting: each secretFile holds the
// base64 secret (the format of BIND's key statement and tsig-keygen),
// surrounding whitespace ignored. An empty list yields an empty keyring.
func KeyringFromConfig(keys []config.TSIGKey) (*StaticKeyring, error) {
	out := make([]TSIGKey, 0, len(keys))
	var errs []error
	for i, c := range keys {
		raw, err := os.ReadFile(c.SecretFile)
		if err != nil {
			errs = append(errs, fmt.Errorf("tsigKeys[%d] (%s): %w", i, c.Name, err))
			continue
		}
		secret, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil {
			errs = append(errs, fmt.Errorf("tsigKeys[%d] (%s): %s does not hold a base64 secret: %w", i, c.Name, c.SecretFile, err))
			continue
		}
		out = append(out, TSIGKey{Name: c.Name, Algorithm: c.Algorithm, Secret: secret})
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return NewStaticKeyring(out...)
}

// TSIGProvider adapts a keyring to miekg/dns's TsigProvider for servers,
// clients and transfers. Unknown keys fail with dns.ErrSecret (BADKEY), an
// algorithm other than the key's with dns.ErrKeyAlg (BADKEY) and a wrong
// MAC with dns.ErrSig (BADSIG). A nil keyring knows no key.
func TSIGProvider(k TSIGKeyring) dns.TsigProvider { return keyringProvider{k} }

type keyringProvider struct{ k TSIGKeyring }

func (p keyringProvider) mac(t *dns.TSIG) (hash.Hash, error) {
	if p.k == nil {
		return nil, dns.ErrSecret
	}
	var secret []byte
	var alg string
	var ok bool
	if static, immutable := p.k.(*StaticKeyring); immutable && static != nil {
		// The private path avoids a per-message secret copy. Only this package
		// can reach the immutable backing bytes, and hash constructors copy them.
		key, found := static.keys[TSIGKeyName(t.Hdr.Name)]
		secret, alg, ok = key.Secret, key.Algorithm, found
	} else {
		secret, alg, ok = p.k.Lookup(t.Hdr.Name)
	}
	if !ok {
		return nil, dns.ErrSecret
	}
	want, ok := TSIGAlgorithm(alg)
	if !ok || dns.CanonicalName(t.Algorithm) != want {
		return nil, dns.ErrKeyAlg
	}
	switch want {
	case dns.HmacSHA256:
		return hmac.New(sha256.New, secret), nil
	default:
		return hmac.New(sha512.New, secret), nil
	}
}

// Generate implements dns.TsigProvider.
func (p keyringProvider) Generate(msg []byte, t *dns.TSIG) ([]byte, error) {
	h, err := p.mac(t)
	if err != nil {
		return nil, err
	}
	h.Write(msg)
	return h.Sum(nil), nil
}

// Verify implements dns.TsigProvider.
func (p keyringProvider) Verify(msg []byte, t *dns.TSIG) error {
	got, err := p.Generate(msg, t)
	if err != nil {
		return err
	}
	mac, err := hex.DecodeString(t.MAC)
	if err != nil || !hmac.Equal(got, mac) {
		return dns.ErrSig
	}
	return nil
}

// TSIG failure reasons reported to Options.OnTSIGFailure.
const (
	TSIGBadKey  = "badkey"
	TSIGBadSig  = "badsig"
	TSIGBadTime = "badtime"
)

// tsigFudge is the time fudge put on signed replies (RFC 8945 §5.2.3
// recommends 300 s).
const tsigFudge = 300

// tsigReserve is the room left for the TSIG record when a UDP reply is
// truncated: names, fixed fields and a 64-octet MAC.
const tsigReserve = 2*255 + 16 + 64 + 6

// signer signs the reply to a verified request with the request's key.
// The zero value (unsigned request) does nothing.
type signer struct {
	key, alg string
	proof    *VerifiedTSIG
}

func (s signer) signed() bool { return s.key != "" }

// sign attaches the TSIG stub that miekg's ResponseWriter fills in with
// the MAC when it writes m.
func (s signer) sign(m *dns.Msg) {
	if s.key != "" && m.IsTsig() == nil {
		m.SetTsig(s.key, s.alg, tsigFudge, time.Now().Unix())
	}
}

// verifyTSIG checks the TSIG status miekg computed for r. Unsigned
// requests pass with a zero signer. A verified request records its key on
// client and returns its signer. A failed one is answered here (NOTAUTH
// plus the TSIG error) and ok is false.
func verifyTSIG(h *Handler, e *env, w dns.ResponseWriter, r *dns.Msg, client *pipeline.Client) (signer, bool) {
	t := r.IsTsig()
	if t == nil {
		return signer{}, true
	}
	err := w.TsigStatus()
	if err == nil {
		client.TSIGKey = TSIGKeyName(t.Hdr.Name)
		return signer{key: t.Hdr.Name, alg: t.Algorithm, proof: captureVerifiedTSIG(e, r, *client, time.Now())}, true
	}
	reason, code := TSIGBadSig, uint16(dns.RcodeBadSig)
	switch {
	case errors.Is(err, dns.ErrSecret), errors.Is(err, dns.ErrKeyAlg):
		reason, code = TSIGBadKey, dns.RcodeBadKey
	case errors.Is(err, dns.ErrTime):
		reason, code = TSIGBadTime, dns.RcodeBadTime
	}
	if e != nil && e.onTSIGFailure != nil {
		e.onTSIGFailure(reason)
	}
	h.logger().Debug("tsig verification failed", "key", t.Hdr.Name, "reason", reason, "err", err)

	m := new(dns.Msg)
	m.SetRcode(r, dns.RcodeNotAuth)
	rr := &dns.TSIG{
		Hdr:        dns.RR_Header{Name: t.Hdr.Name, Rrtype: dns.TypeTSIG, Class: dns.ClassANY},
		Algorithm:  t.Algorithm,
		Fudge:      t.Fudge,
		TimeSigned: t.TimeSigned,
		OrigId:     r.Id,
		Error:      code,
	}
	if code == dns.RcodeBadTime {
		// The reply is signed and carries the server's clock so the client
		// can see the skew (RFC 8945 §5.2.3).
		var now [8]byte
		binary.BigEndian.PutUint64(now[:], uint64(time.Now().Unix())) //nolint:gosec // G115: Unix time is positive
		rr.OtherLen = 6
		rr.OtherData = hex.EncodeToString(now[2:])
	}
	m.Extra = append(m.Extra, rr)
	// BADKEY and BADSIG replies are unsigned (RFC 8945 §5.3.2); miekg's
	// writer leaves the MAC empty for them and signs BADTIME.
	m = paddingFor(r, client.Proto).apply(m)
	if err := w.WriteMsg(m); err != nil {
		h.logger().Debug("write failed", "err", err)
	}
	return signer{}, false
}

// writeStream writes a multi-message answer (pipeline.Response.Transfer)
// on a stream transport. With a signed request, every message is signed:
// the first with the full TSIG variables, the rest with the timers only,
// each chained to the previous MAC (RFC 8945 §5.3.1). At the first write
// error (the client went away, or stopped reading past the per-message
// write deadline) it closes the connection: the stream is cut mid-answer.
func writeStream(h *Handler, w dns.ResponseWriter, msgs iter.Seq[*dns.Msg], sig signer, padding responsePadding) {
	first := true
	for m := range msgs {
		sig.sign(m)
		m = padding.apply(m)
		if err := w.WriteMsg(m); err != nil {
			h.logger().Debug("transfer write failed", "err", err)
			_ = w.Close()
			return
		}
		if first && sig.signed() {
			w.TsigTimersOnly(true)
		}
		first = false
	}
}
