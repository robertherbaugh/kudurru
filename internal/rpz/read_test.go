package rpz

import (
	"net/netip"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func TestSelectionCacheUntilRejectsOriginalClientIPTriggers(t *testing.T) {
	z := fixtureZone(t, "32.1.2.0.192.rpz-client-ip.policy.test. 60 IN CNAME .")
	s := NewSelection(z)
	if !z.AvailableAt(testNow) {
		t.Fatal("original source unavailable")
	}
	if _, ok := s.CacheUntil(testNow); ok {
		t.Fatal("client-IP source qualified shared cache")
	}
	for _, ip := range []string{"192.0.2.1", "192.0.2.2"} {
		d := s.Evaluate(Context{Name: "same.example.", ClientIP: netip.MustParseAddr(ip)}, testNow)
		if d.Unavailable || (d.Action == ActionNXDOMAIN) != (ip == "192.0.2.1") {
			t.Fatal("client-IP proof no longer matches original source")
		}
	}
}

func TestSelectionSummaryCountsAllRequiredSourcesAndExactExpiry(t *testing.T) {
	a := fixtureZone(t, "a.policy.test. 30 IN CNAME .", "b.policy.test. 30 IN CNAME rpz-passthru.")
	b := fixtureZone(t, "c.policy.test. 30 IN CNAME *.")
	b.spec.ExpiresAt = testNow.Add(2 * time.Minute)
	a.confirmedAt = testNow
	for _, tc := range []struct {
		name string
		s    *Selection
		now  time.Time
		n    int
		ok   bool
	}{
		{"nil", nil, testNow, 0, false},
		{"zero-time", NewSelection(a), time.Time{}, 0, false},
		{"empty", NewSelection(), testNow, 0, true},
		{"active", NewSelection(a, b), testNow, 3, true},
		{"missing-first", NewSelection(nil, a, b), testNow, 3, false},
		{"missing-last", NewSelection(a, b, nil), testNow, 3, false},
		{"placeholder", NewSelection(&Zone{}, a), testNow, 2, false},
		{"before-confirmed", NewSelection(a, b), testNow.Add(-time.Nanosecond), 3, false},
		{"before-expiry", NewSelection(a, b), a.spec.ExpiresAt.Add(-time.Nanosecond), 3, true},
		{"exact-expiry", NewSelection(a, b), a.spec.ExpiresAt, 3, false},
		{"all-expired", NewSelection(a, b), b.spec.ExpiresAt, 3, false},
		{"source-cap", NewSelection(make([]*Zone, maxSummarySources+1)...), testNow, 0, false},
		{"invalid-count", NewSelection(&Zone{stats: Stats{Rules: -1}}), testNow, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, ok := tc.s.Summary(tc.now)
			if n != tc.n || ok != tc.ok {
				t.Fatalf("Summary = %d,%v, want %d,%v", n, ok, tc.n, tc.ok)
			}
		})
	}
}

func TestSelectionSummaryBoundAcquiredMissingAndRotatedSources(t *testing.T) {
	fx := newRuntime(t, &testReceiver{serial: 1}, runtimeDB(t), runtimeList())
	startRuntime(t, fx.m)
	awaitStatus(t, fx, func(s Status) bool { return s.Available })
	// Actual manager acquisition/durable acceptance, not a fabricated public
	// credential. The unsigned installation still requires acquired metadata.
	list := fx.list
	id := SourceIdentity(list, 0)
	snapshot := fx.m.Snapshot()
	z := snapshot.Lookup(id)
	if z == nil || z.SOA() == nil {
		t.Fatal("manager did not acquire a complete source")
	}
	keys := snapshotRing(t, "hmac-sha256.", []byte("test-source-key"))
	bound, err := snapshot.Bind([]*public.BlockList{list}, keys)
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := bound.Select([]*public.BlockList{list}, true).Summary(fx.clock.now()); n != 1 || !ok {
		t.Fatal("acquired summary", n, ok)
	}
	if until, ok := bound.Select([]*public.BlockList{list}, true).CacheUntil(fx.clock.now()); !ok || !until.Equal(z.spec.ExpiresAt) {
		t.Fatal("cache proof did not retain original acquired expiry")
	}
	changed := list.DeepCopy()
	changed.Spec.Sources[0].RPZ.TSIGKeyRef = "rpz-key."
	if b, err := snapshot.Bind([]*public.BlockList{changed}, keys); err != nil {
		t.Fatal(err)
	} else if n, ok := b.Select([]*public.BlockList{changed}, true).Summary(fx.clock.now()); n != 0 || ok {
		t.Fatal("source/key change borrowed old acquisition", n, ok)
	} else if _, ok := b.Select([]*public.BlockList{changed}, true).CacheUntil(fx.clock.now()); ok {
		t.Fatal("cache proof borrowed rotated source acquisition")
	}
	if n, ok := bound.Select([]*public.BlockList{list}, true).Summary(z.spec.ExpiresAt); n != 1 || ok {
		t.Fatal("summary renewed actual durable expiry", n, ok)
	}
	if _, ok := bound.Select([]*public.BlockList{list}, true).CacheUntil(z.spec.ExpiresAt); ok {
		t.Fatal("cache proof renewed exact durable expiry")
	}
}

func TestSelectionCacheUntilOriginalBounds(t *testing.T) {
	a := fixtureZone(t, "a.policy.test. 30 IN CNAME .")
	b := fixtureZone(t, "b.policy.test. 30 IN CNAME .")
	b.spec.ExpiresAt = testNow.Add(2 * time.Minute)
	a.confirmedAt = testNow
	for _, s := range []*Selection{nil, NewSelection(nil), NewSelection(&Zone{}), NewSelection(a, nil)} {
		if _, ok := s.CacheUntil(testNow); ok {
			t.Fatal("missing or placeholder original enabled packet caching")
		}
	}
	if until, ok := NewSelection().CacheUntil(testNow); !ok || !until.IsZero() {
		t.Fatal("empty selection fabricated expiry")
	}
	s := NewSelection(a, b)
	if until, ok := s.CacheUntil(testNow); !ok || !until.Equal(a.spec.ExpiresAt) {
		t.Fatal("cache proof missed earliest original boundary")
	}
	for _, at := range []time.Time{{}, testNow.Add(-time.Nanosecond), a.spec.ExpiresAt} {
		if _, ok := s.CacheUntil(at); ok {
			t.Fatal("cache proof extended source lifetime")
		}
	}
}
