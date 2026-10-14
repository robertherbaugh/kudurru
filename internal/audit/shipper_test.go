package audit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// memSource is a Source over a slice.
type memSource struct {
	mu  sync.Mutex
	evs []Event
}

func (m *memSource) SourceID() string { return "src-1" }
func (m *memSource) Node() string     { return "node-a" }
func (m *memSource) add(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for range n {
		m.evs = append(m.evs, Event{ID: int64(len(m.evs) + 1), Time: time.Now().Add(-time.Hour), Action: "x.y"})
	}
}
func (m *memSource) ReadAfter(_ context.Context, after int64, n int) ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Event
	for _, ev := range m.evs {
		if ev.ID > after && len(out) < n {
			out = append(out, ev)
		}
	}
	return out, nil
}

// flakyTarget fails its first appends, storing half of the batch on the
// second failure, like a target that died mid-write.
type flakyTarget struct {
	mu     sync.Mutex
	stored []Event
	fails  int
	calls  int
}

func (f *flakyTarget) Cursor(context.Context, Source) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.stored) == 0 {
		return 0, nil
	}
	return f.stored[len(f.stored)-1].ID, nil
}

func (f *flakyTarget) Append(_ context.Context, _ Source, evs []Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= f.fails {
		if f.calls == f.fails {
			f.stored = append(f.stored, evs[:len(evs)/2]...)
		}
		return errors.New("target down")
	}
	f.stored = append(f.stored, evs...)
	return nil
}

func (f *flakyTarget) ids() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []int64
	for _, ev := range f.stored {
		out = append(out, ev.ID)
	}
	return out
}

func TestShipperDeliversInOrderOnce(t *testing.T) {
	src := &memSource{}
	src.add(25)
	tgt := &flakyTarget{fails: 2}
	sh := &Shipper{Source: src, Target: tgt, Batch: 10, Interval: time.Millisecond, Delay: -1, MinGap: -1, MaxBackoff: time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = sh.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(tgt.ids()) < 25 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	src.add(3)
	for len(tgt.ids()) < 28 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	ids := tgt.ids()
	if len(ids) != 28 {
		t.Fatalf("shipped %d events: %v", len(ids), ids)
	}
	for i, id := range ids {
		if id != int64(i+1) {
			t.Fatalf("event %d has id %d: %v", i, id, ids)
		}
	}
	if pending, err := sh.Pending(context.Background(), 10); err != nil || len(pending) != 0 {
		t.Fatalf("pending after shipping: %v %v", pending, err)
	}
}

func TestShipperBatchesAndPending(t *testing.T) {
	src := &memSource{}
	tgt := &flakyTarget{}
	now := time.Now()
	sh := &Shipper{Source: src, Target: tgt, Interval: time.Millisecond, Delay: time.Minute, MinGap: time.Minute,
		Now: func() time.Time { return now }}
	if p, _ := sh.Pending(context.Background(), 10); p != nil {
		t.Fatal("pending before the cursor is known")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = sh.Run(ctx); close(done) }()
	// Fresh events wait for the batch to fill (their time is now).
	src.mu.Lock()
	src.evs = append(src.evs, Event{ID: 1, Time: now}, Event{ID: 2, Time: now})
	src.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	var pending []Event
	for len(pending) != 2 && time.Now().Before(deadline) {
		pending, _ = sh.Pending(context.Background(), 10)
		time.Sleep(time.Millisecond)
	}
	if len(pending) != 2 || len(tgt.ids()) != 0 {
		t.Fatalf("pending %d shipped %d", len(pending), len(tgt.ids()))
	}
	// Stopping ships what waits.
	cancel()
	<-done
	if got := tgt.ids(); len(got) != 2 {
		t.Fatalf("drained %v", got)
	}
}
