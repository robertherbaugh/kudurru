package sqlitelog

import (
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
)

// minuteAcc accumulates one UTC minute until it is flushed into the _1m
// rollups. Flushes are additive upserts, so a minute may be flushed more
// than once (late events, Flush, Close) without losing or double-counting.
type minuteAcc struct {
	base      [nBase]int64
	clients   map[int64]*[2]int64 // client id → total, blocked
	qtypes    map[uint16]int64
	upstreams map[int64]*[nUp]int64
}

func newMinuteAcc() *minuteAcc {
	return &minuteAcc{clients: map[int64]*[2]int64{}, qtypes: map[uint16]int64{}, upstreams: map[int64]*[nUp]int64{}}
}

func (m *minuteAcc) merge(o *minuteAcc) {
	for i := range m.base {
		m.base[i] += o.base[i]
	}
	for k, v := range o.clients {
		c := m.clients[k]
		if c == nil {
			c = new([2]int64)
			m.clients[k] = c
		}
		c[0] += v[0]
		c[1] += v[1]
	}
	for k, v := range o.qtypes {
		m.qtypes[k] += v
	}
	for k, v := range o.upstreams {
		u := m.upstreams[k]
		if u == nil {
			u = new([nUp]int64)
			m.upstreams[k] = u
		}
		for i := range u {
			u[i] += v[i]
		}
	}
}

// hourAcc caches configuration-bounded list/zone counts for pending hours.
// domains is used only by the bounded source-reconstruction helper; live
// readers use the transaction-maintained retained-fact domain index.
type hourAcc struct {
	domains map[int64]*[2]int64
	lists   map[int64]*[2]int64
	zones   map[int64]int64
}

func newHourAcc() *hourAcc {
	return &hourAcc{domains: map[int64]*[2]int64{}, lists: map[int64]*[2]int64{}, zones: map[int64]int64{}}
}

func addPair(m map[int64]*[2]int64, k, a, b int64) {
	p := m[k]
	if p == nil {
		p = new([2]int64)
		m[k] = p
	}
	p[0] += a
	p[1] += b
}

func (h *hourAcc) merge(o *hourAcc) {
	for k, v := range o.domains {
		addPair(h.domains, k, v[0], v[1])
	}
	for k, v := range o.lists {
		addPair(h.lists, k, v[0], v[1])
	}
	for k, v := range o.zones {
		h.zones[k] += v
	}
}

// accSet is a set of pending minutes and hours plus the hours that were
// already derived but received late events and must be derived again.
type accSet struct {
	minutes map[int64]*minuteAcc
	hours   map[int64]*hourAcc
	dirty   map[int64]struct{}
}

func newAccSet() *accSet {
	return &accSet{minutes: map[int64]*minuteAcc{}, hours: map[int64]*hourAcc{}, dirty: map[int64]struct{}{}}
}

func (s *accSet) minute(ts int64) *minuteAcc {
	m := s.minutes[ts]
	if m == nil {
		m = newMinuteAcc()
		s.minutes[ts] = m
	}
	return m
}

func (s *accSet) hour(ts int64) *hourAcc {
	h := s.hours[ts]
	if h == nil {
		h = newHourAcc()
		s.hours[ts] = h
	}
	return h
}

func (s *accSet) merge(o *accSet) {
	for k, v := range o.minutes {
		s.minute(k).merge(v)
	}
	for k, v := range o.hours {
		s.hour(k).merge(v)
	}
	for k := range o.dirty {
		s.dirty[k] = struct{}{}
	}
}

// isBlocked reports the blocked status class (16-23).
func isBlocked(s pipeline.Status) bool { return s.Class() == pipeline.ClassBlocked }

// addBase counts ev into a rollup_1m accumulator row.
func addBase(b *[nBase]int64, ev *querylog.Event) {
	b[cTotal]++
	switch ev.Status {
	case pipeline.StatusCached, pipeline.StatusCachedStale:
		b[cCached]++
	case pipeline.StatusForwarded, pipeline.StatusRetried, pipeline.StatusPrefetchRefresh:
		b[cForwarded]++
	case pipeline.StatusAuthoritative, pipeline.StatusLocalRecord, pipeline.StatusSpecialDomain:
		b[cAuthoritative]++
	case pipeline.StatusDropped:
		b[cDropped]++
	default:
		if isBlocked(ev.Status) {
			b[cBlocked]++
		}
	}
	switch ev.Rcode {
	case 0:
		b[cRcNoError]++
	case 3:
		b[cRcNXDomain]++
	case 2:
		b[cRcServfail]++
	case 5:
		b[cRcRefused]++
	default:
		b[cRcOther]++
	}
	switch ev.Proto {
	case pipeline.ProtoUDP:
		b[cUDP]++
	case pipeline.ProtoTCP:
		b[cTCP]++
	case pipeline.ProtoDoT:
		b[cDoT]++
	case pipeline.ProtoDoH, pipeline.ProtoDoH3:
		b[cDoH]++
	case pipeline.ProtoDoQ:
		b[cDoQ]++
	}
	if answered(ev) {
		us := ev.Latency.Microseconds()
		b[cLatSum] += us
		b[cLatCnt]++
		b[cLatB0+latBucket(us)]++
	}
}

// addUpstream counts ev into a rollup_upstream_1m accumulator row.
func addUpstream(u *[nUp]int64, ev *querylog.Event) {
	u[uTotal]++
	switch ev.Status {
	case pipeline.StatusServfailUpstream:
		u[uErrors]++
	case pipeline.StatusTimeoutUpstream:
		u[uTimeouts]++
	}
	if answered(ev) {
		us := ev.Latency.Microseconds()
		u[uLatSum] += us
		u[uLatCnt]++
		u[uLatB0+latBucket(us)]++
	}
}

// answered reports whether the client got a response (latency_us is NULL
// otherwise).
func answered(ev *querylog.Event) bool {
	return ev.Status != pipeline.StatusDropped && ev.ReplyType != querylog.ReplyNone
}
