package sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// collectUntilBookmark reads events up to and including the next Bookmark.
func collectUntilBookmark(t *testing.T, w store.Watcher) (evs []store.Event, bookmark store.Event) {
	t.Helper()
	for {
		ev := next(t, w)
		if ev.Type == store.Bookmark {
			return evs, ev
		}
		evs = append(evs, ev)
	}
}

func describe(evs []store.Event) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, string(e.Type)+" "+e.Object.GetName())
	}
	return out
}

func TestWatchResumeFromResourceVersion(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	mustApply(t, s, zone("a", "a."), store.ManagerAPI)
	mustApply(t, s, zone("b", "b."), store.ManagerAPI)
	mustApply(t, s, recordSet("apps", "other-kind", nil), store.ManagerAPI)
	from := s.ResourceVersion()

	changed := zone("a", "a.")
	changed.Spec.DefaultTTL = 60
	mustApply(t, s, changed, store.ManagerAPI)
	if err := s.Delete(ctx, store.Key{Kind: "DNSZone", Name: "b"}, store.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	mustApply(t, s, zone("c", "c."), store.ManagerAPI)
	// d is created and then modified after `from`: one Added carrying the
	// latest state.
	mustApply(t, s, zone("d", "d."), store.ManagerAPI)
	d := zone("d", "d.")
	d.Spec.DefaultTTL = 120
	mustApply(t, s, d, store.ManagerAPI)
	head := s.ResourceVersion()

	w, err := s.Watch(ctx, "DNSZone", store.WatchOptions{ResourceVersion: from})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	evs, bm := collectUntilBookmark(t, w)
	got := fmt.Sprint(describe(evs))
	if want := "[MODIFIED a DELETED b ADDED c ADDED d]"; got != want {
		t.Fatalf("replay = %s, want %s", got, want)
	}
	if evs[3].Object.(*v1alpha1.DNSZone).Spec.DefaultTTL != 120 {
		t.Fatal("coalesced Added must carry the latest state")
	}
	if evs[1].Object.(*v1alpha1.DNSZone).Spec.ZoneName != "b." {
		t.Fatal("Deleted must carry the final object")
	}
	if bm.Object.GetResourceVersion() != head {
		t.Fatalf("bookmark at %s, want %s", bm.Object.GetResourceVersion(), head)
	}
	mustApply(t, s, zone("e", "e."), store.ManagerAPI)
	if ev := next(t, w); ev.Type != store.Added || ev.Object.GetName() != "e" {
		t.Fatalf("live event after replay: %v %s", ev.Type, ev.Object.GetName())
	}

	// Resuming at the head replays nothing.
	w2, err := s.Watch(ctx, "DNSZone", store.WatchOptions{ResourceVersion: s.ResourceVersion()})
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Stop()
	if evs, _ := collectUntilBookmark(t, w2); len(evs) != 0 {
		t.Fatalf("resume at head replayed %v", describe(evs))
	}
	if _, err := s.Watch(ctx, "DNSZone", store.WatchOptions{ResourceVersion: "x"}); err == nil {
		t.Fatal("a malformed resourceVersion must be rejected")
	}
	if _, err := s.Watch(ctx, "DNSZone", store.WatchOptions{ResourceVersion: "1000000"}); !errors.Is(err, ErrResourceVersionTooNew) {
		t.Fatalf("a resourceVersion the store never issued must be ErrResourceVersionTooNew, got %v", err)
	}
}

// TestWatchResumeAcrossReopen resumes a watch on a reopened state.db: the
// change feed is persistent, so a consumer that was down while the process
// restarted replays exactly what it missed.
func TestWatchResumeAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)
	s, err := Open(path, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	mustApply(t, s, zone("a", "a."), store.ManagerAPI)
	from := s.ResourceVersion()
	mustApply(t, s, zone("b", "b."), store.ManagerAPI)
	if err := s.Delete(ctx, store.Key{Kind: "DNSZone", Name: "a"}, store.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = openAt(t, path)
	w, err := s.Watch(ctx, "DNSZone", store.WatchOptions{ResourceVersion: from})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	evs, bm := collectUntilBookmark(t, w)
	if got := fmt.Sprint(describe(evs)); got != "[ADDED b DELETED a]" {
		t.Fatalf("replay after reopen = %s", got)
	}
	if bm.Object.GetResourceVersion() != s.ResourceVersion() {
		t.Fatalf("bookmark at %s, want %s", bm.Object.GetResourceVersion(), s.ResourceVersion())
	}
}

func TestWatchNamespaceFilter(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	mustApply(t, s, recordSet("apps", "a", nil), store.ManagerAPI)
	mustApply(t, s, recordSet("dns", "b", nil), store.ManagerAPI)
	w, err := s.Watch(ctx, "DNSRecordSet", store.WatchOptions{Namespace: "dns"})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	evs, _ := collectUntilBookmark(t, w)
	if fmt.Sprint(describe(evs)) != "[ADDED b]" {
		t.Fatalf("initial = %v", describe(evs))
	}
	mustApply(t, s, recordSet("apps", "c", nil), store.ManagerAPI)
	mustApply(t, s, recordSet("dns", "d", nil), store.ManagerAPI)
	if ev := next(t, w); ev.Object.GetName() != "d" {
		t.Fatalf("namespace filter leaked %s", ev.Object.GetName())
	}
}

// TestWatchSlowConsumerCatchesUp leaves a watcher unread while far more
// changes than its buffers hold are committed: writers must not block and
// the watcher must deliver every change in order afterwards.
func TestWatchSlowConsumerCatchesUp(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	w, err := s.Watch(ctx, "DNSRecordSet", store.WatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	if ev := next(t, w); ev.Type != store.Bookmark {
		t.Fatalf("got %v", ev.Type)
	}
	const n = watchInBuffer + watchOutBuffer + 500
	for i := 0; i < n; i++ {
		mustApply(t, s, recordSet("apps", fmt.Sprintf("r%04d", i), nil), store.ManagerAPI)
	}
	var last uint64
	for i := 0; i < n; i++ {
		ev := next(t, w)
		if ev.Type != store.Added || ev.Object.GetName() != fmt.Sprintf("r%04d", i) {
			t.Fatalf("event %d = %v %s", i, ev.Type, ev.Object.GetName())
		}
		rv, _ := strconv.ParseUint(ev.Object.GetResourceVersion(), 10, 64)
		if rv <= last {
			t.Fatalf("resourceVersions out of order: %d after %d", rv, last)
		}
		last = rv
	}
	mustApply(t, s, recordSet("apps", "tail", nil), store.ManagerAPI)
	if ev := next(t, w); ev.Object.GetName() != "tail" {
		t.Fatalf("after catch-up: %s", ev.Object.GetName())
	}
}

// TestWatchRelistsAfterCompaction resumes below the compaction horizon and
// lets a lagging watcher fall behind it: the first relists as Added, the
// second also learns about objects deleted while it was behind.
func TestWatchRelistsAfterCompaction(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t, func(o *Options) { o.FeedRetention = 10 })
	s.compactEvery = 5

	w, err := s.Watch(ctx, "DNSRecordSet", store.WatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	if ev := next(t, w); ev.Type != store.Bookmark {
		t.Fatalf("got %v", ev.Type)
	}
	mustApply(t, s, recordSet("apps", "doomed", nil), store.ManagerAPI)
	if ev := next(t, w); ev.Object.GetName() != "doomed" {
		t.Fatal("expected doomed")
	}
	// Fill the watcher's buffers so it is unsubscribed, then delete doomed
	// and write enough to compact the feed past the watcher's position.
	for i := 0; i < watchInBuffer+watchOutBuffer+50; i++ {
		mustApply(t, s, recordSet("apps", "keep", map[string]string{"i": strconv.Itoa(i)}), store.ManagerAPI)
	}
	if err := s.Delete(ctx, store.Key{Kind: "DNSRecordSet", Namespace: "apps", Name: "doomed"}, store.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2*watchInBuffer; i++ {
		mustApply(t, s, recordSet("apps", fmt.Sprintf("x%04d", i), nil), store.ManagerAPI)
	}
	if s.compacted.Load() == 0 {
		t.Fatal("feed was not compacted")
	}
	// Drain: the consumer's view must converge on the store's contents.
	view := map[string]bool{"doomed": true}
	sawDeleted := false
	for {
		ev := next(t, w)
		switch ev.Type {
		case store.Added, store.Modified:
			view[ev.Object.GetName()] = true
		case store.Deleted:
			delete(view, ev.Object.GetName())
			if ev.Object.GetName() == "doomed" {
				sawDeleted = true
			}
		}
		if ev.Type != store.Bookmark && ev.Object.GetName() == fmt.Sprintf("x%04d", 2*watchInBuffer-1) {
			break
		}
	}
	if !sawDeleted || view["doomed"] {
		t.Fatal("the lagging watcher never learned that doomed was deleted")
	}
	list, err := s.List(ctx, "DNSRecordSet", store.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != len(view) {
		t.Fatalf("consumer view has %d objects, store has %d", len(view), len(list.Items))
	}

	// A caller resuming below the horizon gets a relist.
	w2, err := s.Watch(ctx, "DNSRecordSet", store.WatchOptions{ResourceVersion: "1"})
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Stop()
	evs, _ := collectUntilBookmark(t, w2)
	if len(evs) != len(list.Items) || evs[0].Type != store.Added {
		t.Fatalf("relist delivered %d events, want %d Added", len(evs), len(list.Items))
	}
}

func TestWatchStopAndClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := openTemp(t)
	w1, err := s.Watch(ctx, "DNSZone", store.WatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	w2, err := s.Watch(context.Background(), "DNSZone", store.WatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	w3, err := s.Watch(context.Background(), "DNSZone", store.WatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	drainClosed := func(w store.Watcher) {
		t.Helper()
		for range w.ResultChan() { //nolint:revive // draining until closed
		}
	}
	cancel()
	drainClosed(w1)
	w2.Stop()
	w2.Stop()
	drainClosed(w2)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	drainClosed(w3)
}
