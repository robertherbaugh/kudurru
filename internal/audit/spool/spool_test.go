package spool

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/audit/audittest"
)

func open(t *testing.T, o Options) *Spool {
	t.Helper()
	if o.Path == "" {
		o.Path = filepath.Join(t.TempDir(), FileName)
	}
	if o.Node == "" {
		o.Node = "node-a"
	}
	s, err := Open(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestConformance(t *testing.T) {
	audittest.Run(t, func(t *testing.T) audittest.Store { return open(t, Options{}) })
}

func record(t *testing.T, s *Spool, n int, at time.Time) {
	t.Helper()
	for i := range n {
		if err := s.Record(context.Background(), audit.Event{Time: at.Add(time.Duration(i) * time.Second), Action: "dnszone.update",
			Resource: audit.Resource{Kind: "DNSZone", Name: "example"}, Result: audit.ResultSuccess}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestChainAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	s, err := Open(context.Background(), Options{Path: path, Node: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	record(t, s, 3, now)
	id := s.SourceID()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopened: same source id, and the chain continues from the stored head.
	s = open(t, Options{Path: path})
	if s.SourceID() != id || id == "" {
		t.Fatalf("source id %q, want %q", s.SourceID(), id)
	}
	record(t, s, 2, now.Add(time.Minute))
	if err := s.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	evs, err := s.ReadAfter(context.Background(), 0, 10)
	if err != nil || len(evs) != 5 {
		t.Fatalf("read %d events: %v", len(evs), err)
	}
	if evs[0].Node != "node-a" || evs[3].PrevHash != evs[2].Hash || evs[0].Time.Nanosecond()%1000 != 0 {
		t.Fatalf("events %+v", evs)
	}
	// Tampering with a stored document is detected.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`UPDATE event SET doc = replace(doc, 'dnszone.update', 'dnszone.delete') WHERE id = 2`); err != nil {
		t.Fatal(err)
	}
	if err := s.Verify(context.Background()); err == nil || !strings.Contains(err.Error(), "event 2") {
		t.Fatalf("tampered spool verified: %v", err)
	}
}

func TestCapAndPrune(t *testing.T) {
	reg := prometheus.NewRegistry()
	s := open(t, Options{Max: 10, Registerer: reg})
	old := time.Now().Add(-48 * time.Hour)
	record(t, s, 15, old)
	evs, err := s.Read(context.Background(), audit.Query{Limit: 100})
	if err != nil || len(evs) != 10 || evs[0].ID != 6 {
		t.Fatalf("after the cap: %d events from %d: %v", len(evs), evs[0].ID, err)
	}
	if got := testutil.ToFloat64(s.dropped); got != 5 {
		t.Fatalf("dropped %v, want 5", got)
	}
	if err := s.Verify(context.Background()); err != nil {
		t.Fatalf("chain after dropping the oldest: %v", err)
	}
	// Two more events push two more old ones out; the prune takes the rest.
	record(t, s, 2, time.Now())
	n, err := s.Prune(context.Background(), time.Now().Add(-time.Hour))
	if err != nil || n != 8 {
		t.Fatalf("pruned %d: %v", n, err)
	}
	if evs, _ = s.Read(context.Background(), audit.Query{}); len(evs) != 2 {
		t.Fatalf("%d events after prune", len(evs))
	}
}

func TestSchemaTooNew(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	s := open(t, Options{Path: path})
	if _, err := s.db.Exec(`UPDATE meta SET value = '99' WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), Options{Path: path}); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("open newer schema: %v", err)
	}
}
