package pgaudit_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/central/centraltest"
	"github.com/robertherbaugh/kudurru/internal/central/pgaudit"
)

func TestIngestRedactsNewCentralChainKeepsSourceEvidence(t *testing.T) {
	db := centraltest.New(t)
	source := openSpool(t, "legacy-source")
	kinds := append(runtimekinds.Private(), "DNSRecordSet")
	events := make([]audit.Event, 0, len(kinds))
	previous := ""
	for i, kind := range kinds {
		raw := json.RawMessage(`{"spec":{"passwordHash":"fixture-credential","data":"fixture-credential","owner":{"kind":"User","name":"alice"},"description":"safe label"}}`)
		if kind == "DNSRecordSet" {
			raw = json.RawMessage(`{"password":"fixture-credential","safe":"record evidence"}`)
		}
		ev := audit.Event{ID: int64(i + 1), Time: time.Unix(1800000000+int64(i), 0), Action: "fixture.update", Result: audit.ResultSuccess, Resource: audit.Resource{Kind: kind}, Before: raw, After: bytes.Clone(raw)}
		// Model existing original source rows, whose hash and bytes must not change.
		audit.Chain(&ev, previous)
		previous = ev.Hash
		events = append(events, ev)
	}
	if err := (&pgaudit.Target{DB: db}).Append(context.Background(), source, events); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Pool()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(context.Background(), `SELECT id,before,after,origin_prev_hash,origin_hash FROM audit ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for rows.Next() {
		var id int64
		var before, after *string
		var prev, hash string
		if err := rows.Scan(&id, &before, &after, &prev, &hash); err != nil {
			t.Fatal(err)
		}
		original := events[id-1]
		if prev != original.PrevHash || hash != original.Hash || audit.ChainHash(original, original.PrevHash) != original.Hash {
			t.Fatal("ingestion altered original source evidence")
		}
		for _, raw := range []*string{before, after} {
			if raw != nil && strings.Contains(*raw, "fixture-credential") {
				t.Fatal("central raw document retained credential")
			}
		}
		if original.Resource.Kind == "DNSRecordSet" && (after == nil || !strings.Contains(*after, "record evidence")) {
			t.Fatal("central ingestion lost safe record evidence")
		}
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if count != len(kinds) {
		t.Fatal("private manifests not fully ingested")
	}
	if err := pgaudit.Verify(context.Background(), db); err != nil {
		t.Fatal(err)
	}
}
