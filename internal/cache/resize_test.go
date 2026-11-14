package cache

import (
	"net/netip"
	"testing"
	"time"
)

func TestResizePreservesEntryStalePolicy(t *testing.T) {
	now := time.Now()
	c := New(Options{MaxEntries: 2, StaleTTL: time.Minute, Now: func() time.Time { return now }})
	a := MakeKey("a.example.", 1, 1, false, false, netip.Prefix{})
	b := MakeKey("b.example.", 1, 1, false, false, netip.Prefix{})
	c.Set(a, []byte{1}, Positive, 10*time.Second)
	c.Set(b, []byte{2}, Positive, 10*time.Second)
	c.Resize(1)
	if _, ok := c.Peek(a); ok {
		t.Fatal("capacity shrink retained the LRU entry")
	}
	now = now.Add(15 * time.Second)
	lookup, ok := c.Get(b)
	if !ok || lookup.State != Stale {
		t.Fatal("capacity-only update changed existing entry stale policy")
	}
	// Future entries must retain the policy too, not inherit zero Options from
	// a storage-capacity callback that was never authorized to change it.
	c.Set(b, []byte{3}, Positive, 10*time.Second)
	now = now.Add(15 * time.Second)
	lookup, ok = c.Get(b)
	if !ok || lookup.State != Stale {
		t.Fatal("capacity-only update changed future entry stale policy")
	}
}
