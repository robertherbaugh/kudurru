package querylog

import (
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Bounds of the dedup index.
const (
	// DefaultDedupRetention is how long the index remembers a query this
	// node answered: longer than a dnstap message is held before it is
	// checked plus the time it takes to arrive.
	DefaultDedupRetention = 30 * time.Second
	// DefaultDedupEntries bounds the queries remembered; the oldest go
	// first.
	DefaultDedupEntries = 1 << 16
	// maxDedupPerKey bounds the times remembered for one key.
	maxDedupPerKey = 8
)

// DedupKey identifies a client query across this node's log and a dnstap
// message about it: the client address and source port as the front end
// passed them on (PROXY protocol), the question name, type and class.
// The DNS message ID is not part of it, because a front end such as dnsdist
// gives every query it forwards an ID of its own.
type DedupKey struct {
	Client netip.Addr
	Port   uint16
	QType  uint16
	QClass uint16
	Name   string
}

// NewDedupKey normalizes the parts of a key: the unmapped address, the
// lowercase name without the root dot.
func NewDedupKey(client netip.Addr, port, qtype, qclass uint16, name string) DedupKey {
	return DedupKey{Client: client.Unmap(), Port: port, QType: qtype, QClass: qclass, Name: strings.TrimSuffix(strings.ToLower(name), ".")}
}

type dedupEntry struct {
	key DedupKey
	at  int64
}

// DedupIndex remembers the queries this node answered for a short time, so
// that a dnstap listener can recognize a message from a front end that
// describes one of them (dnsdist in front of kudurru forwards the query and
// can log it as well) and record each client query once (D372). The
// collector records into it from its own goroutine; listeners claim from
// theirs. It is bounded in time and in entries.
type DedupIndex struct {
	mu        sync.Mutex
	retention time.Duration
	limit     int
	entries   map[DedupKey][]int64
	order     []dedupEntry // FIFO of recorded entries, a ring
	head, n   int
}

// NewDedupIndex returns an index with the given retention and entry bound;
// zero means the defaults.
func NewDedupIndex(retention time.Duration, limit int) *DedupIndex {
	if retention <= 0 {
		retention = DefaultDedupRetention
	}
	if limit <= 0 {
		limit = DefaultDedupEntries
	}
	return &DedupIndex{retention: retention, limit: limit, entries: map[DedupKey][]int64{}, order: make([]dedupEntry, limit)}
}

// Record remembers the query of ev, an event this node answered.
func (d *DedupIndex) Record(ev *Event) {
	if d == nil || !ev.Client.IsValid() || ev.Name == "" {
		return
	}
	key := NewDedupKey(ev.Client, ev.ClientPort, ev.QType, ev.QClass, ev.Name)
	at := ev.Time.UnixNano()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.expire(at - int64(d.retention))
	if d.n == d.limit {
		d.evict()
	}
	times := d.entries[key]
	if len(times) == maxDedupPerKey {
		times = times[1:] // the FIFO entry of the dropped time evicts nothing
	}
	d.entries[key] = append(times, at)
	d.order[(d.head+d.n)%d.limit] = dedupEntry{key: key, at: at}
	d.n++
}

// Claim reports whether this node answered a query with key received
// between from and to, and if so forgets it: one answered query cancels
// one message.
func (d *DedupIndex) Claim(key DedupKey, from, to time.Time) bool {
	if d == nil {
		return false
	}
	lo, hi := from.UnixNano(), to.UnixNano()
	d.mu.Lock()
	defer d.mu.Unlock()
	times := d.entries[key]
	for i, at := range times {
		if at >= lo && at <= hi {
			if len(times) == 1 {
				delete(d.entries, key)
			} else {
				d.entries[key] = append(times[:i:i], times[i+1:]...)
			}
			return true
		}
	}
	return false
}

// Len returns the number of remembered queries.
func (d *DedupIndex) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, times := range d.entries {
		n += len(times)
	}
	return n
}

func (d *DedupIndex) expire(before int64) {
	for d.n > 0 && d.order[d.head].at < before {
		d.evict()
	}
}

// evict forgets the oldest recorded entry, unless it was already claimed.
func (d *DedupIndex) evict() {
	e := d.order[d.head]
	d.order[d.head] = dedupEntry{}
	d.head = (d.head + 1) % d.limit
	d.n--
	times := d.entries[e.key]
	for i, at := range times {
		if at == e.at {
			if len(times) == 1 {
				delete(d.entries, e.key)
			} else {
				d.entries[e.key] = append(times[:i:i], times[i+1:]...)
			}
			return
		}
	}
}
