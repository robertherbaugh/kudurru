package cache

import (
	"bytes"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

func TestMetadataInsertionKeepsOneAllocationAndDetachedValues(t *testing.T) {
	now := time.Now()
	full := Metadata{Scope: pipeline.CacheScope{ID: [32]byte{1}, Epoch: 9}, Upstream: "original",
		Security: pipeline.Security{State: pipeline.ValidationSecure, Reason: "validated", ContextID: [32]byte{2}, ValidatedAt: now, ValidUntil: now.Add(time.Minute)}}
	for _, metadata := range []Metadata{{}, full} {
		c := New(Options{MaxEntries: 64, Now: func() time.Time { return now }})
		k := MakeScopedKey(metadata.Scope, "example.", 1, 1, false, false, netip.Prefix{})
		wire := []byte("unchanged wire")
		if got := testing.AllocsPerRun(100, func() { c.SetWithMetadata(k, wire, Positive, time.Hour, metadata) }); got != 1 {
			t.Fatalf("cache insertion allocations = %g, want one with or without provenance", got)
		}
		lookup, ok := c.Get(k)
		if !ok || lookup.Metadata != metadata {
			t.Fatal("original metadata was lost")
		}
		lookup.Metadata.Scope.Epoch++
		lookup.Metadata.Security.ValidUntil = now.Add(time.Hour)
		lookup.Metadata.Upstream = "foreign"
		if again, ok := c.Peek(k); !ok || again.Metadata != metadata {
			t.Fatal("returned metadata aliases cache ownership")
		}
		var dump bytes.Buffer
		if err := c.Dump(&dump); err != nil {
			t.Fatal(err)
		}
		restored := New(Options{MaxEntries: 64, Now: func() time.Time { return now }})
		if n, err := restored.Load(&dump); err != nil || n != 1 {
			t.Fatal("metadata representation changed dump compatibility", n, err)
		}
		after, ok := restored.Get(k)
		if !ok || after.Metadata.Scope != metadata.Scope || after.Metadata.Upstream != metadata.Upstream ||
			!after.Metadata.Security.ValidatedAt.Equal(metadata.Security.ValidatedAt) || !after.Metadata.Security.ValidUntil.Equal(metadata.Security.ValidUntil) {
			t.Fatal("restart lost original scope or security lifetime")
		}
	}
}

type metadataDumpWriter struct {
	bytes.Buffer
	entered, release chan struct{}
	once             sync.Once
}

func (w *metadataDumpWriter) Write(p []byte) (int, error) {
	w.once.Do(func() {
		close(w.entered)
		<-w.release
	})
	return w.Buffer.Write(p)
}

func TestMetadataDumpRetainsOriginalScopeDuringRebind(t *testing.T) {
	now := time.Now()
	options := Options{MaxEntries: 64, Now: func() time.Time { return now }} // one shard
	original := New(options)
	scope := pipeline.CacheScope{ID: [32]byte{1}, Epoch: 1}
	metadata := Metadata{Scope: scope, Upstream: "original", Security: pipeline.Security{State: pipeline.ValidationSecure, ValidatedAt: now, ValidUntil: now.Add(time.Minute)}}
	for i := range 32 {
		k := MakeScopedKey(scope, fmt.Sprintf("host%d.example.", i), 1, 1, false, false, netip.Prefix{})
		original.SetWithMetadata(k, bytes.Repeat([]byte{1}, 1024), Positive, time.Minute, metadata)
	}
	var seed bytes.Buffer
	if err := original.Dump(&seed); err != nil {
		t.Fatal(err)
	}
	current := New(options)
	if n, err := current.Load(&seed); err != nil || n != 32 {
		t.Fatal("restored fixture incomplete", n, err)
	}
	w := &metadataDumpWriter{entered: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	unblock := func() { release.Do(func() { close(w.release) }) }
	done := make(chan error, 1)
	go func() { done <- current.Dump(w) }()
	defer func() {
		unblock()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("dump did not finish")
		}
	}()
	select {
	case <-w.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("dump did not capture and begin writing the original shard")
	}
	next := scope
	next.Epoch++
	current.RebindRestored(next)
	unblock()
	select {
	case err := <-done:
		done <- err // leave the joined result for deferred cleanup
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("dump did not finish after rebind")
	}
	retained := New(options)
	if n, err := retained.Load(bytes.NewReader(w.Bytes())); err != nil || n != 32 {
		t.Fatal("dump paired original keys with rewritten metadata", n, err)
	}
	for i := range 32 {
		name := fmt.Sprintf("host%d.example.", i)
		oldKey := MakeScopedKey(scope, name, 1, 1, false, false, netip.Prefix{})
		newKey := MakeScopedKey(next, name, 1, 1, false, false, netip.Prefix{})
		if old, ok := retained.Get(oldKey); !ok || old.Metadata.Scope != scope {
			t.Fatal("dump did not preserve its original scope")
		}
		if live, ok := current.Get(newKey); !ok || live.Metadata.Scope != next {
			t.Fatal("live rebind did not retain its new scope")
		}
	}
}
