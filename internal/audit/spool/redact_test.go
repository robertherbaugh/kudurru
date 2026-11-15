package spool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/audit"
)

func TestNewEventsRedactedBeforePersistenceAndHash(t *testing.T) {
	s := open(t, Options{})
	kinds := append(runtimekinds.Private(), "DNSRecordSet")
	for _, kind := range kinds {
		raw := json.RawMessage(`{"metadata":{"annotations":{"opaque":"fixture-credential"}},"spec":{"passwordHash":"fixture-credential","data":"fixture-credential","owner":{"kind":"User","name":"alice"},"description":"safe label"}}`)
		if kind == "DNSRecordSet" {
			raw = json.RawMessage(`{"password":"fixture-credential","safe":"record evidence"}`)
		}
		if err := s.Record(context.Background(), audit.Event{Action: "fixture.update", Resource: audit.Resource{Kind: kind}, Before: raw, After: raw}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.db.Query(`SELECT doc FROM event`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	count := 0
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(raw, "fixture-credential") {
			t.Fatal("raw spool persisted credential")
		}
		var ev audit.Event
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			t.Fatal("invalid stored event")
		}
		if ev.Hash != audit.ChainHash(ev, ev.PrevHash) {
			t.Fatal("redaction occurred after hashing")
		}
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if count != len(kinds) {
		t.Fatal("private manifests not fully exercised")
	}
	if err := s.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
}
