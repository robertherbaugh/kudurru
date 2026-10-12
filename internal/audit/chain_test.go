package audit

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestChain(t *testing.T) {
	ts := time.Date(2026, 10, 12, 9, 0, 0, 123456789, time.UTC)
	evs := make([]Event, 3)
	prev := ""
	for i := range evs {
		evs[i] = Event{ID: int64(i + 1), Time: ts.Add(time.Duration(i) * time.Second), Node: "node-a",
			Actor: Actor{Kind: "User", Name: "alice", Groups: []string{"admins"}}, Action: "dnszone.update",
			Resource: Resource{Kind: "DNSZone", Name: "example"}, Result: ResultSuccess, After: json.RawMessage(`{"b":1,"a":2}`)}
		Chain(&evs[i], prev)
		prev = evs[i].Hash
	}
	if err := VerifyChain(evs); err != nil {
		t.Fatal(err)
	}
	// Time counts in microseconds: a store that keeps µs reproduces it.
	us := evs[0]
	us.Time = time.UnixMicro(us.Time.UnixMicro())
	if ChainHash(us, us.PrevHash) != evs[0].Hash {
		t.Fatal("hash depends on sub-microsecond time")
	}
	// Every hashed field matters, including the exact document bytes.
	changed := append([]Event(nil), evs...)
	changed[1].After = json.RawMessage(`{"a":2,"b":1}`)
	if err := VerifyChain(changed); err == nil || !strings.Contains(err.Error(), "event 2 was changed") {
		t.Fatalf("re-encoded document: %v", err)
	}
	changed = append([]Event(nil), evs...)
	changed[2].Actor.Groups = nil
	if err := VerifyChain(changed); err == nil {
		t.Fatal("changed groups verified")
	}
	// A removed event breaks the link.
	if err := VerifyChain([]Event{evs[0], evs[2]}); err == nil || !strings.Contains(err.Error(), "does not follow") {
		t.Fatalf("gap: %v", err)
	}
	// The first event may follow pruned ones.
	if err := VerifyChain(evs[1:]); err != nil {
		t.Fatal(err)
	}
}

func TestMultiReader(t *testing.T) {
	logOnly := Multi{Log{}}
	if _, ok := ReaderOf(logOnly); ok {
		t.Fatal("log-only chain reads")
	}
	mem := &Memory{}
	if rd, ok := ReaderOf(Multi{Log{}, Multi{nil, mem}}); !ok || rd != Reader(mem) {
		t.Fatal("nested reader not found")
	}
	if rd, ok := ReaderOf(mem); !ok || rd != Reader(mem) {
		t.Fatal("plain reader")
	}
}
