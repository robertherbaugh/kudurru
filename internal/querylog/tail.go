package querylog

import (
	"context"
	"sync"
)

// Tail defaults (docs/architecture.md §11.2: hot ring 65 536; the SSE
// design gives every subscriber a buffer of 1 000).
const (
	DefaultRingSize  = 65536
	DefaultSubBuffer = 1000
)

// Tail is the live-tail fan-out and the in-memory hot ring. The collector
// publishes every event after the privacy transform; subscribers get them
// on buffered channels. A subscriber that falls behind loses events (counted
// in kudurru_querylog_dropped_total{stage="tail"}) instead of slowing the
// collector.
type Tail struct {
	metrics   *Metrics
	subBuffer int

	mu   sync.Mutex
	subs map[chan *Event]struct{}
	ring []*Event
	next int  // next write position in ring
	full bool // ring has wrapped
}

// NewTail creates a tail with a hot ring of ringSize events (0 for the
// default, negative for none) and per-subscriber buffers of subBuffer
// events (0 for the default).
func NewTail(ringSize, subBuffer int, m *Metrics) *Tail {
	if ringSize == 0 {
		ringSize = DefaultRingSize
	}
	if ringSize < 0 {
		ringSize = 0
	}
	if subBuffer <= 0 {
		subBuffer = DefaultSubBuffer
	}
	return &Tail{metrics: m, subBuffer: subBuffer, subs: map[chan *Event]struct{}{}, ring: make([]*Event, ringSize)}
}

// Publish appends ev to the ring and offers it to every subscriber without
// blocking.
func (t *Tail) Publish(ev *Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.ring) > 0 {
		t.ring[t.next] = ev
		t.next++
		if t.next == len(t.ring) {
			t.next, t.full = 0, true
		}
	}
	for ch := range t.subs {
		select {
		case ch <- ev:
		default:
			t.metrics.Drop(DropStageTail, "", 1)
		}
	}
}

// Subscribe returns a channel of events published from now on. The channel
// is closed when ctx ends.
func (t *Tail) Subscribe(ctx context.Context) <-chan *Event {
	ch := make(chan *Event, t.subBuffer)
	t.mu.Lock()
	t.subs[ch] = struct{}{}
	t.mu.Unlock()
	go func() {
		<-ctx.Done()
		t.mu.Lock()
		delete(t.subs, ch)
		close(ch)
		t.mu.Unlock()
	}()
	return ch
}

// Subscribers returns the number of live subscriptions.
func (t *Tail) Subscribers() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.subs)
}

// Recent returns up to n of the newest events in the ring, oldest first,
// whose ID is greater than afterID (0 for no lower bound). It serves SSE
// resume (Last-Event-ID) and source=memory reads.
func (t *Tail) Recent(n int, afterID uint64) []*Event {
	t.mu.Lock()
	defer t.mu.Unlock()
	size := t.next
	if t.full {
		size = len(t.ring)
	}
	if n <= 0 || n > size {
		n = size
	}
	out := make([]*Event, 0, n)
	for i := 0; i < size && len(out) < n; i++ {
		idx := (t.next - 1 - i + len(t.ring)) % len(t.ring)
		ev := t.ring[idx]
		if afterID != 0 && ev.ID <= afterID {
			break
		}
		out = append(out, ev)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}
