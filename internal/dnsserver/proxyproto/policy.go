package proxyproto

import (
	"bytes"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strings"
	"sync/atomic"
)

// MinSecretLen is the shortest shared secret NewPolicy accepts. The
// architecture calls for 32 random bytes; `openssl rand -hex 32` produces 64
// printable bytes carrying the same entropy and is the recommended form.
const MinSecretLen = 32

// Reason is the bounded label value a dropped header is counted under, as in
// kudurru_dns_dropped_total{reason="proxy_unauthenticated"}.
type Reason string

// Drop reasons.
const (
	// ReasonUnauthenticated: the peer is allowed but the header lacks a TLV
	// 0xE0 equal to the shared secret (or a secret is required and unset).
	ReasonUnauthenticated Reason = "proxy_unauthenticated"
	// ReasonUntrustedPeer: the peer is not in listeners.proxyProtocolFrom.
	ReasonUntrustedPeer Reason = "proxy_untrusted_peer"
	// ReasonMalformed: the header is truncated, violates the specification,
	// is too long or carries an address family DNS cannot use.
	ReasonMalformed Reason = "proxy_malformed"
)

// Reasons lists every Reason, for metrics that pre-create their series.
var Reasons = []Reason{ReasonUnauthenticated, ReasonUntrustedPeer, ReasonMalformed}

func (r Reason) index() int {
	switch r {
	case ReasonUnauthenticated:
		return 0
	case ReasonUntrustedPeer:
		return 1
	default:
		return 2
	}
}

// DropError reports a header that was refused. The datagram or connection
// it arrived on must be discarded.
type DropError struct {
	Reason Reason
	Peer   netip.AddrPort
	Err    error
}

func (e *DropError) Error() string {
	return fmt.Sprintf("proxyproto: dropped header from %s (%s): %v", e.Peer, e.Reason, e.Err)
}

func (e *DropError) Unwrap() error { return e.Err }

var (
	errUntrusted = errors.New("peer is not in proxyProtocolFrom")
	errNoSecret  = errors.New("TLV 0xE0 missing")
	errBadSecret = errors.New("TLV 0xE0 does not match the shared secret")
	errNeedTLV   = errors.New("non-loopback peers require a shared secret")
	errFamily    = errors.New("PROXY command with an address family DNS cannot use")
)

// loopback4 and loopback6 are the only peers exempt from the TLV when no
// secret is configured (D56). Other 127/8 addresses are deliberately not
// exempt: the decision names the two host addresses, and treating 127.0.0.2
// as an ordinary peer keeps the rule testable on one host.
var (
	loopback4 = netip.AddrFrom4([4]byte{127, 0, 0, 1})
	loopback6 = netip.IPv6Loopback()
)

func exemptLoopback(a netip.Addr) bool { return a == loopback4 || a == loopback6 }

// Policy decides which PROXYv2 headers are honoured: the transport peer must
// be listed in proxyProtocolFrom (hosts or CIDRs) and the header must carry
// TLV 0xE0 equal to the shared secret. When no secret is configured only the
// loopback peers 127.0.0.1 and ::1 may send headers, without the TLV.
// Everything else is dropped and counted.
//
// A nil or empty Policy is disabled: listeners do not inspect traffic at all.
// A Policy is safe for concurrent use.
type Policy struct {
	peers   map[netip.Addr]struct{}
	ranges  []netip.Prefix
	secret  []byte
	dropped [3]atomic.Uint64
	hook    atomic.Pointer[func(Reason)]
}

// NewPolicy builds a policy from the listeners.proxyProtocolFrom entries
// (bare addresses or CIDR prefixes) and the shared secret (nil when unset).
//
// Wider prefixes require a secret (D72), because CIDR trust alone is
// forgeable by any neighbour in the range (D45). It refuses secrets shorter
// than MinSecretLen and non-loopback peers without a secret (D56).
func NewPolicy(from []string, secret []byte) (*Policy, error) {
	if len(secret) > 0 && len(secret) < MinSecretLen {
		return nil, fmt.Errorf("proxyproto: shared secret is %d bytes, need at least %d", len(secret), MinSecretLen)
	}
	p := &Policy{peers: make(map[netip.Addr]struct{}, len(from))}
	if len(secret) > 0 {
		p.secret = bytes.Clone(secret)
	}
	var errs []error
	for _, entry := range from {
		entry = strings.TrimSpace(entry)
		pfx, err := parsePeer(entry)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !pfx.IsSingleIP() {
			if p.secret == nil {
				errs = append(errs, fmt.Errorf("proxyproto: proxyProtocolFrom entry %q is a /%d; only exact hosts (/32, /128) are allowed without a shared secret (set listeners.proxyProtocolSecretFile)", entry, pfx.Bits()))
			} else {
				p.ranges = append(p.ranges, pfx)
			}
			continue
		}
		addr := pfx.Addr().Unmap()
		if p.secret == nil && !exemptLoopback(addr) {
			errs = append(errs, fmt.Errorf("proxyproto: peer %s: %w (set listeners.proxyProtocolSecretFile)", addr, errNeedTLV))
			continue
		}
		p.peers[addr] = struct{}{}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return p, nil
}

func parsePeer(entry string) (netip.Prefix, error) {
	if !strings.Contains(entry, "/") {
		a, err := netip.ParseAddr(entry)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("proxyproto: proxyProtocolFrom entry %q: %w", entry, err)
		}
		a = a.Unmap()
		return netip.PrefixFrom(a, a.BitLen()), nil
	}
	pfx, err := netip.ParsePrefix(entry)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("proxyproto: proxyProtocolFrom entry %q: %w", entry, err)
	}
	return pfx.Masked(), nil
}

// LoadSecretFile reads the shared secret from path. Trailing CR/LF are
// removed so that `openssl rand -hex 32 > file` works; the remaining bytes
// are compared verbatim with the TLV value, so the dnsdist supervisor must
// send exactly the same bytes. An empty path returns (nil, nil).
func LoadSecretFile(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path) //nolint:gosec // the operator names the file (listeners.proxyProtocolSecretFile)
	if err != nil {
		return nil, fmt.Errorf("proxyproto: read secret: %w", err)
	}
	b = bytes.TrimRight(b, "\r\n")
	if len(b) < MinSecretLen {
		return nil, fmt.Errorf("proxyproto: secret in %s is %d bytes, need at least %d", path, len(b), MinSecretLen)
	}
	return b, nil
}

// Enabled reports whether any peer may send headers.
func (p *Policy) Enabled() bool { return p != nil && (len(p.peers) > 0 || len(p.ranges) > 0) }

// Trusted reports whether peer is listed in proxyProtocolFrom.
func (p *Policy) Trusted(peer netip.Addr) bool {
	if p == nil {
		return false
	}
	peer = peer.Unmap()
	if _, ok := p.peers[peer]; ok {
		return true
	}
	for _, pfx := range p.ranges {
		if pfx.Contains(peer) {
			return true
		}
	}
	return false
}

// SetDropHook registers fn to be called (synchronously, on the I/O path)
// for every dropped header; it must not block. internal/metrics uses it to
// increment kudurru_dns_dropped_total. Passing nil removes the hook.
func (p *Policy) SetDropHook(fn func(Reason)) {
	if p == nil {
		return
	}
	if fn == nil {
		p.hook.Store(nil)
		return
	}
	p.hook.Store(&fn)
}

// Dropped returns how many headers were dropped for reason.
func (p *Policy) Dropped(reason Reason) uint64 {
	if p == nil {
		return 0
	}
	return p.dropped[reason.index()].Load()
}

// DroppedTotal returns the number of headers dropped for any reason.
func (p *Policy) DroppedTotal() uint64 {
	var n uint64
	for _, r := range Reasons {
		n += p.Dropped(r)
	}
	return n
}

func (p *Policy) drop(reason Reason, peer netip.AddrPort, err error) *DropError {
	p.dropped[reason.index()].Add(1)
	if fn := p.hook.Load(); fn != nil {
		(*fn)(reason)
	}
	return &DropError{Reason: reason, Peer: peer, Err: err}
}

// authenticate applies the policy to a parsed header from peer and returns
// the address the query is attributed to and whether it differs from the
// transport peer (CommandProxy with an IP family).
func (p *Policy) authenticate(peer netip.AddrPort, h *Header) (netip.AddrPort, bool, error) {
	peerAddr := peer.Addr().Unmap()
	if !p.Trusted(peerAddr) {
		return netip.AddrPort{}, false, p.drop(ReasonUntrustedPeer, peer, errUntrusted)
	}
	if p.secret != nil {
		v, ok := h.TLV(TLVTypeSecret)
		if !ok {
			return netip.AddrPort{}, false, p.drop(ReasonUnauthenticated, peer, errNoSecret)
		}
		if subtle.ConstantTimeCompare(v, p.secret) != 1 {
			return netip.AddrPort{}, false, p.drop(ReasonUnauthenticated, peer, errBadSecret)
		}
	} else if !exemptLoopback(peerAddr) {
		// Unreachable through NewPolicy; kept so a hand-built Policy fails
		// closed.
		return netip.AddrPort{}, false, p.drop(ReasonUnauthenticated, peer, errNeedTLV)
	}
	if h.Command == CommandLocal {
		return peer, false, nil
	}
	switch h.Family {
	case FamilyINET, FamilyINET6:
		src := netip.AddrPortFrom(h.Source.Addr().Unmap(), h.Source.Port())
		return src, true, nil
	case FamilyUnspec:
		// The specification lets the receiver use the real endpoints.
		return peer, false, nil
	default:
		return netip.AddrPort{}, false, p.drop(ReasonMalformed, peer, errFamily)
	}
}

// Datagram inspects one UDP payload b received from peer.
//
//   - No signature: b is an ordinary query; it is returned unchanged,
//     attributed to peer.
//   - An authenticated header: the payload after the header is returned with
//     the client address from the header; proxied reports whether that
//     address came from the header (false for LOCAL and AF_UNSPEC headers).
//   - Anything else: the drop is counted and a *DropError returned.
//
// The returned payload aliases b. On a disabled Policy Datagram returns b
// unchanged.
func (p *Policy) Datagram(peer netip.AddrPort, b []byte) (payload []byte, client netip.AddrPort, proxied bool, err error) {
	if !p.Enabled() || !HasSignature(b) {
		return b, peer, false, nil
	}
	// Refuse untrusted peers before parsing anything else.
	if !p.Trusted(peer.Addr()) {
		return nil, netip.AddrPort{}, false, p.drop(ReasonUntrustedPeer, peer, errUntrusted)
	}
	h, n, perr := Parse(b)
	if perr != nil {
		return nil, netip.AddrPort{}, false, p.drop(ReasonMalformed, peer, perr)
	}
	client, proxied, err = p.authenticate(peer, &h)
	if err != nil {
		return nil, netip.AddrPort{}, false, err
	}
	return b[n:], client, proxied, nil
}

// Stream reads the start of a stream connection from r (whose transport
// peer is peer) and returns:
//
//   - for a connection without a header, the bytes it consumed (to be
//     replayed before further reads) and peer;
//   - for an authenticated header, no bytes, the client address and whether
//     it came from the header;
//   - otherwise a *DropError (counted) or the read error.
//
// It reads only as far as needed to rule a header out, so a DNS or TLS
// stream costs one Read. The caller sets the read deadline. On a disabled
// Policy Stream reads nothing.
func (p *Policy) Stream(peer netip.AddrPort, r io.Reader) (consumed []byte, client netip.AddrPort, proxied bool, err error) {
	if !p.Enabled() {
		return nil, peer, false, nil
	}
	var fixed [fixedLen]byte
	n := 0
	for n < fixedLen {
		m, rerr := r.Read(fixed[n:])
		n += m
		if !maybeSignature(fixed[:n]) {
			return bytes.Clone(fixed[:n]), peer, false, nil
		}
		if rerr != nil {
			if n == 0 {
				return nil, peer, false, rerr
			}
			// A partial signature followed by EOF or a timeout.
			return nil, netip.AddrPort{}, false, p.drop(ReasonMalformed, peer, fmt.Errorf("%w: %w", ErrTruncated, rerr))
		}
	}
	if !p.Trusted(peer.Addr()) {
		return nil, netip.AddrPort{}, false, p.drop(ReasonUntrustedPeer, peer, errUntrusted)
	}
	total, herr := headerLen(fixed[:])
	if herr != nil {
		return nil, netip.AddrPort{}, false, p.drop(ReasonMalformed, peer, herr)
	}
	buf := make([]byte, total)
	copy(buf, fixed[:])
	if _, rerr := io.ReadFull(r, buf[fixedLen:]); rerr != nil {
		return nil, netip.AddrPort{}, false, p.drop(ReasonMalformed, peer, fmt.Errorf("%w: %w", ErrTruncated, rerr))
	}
	h, _, perr := Parse(buf)
	if perr != nil {
		return nil, netip.AddrPort{}, false, p.drop(ReasonMalformed, peer, perr)
	}
	client, proxied, err = p.authenticate(peer, &h)
	if err != nil {
		return nil, netip.AddrPort{}, false, err
	}
	return nil, client, proxied, nil
}
