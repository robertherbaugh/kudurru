package requestlimit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/loginlimit"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func TestRequestBudgetIndependentPoliciesExpiryAndPrivacy(t *testing.T) {
	at := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	var raw []byte
	for _, lane := range []Lane{Start, Callback, CLI} {
		for range lane.maximum() {
			var err error
			raw, err = charge(raw, lane, "192.0.2.1", at)
			if err != nil {
				t.Fatal(lane, err)
			}
		}
		_, err := charge(raw, lane, "192.0.2.1", at.Add(time.Second))
		var locked *loginlimit.Locked
		if !errors.As(err, &locked) || locked.RetryAfter != 59*time.Second {
			t.Fatal("missing exact shared lock", lane, err)
		}
	}
	if bytes.Contains(raw, []byte("192.0.2.1")) {
		t.Fatal("raw address persisted")
	}
	if _, err := charge(raw, Start, "192.0.2.2", at); err != nil {
		t.Fatal("unrelated prefix locked", err)
	}
	if _, err := charge(raw, Start, "192.0.2.1", at.Add(-time.Nanosecond)); !errors.Is(err, ErrState) {
		t.Fatal("clock rollback admitted", err)
	}
	if _, err := charge(raw, Start, "192.0.2.1", at.Add(time.Minute)); err != nil {
		t.Fatal("exact lock expiry refused", err)
	}
	if err := ValidateEntry(Section, replication.Entry{Key: Key, Data: raw}); err != nil {
		t.Fatal("source-time validation failed", err)
	}
}

func TestRequestBudgetMalformedStateAndCapacityNeverReset(t *testing.T) {
	at := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	raw, err := charge(nil, Start, "192.0.2.1", at)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"version", "salt", "lane", "count", "time", "until", "unknown", "trailing", "empty", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			d, err := decode(raw)
			if err != nil {
				t.Fatal(err)
			}
			var key string
			for k := range d.Entries {
				key = k
			}
			e := d.Entries[key]
			switch mode {
			case "version":
				d.Version = 2
			case "salt":
				d.Salt = "invalid"
			case "lane":
				e.Lane = "other"
			case "count":
				e.Used = 5
			case "time":
				e.Last = at.Add(time.Nanosecond)
			case "until":
				e.Until = at.Add(time.Hour)
			}
			d.Entries[key] = e
			bad, _ := json.Marshal(d)
			switch mode {
			case "unknown":
				bad = append([]byte(`{"unknown":1,`), bad[1:]...)
			case "trailing":
				bad = append(bad, []byte(`{}`)...)
			case "empty":
				bad = []byte{}
			case "oversized":
				bad = []byte(strings.Repeat("x", MaxBytes+1))
			}
			if _, err := charge(bad, Start, "192.0.2.2", at); err == nil {
				t.Fatal("malformed history reset")
			}
		})
	}
	// Fill the actual serialized-byte bound with live identities. The failed
	// charge must leave the entire prior document and its live usage exact.
	d, _ := decode(raw)
	low, high := 0, MaxKeys
	for low+1 < high {
		count := (low + high) / 2
		d.Entries = map[string]entry{}
		for i := range count {
			d.Entries[fmt.Sprintf("%064x", i)] = entry{Lane: Start, Used: 1, Last: at}
		}
		candidate, _ := json.Marshal(d)
		if len(candidate) <= MaxBytes {
			low = count
		} else {
			high = count
		}
	}
	d.Entries = map[string]entry{}
	for i := range low {
		d.Entries[fmt.Sprintf("%064x", i)] = entry{Lane: Start, Used: 1, Last: at}
	}
	full, _ := json.Marshal(d)
	before := bytes.Clone(full)
	if _, err := charge(full, Callback, "198.51.100.1", at); !errors.Is(err, ErrCapacity) {
		t.Fatal("live capacity evicted", err)
	}
	if !bytes.Equal(full, before) {
		t.Fatal("failed charge modified input")
	}
	if _, err := charge(full, Callback, "198.51.100.1", at.Add(window+time.Nanosecond)); err != nil {
		t.Fatal("expired history not reclaimed", err)
	}
}
