package querylog

import (
	"context"
	"net/netip"
	"testing"
	"time"
)

func TestDedupIndexClaimsOncePerAnsweredQuery(t *testing.T) {
	d := NewDedupIndex(time.Minute, 4)
	at := time.Unix(1_800_000_000, 0)
	addr := netip.MustParseAddr("::ffff:192.0.2.1")
	ev := func(port uint16, name string, offset time.Duration) *Event {
		return &Event{Time: at.Add(offset), Client: addr, ClientPort: port, Name: name, QType: 1, QClass: 1}
	}
	d.Record(ev(53, "A.example.", 0))
	d.Record(ev(53, "a.example", time.Second))
	key := NewDedupKey(netip.MustParseAddr("192.0.2.1"), 53, 1, 1, "a.example.")
	if d.Claim(key, at.Add(-time.Hour), at.Add(-time.Minute)) {
		t.Fatal("claimed outside the window")
	}
	// Claim forgets what it matched, so the same call succeeds once per
	// answered query.
	for i := range 2 {
		if !d.Claim(key, at.Add(-time.Second), at.Add(2*time.Second)) {
			t.Fatalf("message %d: two answered queries must cancel two messages", i+1)
		}
	}
	if d.Claim(key, at.Add(-time.Second), at.Add(2*time.Second)) {
		t.Fatal("a third message was claimed")
	}
	if d.Claim(NewDedupKey(addr, 54, 1, 1, "a.example"), at, at) {
		t.Fatal("another port matched")
	}
	// The entry bound evicts the oldest first.
	for i := range 6 {
		d.Record(ev(uint16(100+i), "b.example", 0))
	}
	if d.Len() != 4 {
		t.Fatalf("len %d", d.Len())
	}
	if d.Claim(NewDedupKey(addr, 100, 1, 1, "b.example"), at, at) || !d.Claim(NewDedupKey(addr, 105, 1, 1, "b.example"), at, at) {
		t.Fatal("eviction order")
	}
	// Retention forgets old queries as new ones arrive.
	d.Record(ev(200, "c.example", 2*time.Minute))
	if d.Len() != 1 {
		t.Fatalf("len %d after retention", d.Len())
	}
}

// TestAsyncRecordsNativeQueriesBeforeThePrivacyTransform: the collector
// indexes the client address as received, never an ingested event, and
// clears the client port before any consumer sees the event.
func TestAsyncRecordsNativeQueriesBeforeThePrivacyTransform(t *testing.T) {
	sink := &recSink{}
	p, err := NewPrivacy(PrivacyOptions{Mode: PrivacyAnonymizeClient})
	if err != nil {
		t.Fatal(err)
	}
	a := NewAsyncWith(AsyncOptions{Sink: sink, BatchSize: 1, Interval: time.Millisecond, Privacy: p})
	d := NewDedupIndex(0, 0)
	a.SetDedup(d)
	client := netip.MustParseAddr("192.0.2.77")
	now := time.Now()
	a.Enqueue(&Event{Time: now, Client: client, ClientPort: 4444, Name: "n.example.", QType: 1, QClass: 1})
	a.Enqueue(&Event{Time: now, Client: client, ClientPort: 5555, Name: "n.example.", QType: 1, QClass: 1, Origin: "edge-1"})
	if err := a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d.Len() != 1 || !d.Claim(NewDedupKey(client, 4444, 1, 1, "n.example"), now, now) {
		t.Fatal("the native query was not indexed with its real address")
	}
	if _, n := sink.count(); n != 2 {
		t.Fatalf("%d events stored", n)
	}
	for _, b := range sink.batches {
		for _, ev := range b {
			if ev.ClientPort != 0 || ev.Client == client {
				t.Fatalf("event leaves the collector with %v port %d", ev.Client, ev.ClientPort)
			}
		}
	}
}
