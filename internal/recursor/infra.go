package recursor

import (
	"math/rand/v2"
	"net/netip"
	"slices"
	"sync"
	"time"
)

// Per-server timeouts: an unknown server gets initialRTO; a known one
// three times its smoothed RTT, within [minRTO, maxRTO].
const (
	initialRTO = 800 * time.Millisecond
	minRTO     = 150 * time.Millisecond
	maxRTO     = 2 * time.Second
	// rttBand is the window above the fastest server within which servers
	// are picked at random, spreading queries (and what each server
	// learns) without favoring slow servers.
	rttBand = 100 * time.Millisecond
	// maxInfra bounds the server table; it is cleared when full.
	maxInfra = 20000
)

// serverStat is what the resolver knows about one authoritative server.
type serverStat struct {
	srtt     time.Duration
	failures int
	until    time.Time // backoff end
	caseMiss int       // answers that did not echo the query name's case
}

// no0x20After is how many case mismatches turn 0x20 off for a server: a
// spoofer who guessed ID and port but not the case must not disable it
// with one packet.
const no0x20After = 3

// infra tracks round-trip times and failures per server address.
type infra struct {
	mu  sync.Mutex
	m   map[netip.Addr]*serverStat
	now func() time.Time
}

func newInfra(now func() time.Time) *infra {
	return &infra{m: make(map[netip.Addr]*serverStat), now: now}
}

func (i *infra) stat(a netip.Addr) *serverStat {
	s, ok := i.m[a]
	if !ok {
		if len(i.m) >= maxInfra {
			i.m = make(map[netip.Addr]*serverStat)
		}
		s = &serverStat{}
		i.m[a] = s
	}
	return s
}

// rto returns the timeout for one exchange with a.
func (i *infra) rto(a netip.Addr) time.Duration {
	i.mu.Lock()
	defer i.mu.Unlock()
	s, ok := i.m[a]
	if !ok || s.srtt == 0 {
		return initialRTO
	}
	return min(max(3*s.srtt, minRTO), maxRTO)
}

// ok records an answer after rtt.
func (i *infra) ok(a netip.Addr, rtt time.Duration) {
	i.mu.Lock()
	defer i.mu.Unlock()
	s := i.stat(a)
	if s.srtt == 0 {
		s.srtt = rtt
	} else {
		s.srtt = (7*s.srtt + 3*rtt) / 10
	}
	s.failures, s.until = 0, time.Time{}
}

// fail records a timeout or an unusable answer; repeated failures back
// the server off for up to a minute.
func (i *infra) fail(a netip.Addr, timeout bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	s := i.stat(a)
	s.failures++
	if timeout {
		s.srtt = min(max(2*s.srtt, initialRTO), maxRTO)
	}
	if s.failures >= 3 {
		backoff := min(time.Duration(1<<min(s.failures-3, 6))*time.Second, time.Minute)
		s.until = i.now().Add(backoff)
	}
}

// caseMismatch records an answer that changed the case of the query name.
func (i *infra) caseMismatch(a netip.Addr) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.stat(a).caseMiss++
}

func (i *infra) supports0x20(a netip.Addr) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	s, ok := i.m[a]
	return !ok || s.caseMiss < no0x20After
}

// order sorts candidate addresses for one query: servers out of backoff
// first, picked at random among those within rttBand of the fastest (an
// unknown server counts as fast so it gets measured), then the rest by
// RTT; servers in backoff last.
func (i *infra) order(addrs []netip.AddrPort) []netip.AddrPort {
	type cand struct {
		a    netip.AddrPort
		rtt  time.Duration
		down bool
		r    uint32
	}
	now := i.now()
	cs := make([]cand, len(addrs))
	i.mu.Lock()
	for k, a := range addrs {
		c := cand{a: a, r: rand.Uint32()} //nolint:gosec // load spreading, not security
		if s, ok := i.m[a.Addr()]; ok {
			c.rtt = s.srtt
			c.down = now.Before(s.until)
		}
		cs[k] = c
	}
	i.mu.Unlock()
	best := time.Duration(-1)
	for _, c := range cs {
		if !c.down && (best < 0 || c.rtt < best) {
			best = c.rtt
		}
	}
	slices.SortStableFunc(cs, func(x, y cand) int {
		if x.down != y.down {
			if x.down {
				return 1
			}
			return -1
		}
		xin, yin := x.rtt <= best+rttBand, y.rtt <= best+rttBand
		switch {
		case xin && yin:
			return int(int64(x.r) - int64(y.r))
		case xin:
			return -1
		case yin:
			return 1
		}
		return int(x.rtt - y.rtt)
	})
	out := make([]netip.AddrPort, len(cs))
	for k, c := range cs {
		out[k] = c.a
	}
	return out
}
