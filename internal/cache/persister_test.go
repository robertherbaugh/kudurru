package cache

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// memPersister keeps dumps in memory and can be told to fail.
type memPersister struct {
	mu    sync.Mutex
	dump  []byte
	saves int
	fail  error
}

func (m *memPersister) Save(_ context.Context, c *Cache) (PersistStats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saves++
	if m.fail != nil {
		return PersistStats{}, m.fail
	}
	var buf bytes.Buffer
	n, err := c.WriteDump(&buf)
	m.dump = buf.Bytes()
	return PersistStats{Entries: n, Bytes: buf.Len()}, err
}

func (m *memPersister) Restore(_ context.Context, c *Cache) (PersistStats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, err := c.Load(bytes.NewReader(m.dump))
	return PersistStats{Entries: n, Bytes: len(m.dump)}, err
}

func (m *memPersister) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saves
}

func TestRunPersister(t *testing.T) {
	quiet := slog.New(slog.DiscardHandler)
	c := New(Options{MaxEntries: 100})
	for _, n := range []string{"a.", "b.", "c."} {
		c.Set(key(n), []byte(n), Positive, time.Hour)
	}
	var buf bytes.Buffer
	if n, err := c.WriteDump(&buf); err != nil || n != 3 {
		t.Fatalf("WriteDump: %d %v", n, err)
	}

	p := &memPersister{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunPersister(ctx, c, p, 10*time.Millisecond, time.Second, quiet) }()
	deadline := time.Now().Add(5 * time.Second)
	for p.count() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if p.count() < 2 {
		t.Fatalf("periodic saves: %d", p.count())
	}
	p.mu.Lock()
	p.fail = errors.New("disk full") // failures are logged, never fatal
	p.mu.Unlock()
	time.Sleep(30 * time.Millisecond)
	p.mu.Lock()
	p.fail = nil
	before := p.saves
	p.mu.Unlock()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// A periodic tick may still land between reading before and cancel;
	// the final save always comes on top.
	if p.count() < before+1 {
		t.Fatalf("final save: %d saves, want at least %d", p.count(), before+1)
	}
	d := New(Options{MaxEntries: 100})
	if st, err := p.Restore(context.Background(), d); err != nil || st.Entries != 3 {
		t.Fatalf("restore: %+v %v", st, err)
	}

	// interval 0: only the final save.
	p2 := &memPersister{}
	ctx2, cancel2 := context.WithCancel(context.Background())
	go func() { done <- RunPersister(ctx2, c, p2, 0, 0, quiet) }()
	time.Sleep(20 * time.Millisecond)
	cancel2()
	if err := <-done; err != nil || p2.count() != 1 {
		t.Fatalf("interval 0: %d saves, %v", p2.count(), err)
	}
}
