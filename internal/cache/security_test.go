package cache

import (
	"bytes"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

func TestSecurityLifetimeScopeAndRestart(t *testing.T) {
	now := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	c := New(Options{MaxEntries: 100, StaleTTL: time.Hour, Now: func() time.Time { return now }})
	scope := pipeline.CacheScope{ID: [32]byte{1}, Epoch: 10}
	k := MakeScopedKey(scope, "Example.", 1, 1, true, false, netip.Prefix{})
	m := Metadata{Scope: scope, Security: pipeline.Security{State: pipeline.ValidationSecure, ContextID: [32]byte{2}, ValidatedAt: now, ValidUntil: now.Add(time.Minute)}, Upstream: "default/verified"}
	c.SetWithMetadata(k, []byte("wire"), Positive, 20*time.Second, m)
	if p, ok := k.Parts(); !ok || p.Scope != scope || p.Name != "example." {
		t.Fatal("scoped key decode")
	}
	now = now.Add(30 * time.Second)
	if l, ok := c.Get(k); !ok || l.State != Stale || l.Metadata != m {
		t.Fatal("stale provenance lost")
	}
	var dump bytes.Buffer
	if err := c.Dump(&dump); err != nil {
		t.Fatal(err)
	}
	d := New(Options{MaxEntries: 100, StaleTTL: time.Hour, Now: func() time.Time { return now }})
	if n, err := d.Load(bytes.NewReader(dump.Bytes())); err != nil || n != 1 {
		t.Fatalf("load %d %v", n, err)
	}
	newScope := scope
	newScope.Epoch = 99
	d.RebindRestored(newScope)
	newKey := MakeScopedKey(newScope, "example.", 1, 1, true, false, netip.Prefix{})
	if l, ok := d.Get(newKey); !ok || l.Metadata.Scope != newScope || l.Metadata.Security != m.Security {
		t.Fatal("stable trust context not restored")
	}
	if _, ok := d.Get(k); ok {
		t.Fatal("old epoch remains")
	}
	now = m.Security.ValidUntil
	if _, ok := d.Get(newKey); ok {
		t.Fatal("signature exact expiry served stale")
	}
	if n, err := New(Options{MaxEntries: 100, Now: func() time.Time { return now }}).Load(bytes.NewReader(dump.Bytes())); err != nil || n != 0 {
		t.Fatal("restart renewed security expiry")
	}
}

func TestRestoredEpochMovesShardAndPreservesLiveEntry(t *testing.T) {
	now := time.Now()
	c := New(Options{MaxEntries: 1024, Now: func() time.Time { return now }})
	old := pipeline.CacheScope{ID: [32]byte{1}, Epoch: 1}
	next := old
	next.Epoch = 2
	var dump bytes.Buffer
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("host%d.example.", i)
		c.SetWithMetadata(MakeScopedKey(old, name, 1, 1, false, false, netip.Prefix{}), []byte(name), Positive, time.Hour, Metadata{Scope: old})
	}
	if err := c.Dump(&dump); err != nil {
		t.Fatal(err)
	}
	m := NewMetrics(nil)
	d := New(Options{MaxEntries: 1024, Now: func() time.Time { return now }, Metrics: m})
	if n, err := d.Load(&dump); err != nil || n != 20 {
		t.Fatalf("load: %d %v", n, err)
	}
	live := MakeScopedKey(next, "host0.example.", 1, 1, false, false, netip.Prefix{})
	d.SetWithMetadata(live, []byte("live"), Positive, time.Hour, Metadata{Scope: next})
	moved := false
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("host%d.example.", i)
		moved = moved || d.shard(MakeScopedKey(old, name, 1, 1, false, false, netip.Prefix{})) != d.shard(MakeScopedKey(next, name, 1, 1, false, false, netip.Prefix{}))
	}
	if !moved {
		t.Fatal("test did not exercise a changed shard")
	}
	d.RebindRestored(next)
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("host%d.example.", i)
		l, ok := d.Get(MakeScopedKey(next, name, 1, 1, false, false, netip.Prefix{}))
		if !ok || l.Metadata.Scope != next {
			t.Fatalf("promoted entry missing: %s", name)
		}
		if i == 0 && string(l.Wire) != "live" {
			t.Fatal("restored entry replaced live data")
		}
		if _, ok := d.Get(MakeScopedKey(old, name, 1, 1, false, false, netip.Prefix{})); ok {
			t.Fatal("retired epoch retained")
		}
	}
	if got := testutil.ToFloat64(m.Entries.WithLabelValues("positive")); got != 20 {
		t.Fatalf("entry gauge %v", got)
	}
}

func TestCacheMutationFencesDetachedWrites(t *testing.T) {
	for _, action := range []string{"flush", "delete", "zone"} {
		t.Run(action, func(t *testing.T) {
			c := New(Options{MaxEntries: 100})
			k := key("example.")
			g := c.Generation()
			switch action {
			case "flush":
				c.Flush()
			case "delete":
				c.Delete(k)
			case "zone":
				c.FlushZone("example.")
			}
			c.SetIfGeneration(k, []byte("retired"), Positive, time.Hour, Metadata{}, g)
			if _, ok := c.Get(k); ok {
				t.Fatal("retired work repopulated invalidated cache")
			}
		})
	}
}

func TestRestoredScopeDoesNotPromoteLiveRetiredEntries(t *testing.T) {
	c := New(Options{MaxEntries: 100})
	old := pipeline.CacheScope{ID: [32]byte{1}, Epoch: 1}
	next := old
	next.Epoch = 2
	k := MakeScopedKey(old, "example.", 1, 1, false, false, netip.Prefix{})
	c.SetWithMetadata(k, []byte("live"), Positive, time.Hour, Metadata{Scope: old})
	c.RebindRestored(next)
	if _, ok := c.Get(MakeScopedKey(next, "example.", 1, 1, false, false, netip.Prefix{})); ok {
		t.Fatal("live retired work promoted into current scope")
	}
	if _, err := c.Load(bytes.NewReader([]byte("KDRC\x01\x00"))); err == nil {
		t.Fatal("legacy unauthenticated dump accepted")
	}
}

func TestClockRollbackCannotReviveClaims(t *testing.T) {
	now := time.Now()
	c := New(Options{MaxEntries: 100, Now: func() time.Time { return now }})
	k := key("example.")
	c.SetWithMetadata(k, []byte("wire"), Positive, time.Hour, Metadata{Security: pipeline.Security{State: pipeline.ValidationSecure, ValidatedAt: now, ValidUntil: now.Add(time.Hour)}})
	now = now.Add(-time.Second)
	if _, ok := c.Get(k); ok {
		t.Fatal("backward clock revived claim")
	}
}
