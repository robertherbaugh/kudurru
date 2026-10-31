package sqlitelog

import (
	"context"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/querylog"
)

func TestCapturedBatchPrivacyOverridesSuccessorAndCancellationDoesNotReplay(t *testing.T) {
	w, _ := newWriter(t, t0, func(o *Options) { o.Privacy = querylog.PrivacyNothing })
	full := event(t0, "192.0.2.8", "original.example")
	if err := w.WriteBatch(t.Context(), []*querylog.Event{full}, querylog.PrivacyFull); err != nil {
		t.Fatal(err)
	}
	w.SetPrivacy(querylog.PrivacyFull)
	totals := event(t0.Add(time.Second), "192.0.2.8", "never-store.example")
	p, err := querylog.NewPrivacy(querylog.PrivacyOptions{Mode: querylog.PrivacyNothing})
	if err != nil {
		t.Fatal(err)
	}
	p.Apply(totals)
	if err := w.WriteBatch(t.Context(), []*querylog.Event{totals}, querylog.PrivacyNothing); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := w.WriteBatch(ctx, []*querylog.Event{event(t0, "192.0.2.8", "canceled.example")}, querylog.PrivacyFull); err == nil {
		t.Fatal("canceled batch committed")
	}
	if got := queryInt(t, w.w, `SELECT count(*) FROM query`); got != 1 {
		t.Fatal("successor changed original facts decision or canceled batch replayed", got)
	}
	if err := w.CloseContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	reopened, _ := newWriter(t, t0, func(o *Options) { o.Path = w.opts.Path })
	if n := queryInt(t, reopened.w, `SELECT sum(total) FROM rollup_1m`); n != 2 {
		t.Fatal("original fact/totals batches lost or duplicated", n)
	}
}

func TestCapturedBatchAndFinalCloseHonorDeadlineWhileOwnerBusy(t *testing.T) {
	w, _ := newWriter(t, t0)
	w.mu.Lock()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	done := make(chan error, 1)
	go func() {
		done <- w.WriteBatch(ctx, []*querylog.Event{event(t0, "192.0.2.8", "never-resume.example")}, querylog.PrivacyFull)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("busy batch ignored deadline")
		}
	case <-time.After(time.Second):
		t.Error("busy batch did not cancel")
	}
	cancel()
	ctx, cancel = context.WithTimeout(t.Context(), 20*time.Millisecond)
	go func() { done <- w.CloseContext(ctx) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("busy final close ignored deadline")
		}
	case <-time.After(time.Second):
		t.Error("busy final close did not cancel")
	}
	cancel()
	w.mu.Unlock()
	if n := queryInt(t, w.w, `SELECT count(*) FROM query`); n != 0 {
		t.Fatal("canceled waiting batch resumed", n)
	}
	if err := w.CloseContext(t.Context()); err != nil {
		t.Fatal("owner did not join after busy operation", err)
	}
}
