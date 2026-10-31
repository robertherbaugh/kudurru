package main

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
)

type querylogRecords struct {
	mu   sync.Mutex
	rows []map[string]any
}

func (*querylogRecords) Enabled(context.Context, slog.Level) bool { return true }
func (h *querylogRecords) Handle(_ context.Context, rec slog.Record) error {
	if rec.Message != "query" {
		return nil
	}
	row := map[string]any{}
	rec.Attrs(func(a slog.Attr) bool { row[a.Key] = a.Value.Any(); return true })
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rows = append(h.rows, row)
	return nil
}
func (h *querylogRecords) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *querylogRecords) WithGroup(string) slog.Handler      { return h }

func TestQuerylogRuntimeLazyOwnersFailedPrepareAndOriginalSink(t *testing.T) {
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Node.DataDir = t.TempDir()
	cfg.Querylog.Enabled = false
	records := &querylogRecords{}
	r, err := newQuerylogRuntime(t.Context(), cfg, prometheus.NewRegistry(), slog.New(records), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.close(t.Context()) })
	old := r.current.Load()
	path := filepath.Join(cfg.QuerylogDataDir(), sqlitelog.FileName)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("disabled startup created a database", err)
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	next := cfg.Querylog
	next.Enabled = true
	if _, err := r.prepare(t.Context(), next, old); err == nil {
		t.Fatal("unusable SQLite destination prepared")
	}
	if r.current.Load() != old || old.Enabled() {
		t.Fatal("failed preparation adopted a route")
	}
	if ok, _ := r.ready(); ok {
		t.Fatal("failed preparation reported available")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	ready, err := r.prepare(t.Context(), next, old)
	if err != nil {
		t.Fatal(err)
	}
	r.adopt(ready, next)
	writer := r.writer.Load()
	if writer == nil || r.reader() == nil {
		t.Fatal("lazy SQLite reader absent")
	}
	slogCfg := next
	slogCfg.Sink = "slog"
	slogCfg.Privacy = "hide_domains"
	logRoute, err := r.prepare(t.Context(), slogCfg, ready)
	if err != nil {
		t.Fatal(err)
	}
	r.adopt(logRoute, slogCfg)
	off := slogCfg
	off.Enabled = false
	offRoute, err := r.prepare(t.Context(), off, logRoute)
	if err != nil {
		t.Fatal(err)
	}
	r.adopt(offRoute, off)
	emit := func(route *querylog.Captured, name string) bool {
		return route.Enqueue(&querylog.Event{Time: time.Now(), Name: name, Client: netip.MustParseAddr("192.0.2.8"), QType: 1, QClass: 1})
	}
	if !emit(ready, "original-sqlite.example.") || !emit(logRoute, "original-slog.example.") || emit(offRoute, "disabled.example.") || emit(old, "original-disabled.example.") {
		t.Fatal("captured decision changed after replacement")
	}
	for range 20 {
		if err := r.reload(t.Context(), next); err != nil {
			t.Fatal(err)
		}
		if err := r.reload(t.Context(), off); err != nil {
			t.Fatal(err)
		}
		if r.writer.Load() != writer {
			t.Fatal("toggles replaced the bounded writer owner")
		}
	}
	if err := r.async.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	records.mu.Lock()
	if len(records.rows) != 1 || records.rows[0]["name"] != querylog.HiddenName {
		t.Error("original slog destination/privacy lost", len(records.rows))
	}
	records.mu.Unlock()
	page, err := r.reader().Queries(t.Context(), sqlitelog.QueryFilter{Limit: 20})
	if err != nil || len(page.Queries) != 1 {
		t.Fatal("local tail/history lost after disable", err)
	}
	if n, err := r.Purge(t.Context(), time.Time{}); err != nil || n != 1 {
		t.Fatal("inactive local writer could not purge exact stored fact", n, err)
	}
	if len(r.tail.Recent(0, 0)) != 2 {
		t.Fatal("purge destroyed the separately retained local tail")
	}
	if err := r.close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestQuerylogPreparationCannotAdoptRetentionOrResetExistingIDs(t *testing.T) {
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Node.DataDir = t.TempDir()
	cfg.Querylog.Enabled = false
	cfg.Querylog.Retention.Raw = config.Duration(24 * time.Hour)
	opts, err := sqlitelog.OptionsFrom(cfg.Querylog, cfg.QuerylogDataDir(), cfg.Node.Name)
	if err != nil {
		t.Fatal(err)
	}
	w, err := sqlitelog.Open(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write([]*querylog.Event{{ID: 40, Time: time.Now().Add(-2 * time.Hour), Name: "retained.example.", Client: netip.MustParseAddr("192.0.2.8"), QType: 1, QClass: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := newQuerylogRuntime(t.Context(), cfg, prometheus.NewRegistry(), slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.close(t.Context()) }()
	candidate := cfg.Querylog
	candidate.Enabled = true
	candidate.Sink = "slog"
	candidate.Retention.Raw = config.Duration(time.Minute)
	route, err := r.prepare(t.Context(), candidate, r.current.Load())
	if err != nil {
		t.Fatal(err)
	}
	// Opening existing history for a prospective slog route must neither adopt
	// the shortened retention nor move the disabled operational route.
	status, err := r.reader().Status(t.Context())
	if err != nil || status.Retention.RawSeconds != 24*60*60 || r.current.Load().Enabled() {
		t.Fatal("unpublished candidate adopted retention/admission", err)
	}
	candidate.Retention.Raw = cfg.Querylog.Retention.Raw
	r.adopt(route, candidate)
	if !route.Enqueue(&querylog.Event{Time: time.Now(), Name: "new-slog.example.", QType: 1, QClass: 1}) {
		t.Fatal("accepted slog event refused")
	}
	candidate.Sink = "sqlite"
	if err := r.reload(t.Context(), candidate); err != nil {
		t.Fatal(err)
	}
	if !r.current.Load().Enqueue(&querylog.Event{Time: time.Now(), Name: "new-sqlite.example.", QType: 1, QClass: 1}) {
		t.Fatal("accepted SQLite event refused")
	}
	if err := r.async.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	page, err := r.reader().Queries(t.Context(), sqlitelog.QueryFilter{Limit: 20})
	if err != nil || len(page.Queries) != 2 || page.Queries[0].ID != 42 || page.Queries[1].ID != 40 {
		t.Fatal("late SQLite switch collided with existing history IDs", err)
	}
	tail := r.tail.Recent(0, 0)
	if len(tail) != 2 || tail[0].ID != 41 || tail[1].ID != 42 {
		t.Fatal("tail lost original existing-history ID floor")
	}
}
