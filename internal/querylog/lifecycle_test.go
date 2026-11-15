package querylog

import (
	"context"
	"io"
	"log/slog"
	"net/netip"
	"sync"
	"testing"
	"time"
)

func TestCapturedLoggingRetainsOriginalDestinationPrivacyAndDecision(t *testing.T) {
	block := make(chan struct{})
	var release sync.Once
	unblock := func() { release.Do(func() { close(block) }) }
	t.Cleanup(unblock)
	oldSink := &blockingSink{recSink: &recSink{}, block: block}
	newSink := &recSink{}
	tail := NewTail(32, 32, nil)
	a := NewAsyncWith(AsyncOptions{Sink: oldSink, BatchSize: 1, QueueSize: 16, Tail: tail, FirstID: 1, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	t.Cleanup(func() {
		unblock()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = a.Close(ctx)
	})
	full, _ := NewPrivacy(PrivacyOptions{Mode: PrivacyFull})
	hide, _ := NewPrivacy(PrivacyOptions{Mode: PrivacyHideDomains})
	old := a.PrepareCaptured(oldSink, full, true, 1, nil)
	a.AdoptCaptured(old)
	if !old.Enqueue(ev("192.0.2.1", "old-first.example.")) {
		t.Fatal("old owner refused event")
	}
	fresh := a.PrepareCaptured(newSink, hide, true, 1, old)
	a.AdoptCaptured(fresh)
	if !old.Enqueue(ev("192.0.2.1", "old-held.example.")) || !fresh.Enqueue(ev("192.0.2.1", "new.example.")) {
		t.Fatal("reload refused bounded old/new events")
	}
	off := a.PrepareCaptured(newSink, hide, false, 1, fresh)
	a.AdoptCaptured(off)
	if off.Enqueue(ev("192.0.2.1", "disabled.example.")) || a.Enqueue(ev("192.0.2.1", "operational-disabled.example.")) {
		t.Fatal("disabled owner recorded an event")
	}
	if !old.Enqueue(ev("192.0.2.1", "old-after-disable.example.")) {
		t.Fatal("held original decision adopted successor disabled setting")
	}
	unblock()
	if err := a.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if old.Enqueue(ev("192.0.2.1", "after-close.example.")) {
		t.Fatal("closed lifetime accepted a stale captured write")
	}
	oldSink.mu.Lock()
	defer oldSink.mu.Unlock()
	if len(oldSink.batches) != 3 || oldSink.batches[1][0].Name != "old-held.example." || oldSink.batches[2][0].Name != "old-after-disable.example." {
		t.Fatal("pending records were redirected, transformed by a successor or duplicated")
	}
	newSink.mu.Lock()
	defer newSink.mu.Unlock()
	if len(newSink.batches) != 1 || newSink.batches[0][0].Name != HiddenName {
		t.Fatal("new owner lost its exact privacy or destination")
	}
	rows := tail.Recent(0, 0)
	if len(rows) != 4 {
		t.Fatal("route replacement reset or duplicated local tail")
	}
	seen := map[uint64]bool{}
	for _, row := range rows {
		if row.ID == 0 || seen[row.ID] {
			t.Fatal("tail IDs were reset or duplicated")
		}
		seen[row.ID] = true
	}
}

type cancelBatchSink struct {
	*recSink
	entered chan struct{}
	once    sync.Once
}

func (s *cancelBatchSink) WriteBatch(ctx context.Context, _ []*Event, _ Mode) error {
	s.once.Do(func() { close(s.entered) })
	<-ctx.Done()
	return ctx.Err()
}

func TestLoggingDrainCancellationJoinsWithoutReplayOrLateAdmission(t *testing.T) {
	sink := &cancelBatchSink{recSink: &recSink{}, entered: make(chan struct{})}
	a := NewAsyncWith(AsyncOptions{Sink: sink, BatchSize: 1, QueueSize: 2, FirstID: 1, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	a.Enqueue(ev("192.0.2.1", "pending.example."))
	select {
	case <-sink.entered:
	case <-time.After(time.Second):
		t.Fatal("pending write did not start")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := a.Close(ctx); err == nil {
		t.Fatal("blocked write falsely reported a drained queue")
	}
	joined, cancelJoin := context.WithTimeout(t.Context(), time.Second)
	defer cancelJoin()
	if err := a.Close(joined); err != nil {
		t.Fatal("canceled original write did not join", err)
	}
	if a.Enqueue(ev("192.0.2.1", "late.example.")) {
		t.Fatal("late event accepted after final drain")
	}
	if _, count := sink.count(); count != 0 {
		t.Fatal("canceled batch replayed through another write method")
	}
}

func TestCapturedPrivacyContinuationIgnoresRejectedPreparation(t *testing.T) {
	a := NewAsyncWith(AsyncOptions{Sink: &recSink{}})
	defer func() { _ = a.Close(t.Context()) }()
	p, _ := NewPrivacy(PrivacyOptions{Mode: PrivacyAnonymizeClient, Pseudonym: true})
	old := a.PrepareCaptured(&recSink{}, p, true, 1, nil)
	addr := netip.MustParseAddr("192.0.2.1")
	identity := old.Pseudonym(addr)
	full, _ := NewPrivacy(PrivacyOptions{Mode: PrivacyFull})
	_ = a.PrepareCaptured(&recSink{}, full, true, 1, old) // rejected candidate
	next, _ := NewPrivacy(PrivacyOptions{Mode: PrivacyAnonymizeClient, Pseudonym: true, ExcludeDomains: []string{"skip.example"}})
	accepted := a.PrepareCaptured(&recSink{}, next, false, 1, old)
	if accepted.Pseudonym(addr) != identity || old.Pseudonym(addr) != identity {
		t.Fatal("sink/disabled/exclusion reload or rejected candidate changed daily identity")
	}
	changed := a.PrepareCaptured(&recSink{}, full, true, 1, old)
	returned := a.PrepareCaptured(&recSink{}, next, true, 1, changed)
	if returned.Pseudonym(addr) == identity {
		t.Fatal("accepted privacy-mode transition reused original daily key")
	}
}
