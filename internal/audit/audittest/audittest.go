// Package audittest is the conformance suite of audit stores: every
// recorder that reads its events back (audit.Memory, the state.db
// recorder, the node spool, the cluster-wide stores) runs Run, so the
// audit routes behave the same whichever store answers them.
package audittest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/audit"
)

// Store is a recorder that reads events back.
type Store interface {
	audit.Recorder
	audit.Reader
}

// Run records a fixed set of events into a fresh store from open and
// checks reads: order, filters, keyset pages in both directions and the
// limit. open is called once per subtest.
func Run(t *testing.T, open func(t *testing.T) Store) {
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	fixture := func(t *testing.T) Store {
		t.Helper()
		st := open(t)
		for i := range 30 {
			ev := audit.Event{
				Time:     base.Add(time.Duration(i) * time.Minute),
				Node:     "node-a",
				Actor:    audit.Actor{Kind: "User", Name: []string{"alice", "bob", "carol"}[i%3]},
				Action:   "dnszone.update",
				Resource: audit.Resource{Kind: "DNSZone", Name: fmt.Sprintf("zone-%d", i%5)},
				Result:   audit.ResultSuccess,
				Manager:  "api",
			}
			if i%10 == 9 {
				ev.Action, ev.Resource, ev.Result = "blocking.toggle", audit.Resource{Kind: "blocking"}, audit.ResultDenied
			}
			if err := st.Record(context.Background(), ev); err != nil {
				t.Fatalf("record %d: %v", i, err)
			}
		}
		return st
	}
	read := func(t *testing.T, st Store, q audit.Query) []audit.Event {
		t.Helper()
		evs, err := st.Read(context.Background(), q)
		if err != nil {
			t.Fatalf("read %+v: %v", q, err)
		}
		return evs
	}
	ordered := func(t *testing.T, evs []audit.Event, desc bool) {
		t.Helper()
		for i := 1; i < len(evs); i++ {
			if (evs[i].ID > evs[i-1].ID) == desc || evs[i].ID == evs[i-1].ID {
				t.Fatalf("events %d and %d out of order (desc=%v)", evs[i-1].ID, evs[i].ID, desc)
			}
		}
	}

	t.Run("OldestFirst", func(t *testing.T) {
		st := fixture(t)
		evs := read(t, st, audit.Query{Limit: 1000})
		if len(evs) != 30 {
			t.Fatalf("got %d events, want 30", len(evs))
		}
		ordered(t, evs, false)
		first := evs[0]
		if first.ID <= 0 || first.Action != "dnszone.update" || first.Actor.Name != "alice" || first.Resource.Name != "zone-0" ||
			first.Result != audit.ResultSuccess || !first.Time.Equal(base) {
			t.Fatalf("first event %+v", first)
		}
	})
	t.Run("NewestFirst", func(t *testing.T) {
		st := fixture(t)
		evs := read(t, st, audit.Query{Desc: true, Limit: 5})
		if len(evs) != 5 || evs[0].Action != "blocking.toggle" {
			t.Fatalf("newest page %+v", evs)
		}
		ordered(t, evs, true)
	})
	t.Run("Filters", func(t *testing.T) {
		st := fixture(t)
		for _, tc := range []struct {
			name string
			q    audit.Query
			want int
		}{
			{"kind", audit.Query{Kind: "blocking"}, 3},
			{"kind+name", audit.Query{Kind: "DNSZone", Name: "zone-0"}, 6},
			{"actor name", audit.Query{Actor: "bob"}, 10},
			{"actor kind:name", audit.Query{Actor: "User:carol"}, 10},
			{"unknown actor", audit.Query{Actor: "mallory"}, 0},
			{"since", audit.Query{Since: base.Add(20 * time.Minute)}, 10},
			{"actor+since", audit.Query{Actor: "alice", Since: base.Add(15 * time.Minute)}, 5},
		} {
			tc.q.Limit = 1000
			if got := read(t, st, tc.q); len(got) != tc.want {
				t.Errorf("%s: %d events, want %d", tc.name, len(got), tc.want)
			}
		}
	})
	t.Run("Pages", func(t *testing.T) {
		st := fixture(t)
		for _, desc := range []bool{false, true} {
			seen := map[int64]bool{}
			q := audit.Query{Limit: 7, Desc: desc}
			pages := 0
			for {
				evs := read(t, st, q)
				ordered(t, evs, desc)
				for _, ev := range evs {
					if seen[ev.ID] {
						t.Fatalf("desc=%v: event %d on two pages", desc, ev.ID)
					}
					seen[ev.ID] = true
				}
				pages++
				if len(evs) < q.Limit {
					break
				}
				last := evs[len(evs)-1].ID
				if desc {
					q.BeforeID = last
				} else {
					q.AfterID = last
				}
			}
			if len(seen) != 30 || pages != 5 {
				t.Fatalf("desc=%v: %d events in %d pages", desc, len(seen), pages)
			}
		}
		// A filtered page continues after its last event.
		evs := read(t, st, audit.Query{Actor: "bob", Limit: 4})
		rest := read(t, st, audit.Query{Actor: "bob", Limit: 100, AfterID: evs[len(evs)-1].ID})
		if len(evs)+len(rest) != 10 {
			t.Fatalf("filtered pages %d + %d, want 10", len(evs), len(rest))
		}
	})
	t.Run("Limit", func(t *testing.T) {
		st := fixture(t)
		if got := read(t, st, audit.Query{}); len(got) != 30 {
			t.Fatalf("default limit returned %d of 30", len(got))
		}
		if got := read(t, st, audit.Query{Limit: 2}); len(got) != 2 {
			t.Fatalf("limit 2 returned %d", len(got))
		}
	})
}
