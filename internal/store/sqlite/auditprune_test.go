package sqlite

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store"
)

func TestAuditPrunerRuns(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	s := openTemp(t, func(o *Options) { o.Now = clock })
	mustApply(t, s, zone("old", "old."), store.ManagerAPI)
	mu.Lock()
	now = now.Add(DefaultAuditRetention + time.Hour)
	mu.Unlock()
	mustApply(t, s, zone("new", "new."), store.ManagerAPI)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	p := &AuditPruner{Store: s, Interval: 10 * time.Millisecond, Now: clock}
	go func() { done <- p.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, err := s.ReadAudit(context.Background(), AuditQuery{})
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) == 1 && entries[0].Key.Name == "new" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not pruned: %+v", entries)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyAudit(context.Background()); err != nil {
		t.Fatal(err)
	}
}
