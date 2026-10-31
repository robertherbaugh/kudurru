package recursor

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/netip"
	"sync"
	"time"

	"github.com/miekg/dns"
)

const (
	maxCookiePeers   = 4096
	serverCookieLife = time.Hour
	cookieProbeDelay = 5 * time.Minute
	clientCookieLife = 24 * time.Hour
)

var errCookie = errors.New("recursor: invalid authority DNS cookie")

// Cookies belong to a connected local IP and remote endpoint, not an NS name,
// DNS question, client-supplied option, or validation/response cache entry.
type cookiePeer struct {
	local  netip.Addr
	server netip.AddrPort
}

type peerCookie struct {
	client           [8]byte
	created, expires time.Time
	server           []byte // opaque: only the authority can verify its own MAC
	serverUntil      time.Time
	probeAfter       time.Time
}

type cookieJar struct {
	mu    sync.Mutex
	peers map[cookiePeer]*peerCookie
	now   func() time.Time
}

type cookieRequest struct {
	peer        cookiePeer
	client      [8]byte
	sent, known bool
}

func newCookieJar(now func() time.Time) *cookieJar {
	return &cookieJar{peers: make(map[cookiePeer]*peerCookie), now: now}
}

func freshCookie(now time.Time) *peerCookie {
	p := &peerCookie{created: now}
	_, _ = rand.Read(p.client[:])
	// Independent jitter avoids synchronized rollover; no client cookie lasts
	// longer than a day (RFC7873 section7.1, RFC9018 section3).
	jitter := time.Duration(cryptoUint64() % uint64(clientCookieLife*2/5))
	p.expires = now.Add(clientCookieLife - jitter)
	return p
}

// prepare runs after dialing, so a route/source-address change never reuses a
// cookie learned on another address. The private wire message is owned here.
func (j *cookieJar) prepare(m *dns.Msg, peer cookiePeer) cookieRequest {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := j.now()
	p := j.peers[peer]
	if p == nil || now.Before(p.created) || !now.Before(p.expires) {
		if p == nil && len(j.peers) >= maxCookiePeers {
			clear(j.peers)
		}
		p = freshCookie(now)
		j.peers[peer] = p
	}
	r := cookieRequest{peer: peer, client: p.client}
	stripCookies(m)
	if now.Before(p.probeAfter) {
		return r
	}
	r.sent = true
	r.known = len(p.server) > 0
	data := append([]byte(nil), p.client[:]...)
	if now.Before(p.serverUntil) {
		data = append(data, p.server...)
	}
	if m.IsEdns0() == nil {
		m.SetEdns0(ednsSize, false)
	}
	opt := m.IsEdns0()
	opt.Option = append(opt.Option, &dns.EDNS0_COOKIE{Code: dns.EDNS0COOKIE, Cookie: hex.EncodeToString(data)})
	return r
}

// accept checks only the first COOKIE option per RFC7873 section5.3. Replies
// have an 8-byte echo plus an 8..32-byte opaque server cookie. Even BADCOOKIE
// must authenticate the echo before it can change state or trigger a retry.
func (j *cookieJar) accept(m *dns.Msg, r cookieRequest) error {
	var cookie *dns.EDNS0_COOKIE
	var opt *dns.OPT
	for _, rr := range m.Extra {
		if candidate, ok := rr.(*dns.OPT); ok {
			// Multiple OPT records are malformed (RFC6891 section6.1.1);
			// do not let a later OPT hide an incorrect first cookie.
			if opt != nil {
				return errCookie
			}
			opt = candidate
		}
	}
	if opt != nil {
		for _, option := range opt.Option {
			if option.Option() == dns.EDNS0COOKIE {
				var ok bool
				cookie, ok = option.(*dns.EDNS0_COOKIE)
				if !ok {
					return errCookie
				}
				break
			}
		}
	}
	var data []byte
	if cookie != nil {
		if !r.sent || len(cookie.Cookie) < 32 || len(cookie.Cookie) > 80 {
			return errCookie
		}
		var err error
		data, err = hex.DecodeString(cookie.Cookie)
		if err != nil || subtle.ConstantTimeCompare(data[:8], r.client[:]) != 1 {
			return errCookie
		}
	} else if r.known || m.Rcode == dns.RcodeBadCookie {
		return errCookie
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	now := j.now()
	p := j.peers[r.peer]
	// A rotated/evicted in-flight request can finish, but cannot repopulate or
	// overwrite current transport authority with its older cookie pair.
	if p == nil || p.client != r.client || now.Before(p.created) || !now.Before(p.expires) {
		return nil
	}
	if cookie != nil {
		if !bytes.Equal(p.server, data[8:]) {
			p.server = append([]byte(nil), data[8:]...)
			p.serverUntil = minTime(now.Add(serverCookieLife), p.expires)
		}
		p.probeAfter = time.Time{}
	} else if r.sent && len(p.server) == 0 {
		// RFC9018: never expose this same client cookie again to a server that
		// did not support cookies. Suppress probes, then use fresh entropy.
		p = freshCookie(now)
		p.probeAfter = now.Add(cookieProbeDelay)
		j.peers[r.peer] = p
	}
	return nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func stripCookies(m *dns.Msg) {
	if opt := m.IsEdns0(); opt != nil {
		options := opt.Option[:0]
		for _, option := range opt.Option {
			if option.Option() != dns.EDNS0COOKIE {
				options = append(options, option)
			}
		}
		opt.Option = options
	}
}
