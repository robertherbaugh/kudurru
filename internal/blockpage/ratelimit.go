package blockpage

import (
	"net/netip"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// maxLimited bounds the clients tracked by a rate limiter; when full,
// clients idle for a period are forgotten, and if none is, the table is
// reset (every client starts again with a full bucket).
const maxLimited = 16384

// limiter is a per-client token bucket: n requests per period with bursts
// of the same size. IPv6 clients are limited per /64 so address rotation
// within a network does not escape it. The page uses n per minute, the
// unblock request form n per hour.
type limiter struct {
	mu      sync.Mutex
	n       int
	per     time.Duration
	clients map[netip.Addr]*clientBucket
	now     func() time.Time
}

type clientBucket struct {
	lim  *rate.Limiter
	seen time.Time
}

func newLimiter(n int, per time.Duration, now func() time.Time) *limiter {
	return &limiter{n: n, per: per, clients: map[netip.Addr]*clientBucket{}, now: now}
}

// setRate changes the limit; existing buckets are dropped.
func (l *limiter) setRate(n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n != l.n {
		l.n = n
		l.clients = map[netip.Addr]*clientBucket{}
	}
}

// allow reports whether client may make one more request.
func (l *limiter) allow(client netip.Addr) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n <= 0 || !client.IsValid() {
		return true
	}
	key := client.Unmap()
	if key.Is6() {
		key = netip.PrefixFrom(key, 64).Masked().Addr()
	}
	now := l.now()
	b, ok := l.clients[key]
	if !ok {
		if len(l.clients) >= maxLimited {
			l.sweep(now)
		}
		b = &clientBucket{lim: rate.NewLimiter(rate.Every(l.per/time.Duration(l.n)), l.n)}
		l.clients[key] = b
	}
	b.seen = now
	return b.lim.AllowN(now, 1)
}

// sweep forgets idle clients, or everyone when nobody is idle.
func (l *limiter) sweep(now time.Time) {
	for k, b := range l.clients {
		if now.Sub(b.seen) > l.per {
			delete(l.clients, k)
		}
	}
	if len(l.clients) >= maxLimited {
		l.clients = map[netip.Addr]*clientBucket{}
	}
}
