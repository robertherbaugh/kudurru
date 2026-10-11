package blockpage

import (
	"net/netip"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// maxLimited bounds the clients tracked by the rate limiter; when full,
// clients idle for a minute are forgotten, and if none is, the table is
// reset (every client starts again with a full bucket).
const maxLimited = 16384

// limiter is the per-client token bucket of the block page: perMinute
// requests per minute with bursts of the same size. IPv6 clients are
// limited per /64 so address rotation within a network does not escape it.
type limiter struct {
	mu        sync.Mutex
	perMinute int
	clients   map[netip.Addr]*clientBucket
	now       func() time.Time
}

type clientBucket struct {
	lim  *rate.Limiter
	seen time.Time
}

func newLimiter(perMinute int, now func() time.Time) *limiter {
	return &limiter{perMinute: perMinute, clients: map[netip.Addr]*clientBucket{}, now: now}
}

// setRate changes the limit; existing buckets are dropped.
func (l *limiter) setRate(perMinute int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if perMinute != l.perMinute {
		l.perMinute = perMinute
		l.clients = map[netip.Addr]*clientBucket{}
	}
}

// allow reports whether client may make one more request.
func (l *limiter) allow(client netip.Addr) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.perMinute <= 0 || !client.IsValid() {
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
		b = &clientBucket{lim: rate.NewLimiter(rate.Every(time.Minute/time.Duration(l.perMinute)), l.perMinute)}
		l.clients[key] = b
	}
	b.seen = now
	return b.lim.AllowN(now, 1)
}

// sweep forgets idle clients, or everyone when nobody is idle.
func (l *limiter) sweep(now time.Time) {
	for k, b := range l.clients {
		if now.Sub(b.seen) > time.Minute {
			delete(l.clients, k)
		}
	}
	if len(l.clients) >= maxLimited {
		l.clients = map[netip.Addr]*clientBucket{}
	}
}
