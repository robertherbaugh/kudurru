package sqlite

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func TestResultOf(t *testing.T) {
	for _, tt := range []struct {
		err  error
		want string
	}{
		{nil, "ok"},
		{fmt.Errorf("wrapped: %w", store.ErrNotFound), "not_found"},
		{store.ErrConflict, "conflict"},
		{&store.ErrManagedBy{Owner: "api"}, "managed_by"},
		{store.ErrClosed, "closed"},
		{context.Canceled, "canceled"},
		{context.DeadlineExceeded, "canceled"},
		{errors.New("disk I/O error"), "error"},
	} {
		if got := resultOf(tt.err); got != tt.want {
			t.Errorf("resultOf(%v) = %q, want %q", tt.err, got, tt.want)
		}
	}
}

func assertFencedMetric(t *testing.T, m *Metrics, want float64) {
	t.Helper()
	if got := testutil.ToFloat64(m.ClusterFenced); got != want {
		t.Fatalf("kudurru_cluster_fenced = %v, want %v", got, want)
	}
}

func TestClusterFencedMetricsTransitions(t *testing.T) {
	ctx := context.Background()
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	p := openTemp(t)
	s := openTemp(t, func(o *Options) { o.InitialMode = replication.Follower; o.Metrics = m })
	assertFencedMetric(t, m, 0) // Unenrolled is not an authority fence.
	if err := s.Enroll(ctx, p.Authority().Term); err != nil {
		t.Fatal(err)
	}
	first := imageOf(t, p)
	install(t, s, first)
	assertFencedMetric(t, m, 0)
	if _, err := s.Promote(ctx, replication.PromoteRequest{ExpectedTerm: first.Term, ExpectedInstalled: first.Generation(), RequestID: "metrics-promotion"}); err != nil {
		t.Fatal(err)
	}
	assertFencedMetric(t, m, 0)
	winner := *first
	winner.Term.Epoch = s.Authority().Term.Epoch + 1
	winner.Term.PrimaryID, winner.Term.PromotionID = "winner", "winning-promotion"
	winner.Parent = nil
	if err := replication.Seal(&winner); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ObserveAuthority(ctx, winner.Term); err != nil {
		t.Fatal(err)
	}
	assertFencedMetric(t, m, 1)
	if err := testutil.GatherAndCompare(reg, bytes.NewBufferString(`# HELP kudurru_cluster_fenced Whether the SQLite member is fenced by replication authority or a failed durable fence (1 fenced, 0 otherwise); not DNS or write readiness.
# TYPE kudurru_cluster_fenced gauge
kudurru_cluster_fenced 1
`), "kudurru_cluster_fenced"); err != nil {
		t.Fatal(err)
	}
	install(t, s, &winner)
	assertFencedMetric(t, m, 0)
	if s.Authority().Writable() {
		t.Fatal("following the new primary must remain read only")
	}
}

func TestStoreMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	// serve registers zonecache's kudurru_store_watch_resyncs_total{kind}
	// on the same registry; the driver's metrics must not collide with it.
	reg.MustRegister(prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kudurru_store_watch_resyncs_total", Help: "zonecache"}, []string{"kind"}))
	m := NewMetrics(reg)
	s := openTemp(t, func(o *Options) { o.Metrics = m })
	ctx := context.Background()
	ops := func(op, result string) float64 { return testutil.ToFloat64(m.Operations.WithLabelValues(op, result)) }

	if _, err := s.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: "file:x.yaml"}); !store.IsManagedBy(err) {
		t.Fatalf("want ErrManagedBy, got %v", err)
	}
	if _, err := s.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI, ExpectedResourceVersion: "999"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
	bad := zone("bad", "not a zone name")
	if _, err := s.Apply(ctx, bad, store.ApplyOptions{Manager: store.ManagerAPI}); err == nil {
		t.Fatal("an invalid zone must be refused")
	}
	var z = zone("", "")
	if err := s.Get(ctx, store.Key{Kind: "DNSZone", Name: "home"}, z); err != nil {
		t.Fatal(err)
	}
	if err := s.Get(ctx, store.Key{Kind: "DNSZone", Name: "nope"}, z); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.List(ctx, "DNSZone", store.ListOptions{}); err != nil {
		t.Fatal(err)
	}
	z.Status.Serial = 7
	if err := s.UpdateStatus(ctx, z); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := s.Snapshot(ctx, &buf, store.SnapshotOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, store.Key{Kind: "DNSZone", Name: "home"}, store.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		op, result string
		want       float64
	}{
		{opApply, "ok", 1}, {opApply, "managed_by", 1}, {opApply, "conflict", 1}, {opApply, "invalid", 1},
		{opGet, "ok", 1}, {opGet, "not_found", 1}, {opList, "ok", 1}, {opUpdateStatus, "ok", 1},
		{opSnapshot, "ok", 1}, {opDelete, "ok", 1},
	} {
		if got := ops(c.op, c.result); got != c.want {
			t.Errorf("kudurru_store_operations_total{op=%q,result=%q} = %v, want %v", c.op, c.result, got, c.want)
		}
	}
	if n := testutil.CollectAndCount(m.Duration); n != 6 {
		t.Errorf("duration series %d, want 6 (apply, get, list, update_status, snapshot, delete)", n)
	}

	// A watcher that stops reading falls behind the broadcast and resyncs
	// from the change feed; the gauge tracks open watches.
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w, err := s.Watch(wctx, "DNSZone", store.WatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(m.Watchers); got != 1 {
		t.Fatalf("kudurru_store_watchers = %v, want 1", got)
	}
	for i := range watchInBuffer + watchOutBuffer + 50 {
		name := fmt.Sprintf("z%d", i)
		if _, err := s.Apply(ctx, zone(name, name+"."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	got := 0
	deadline := time.After(30 * time.Second)
	for got < watchInBuffer+watchOutBuffer+50 {
		select {
		case ev := <-w.ResultChan():
			if ev.Type == store.Added {
				got++
			}
		case <-deadline:
			t.Fatalf("watch delivered %d events", got)
		}
	}
	if r := testutil.ToFloat64(m.Catchups.WithLabelValues("DNSZone")); r < 1 {
		t.Errorf("kudurru_store_watch_catchups_total{kind=DNSZone} = %v, want >= 1", r)
	}
	w.Stop()
	if got := testutil.ToFloat64(m.Watchers); got != 0 {
		t.Fatalf("kudurru_store_watchers after Stop = %v, want 0", got)
	}
	if ops(opWatch, "ok") != 1 {
		t.Errorf("watch calls %v", ops(opWatch, "ok"))
	}
	if n, err := testutil.GatherAndCount(reg, "kudurru_store_operations_total", "kudurru_store_operation_duration_seconds", "kudurru_store_watch_catchups_total", "kudurru_store_watchers"); err != nil || n == 0 {
		t.Fatalf("metrics not registered: %d %v", n, err)
	}
}
