package objects

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/robertherbaugh/kudurru/internal/store"
)

// runLoader starts l.Run and stops it at the end of the test.
func runLoader(t *testing.T, l *Loader) {
	t.Helper()
	c, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Run(c) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
}

func TestWatchReappliesOnChange(t *testing.T) {
	f := newFixture(t, nil)
	f.write(t, "zones.yaml", zonesYAML)
	runLoader(t, f.l)
	eventually(t, "initial load", func() bool { return zone(t, f.st, "home") != nil })
	eventually(t, "watching", func() bool { return f.l.Status().Watching })

	// A new file is applied.
	f.write(t, "lists.yaml", listsYAML)
	eventually(t, "new file applied", func() bool { return owner(t, f.st, nsKey("BlockList", "default", "ads")) != "" })

	// An edit is re-applied.
	f.write(t, "zones.yaml", `
apiVersion: kudurru.io/v1alpha1
kind: DNSZone
metadata: { name: home }
spec: { zoneName: home.example., type: primary, defaultTTL: 42 }
`)
	eventually(t, "edit applied", func() bool {
		z := zone(t, f.st, "home")
		return z != nil && z.Spec.DefaultTTL == 42
	})
	eventually(t, "dropped document deleted", func() bool { return owner(t, f.st, nsKey("ClientGroup", "dns", "kids")) == "" })

	// Deleting the file deletes its objects.
	f.remove(t, "zones.yaml")
	eventually(t, "deleted file's objects removed", func() bool { return zone(t, f.st, "home") == nil })
	if owner(t, f.st, nsKey("BlockList", "default", "ads")) == "" {
		t.Fatal("objects of other files must stay")
	}
}

func TestWatchDebouncesBursts(t *testing.T) {
	f := newFixture(t, nil, func(o *Options) { o.Debounce = 300 * time.Millisecond })
	runLoader(t, f.l)
	eventually(t, "watching", func() bool { return f.l.Status().Watching })
	first := f.l.Status().LastLoad

	// Ten writes inside the debounce window are coalesced.
	for range 10 {
		f.write(t, "lists.yaml", listsYAML)
		time.Sleep(10 * time.Millisecond)
	}
	eventually(t, "burst applied", func() bool { return owner(t, f.st, nsKey("BlockList", "default", "ads")) != "" })
	time.Sleep(500 * time.Millisecond)
	// One load is expected; allow a second in case the shared CI machine
	// stalls a write past the debounce window.
	if loads := sampleLoads(f); loads < 1 || loads > 2 {
		t.Fatalf("burst caused %d loads after the initial one (last load %v, first %v)", loads, f.l.Status().LastLoad, first)
	}
}

// sampleLoads counts loads after the initial one via the applied metrics:
// the initial load saw no objects, every later load sees the one BlockList
// (created once, then unchanged).
func sampleLoads(f *fixture) int {
	n := 0
	for _, r := range []string{ResultCreated, ResultUpdated, ResultUnchanged} {
		n += int(testutil.ToFloat64(f.l.m.applied.WithLabelValues(r)))
	}
	return n
}

func TestWatchPollPicksUpLateDirectory(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "objects.d")
	f := newFixture(t, nil, func(o *Options) {
		o.Dir = dir
		o.PollInterval = 100 * time.Millisecond
	})
	runLoader(t, f.l)
	eventually(t, "initial load", func() bool { return f.l.Status().Loaded })
	if f.l.Status().Watching {
		t.Fatal("a missing directory cannot be watched")
	}
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "zones.yaml"), []byte(zonesYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	eventually(t, "late directory loaded by the poll", func() bool { return zone(t, f.st, "home") != nil })
	eventually(t, "late directory watched", func() bool { return f.l.Status().Watching })

	// Once watched, changes arrive through fsnotify as well.
	if err := os.Remove(filepath.Join(dir, "zones.yaml")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "removal applied", func() bool { return zone(t, f.st, "home") == nil })
}

func TestTriggerReloads(t *testing.T) {
	f := newFixture(t, nil, func(o *Options) { o.Dir = "" })
	runLoader(t, f.l)
	eventually(t, "initial load", func() bool { return f.l.Status().Loaded })
	seedAPIZone(t, f.st)
	before := f.l.Status().LastLoad
	f.l.Trigger()
	f.l.Trigger() // coalesced, never blocks
	eventually(t, "triggered load", func() bool { return f.l.Status().LastLoad.After(before) })
}

func seedAPIZone(t *testing.T, st store.Store) {
	t.Helper()
	if _, err := ApplyBytes(ctx(t), st, "seed.yaml", []byte(zonesYAML), WithValidator(testValidator(t))); err != nil {
		t.Fatal(err)
	}
}
