package pgaudit

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/audit"
)

func TestHistoricalCanonicalDoesNotRedact(t *testing.T) {
	original := json.RawMessage(`{"spec":{"passwordHash":"historical-fixture-evidence"}}`)
	ev := audit.Event{Time: time.Unix(1800000000, 0).UTC(), Node: "old-node", Resource: audit.Resource{Kind: "LocalUser"}, Before: original, After: original}
	audit.Chain(&ev, "old-predecessor")
	savedHash := ev.Hash
	c := canonical(ev, "old-node")
	if !bytes.Equal(c.Before, original) || !bytes.Equal(c.After, original) || audit.ChainHash(c, ev.PrevHash) != savedHash {
		t.Fatal("historical verifier canonicalization changed hashed documents")
	}
	prepared := canonical(audit.PrepareDocuments(ev), "old-node")
	if prepared.Before != nil || prepared.After != nil {
		t.Fatal("new central ingestion did not omit private documents")
	}
	if !bytes.Equal(ev.Before, original) || ev.Hash != savedHash {
		t.Fatal("new ingestion mutated source evidence")
	}
}
