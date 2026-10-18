package objects

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

func testValidator(t testing.TB) *validator.Validator {
	t.Helper()
	v, err := sharedValidator()
	if err != nil {
		t.Fatalf("validator: %v", err)
	}
	return v
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type fixture struct {
	st  *memory.Store
	dir string // .../objects.d
	reg *prometheus.Registry
	l   *Loader
}

// newFixture builds a memory store, an objects.d directory and a loader.
func newFixture(t *testing.T, inline []map[string]any, mutate ...func(*Options)) *fixture {
	t.Helper()
	f := &fixture{st: memory.New(), dir: filepath.Join(t.TempDir(), "objects.d"), reg: prometheus.NewRegistry()}
	t.Cleanup(func() { _ = f.st.Close() })
	if err := os.Mkdir(f.dir, 0o750); err != nil {
		t.Fatal(err)
	}
	opts := Options{
		Store:        f.st,
		Validator:    testValidator(t),
		Inline:       inline,
		Dir:          f.dir,
		Debounce:     50 * time.Millisecond,
		PollInterval: -1,
		Registerer:   f.reg,
		Logger:       quiet,
	}
	for _, m := range mutate {
		m(&opts)
	}
	l, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	f.l = l
	return f
}

func memoryStore(t *testing.T) *memory.Store {
	t.Helper()
	st := memory.New()
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func (f *fixture) write(t *testing.T, name, content string) {
	t.Helper()
	tmp := filepath.Join(filepath.Dir(f.dir), ".tmp-"+filepath.Base(name))
	if err := os.WriteFile(tmp, []byte(strings.TrimLeft(content, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(f.dir, name)); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) remove(t *testing.T, name string) {
	t.Helper()
	if err := os.Remove(filepath.Join(f.dir, name)); err != nil {
		t.Fatal(err)
	}
}

// get returns the stored object or nil.
func get[T store.Object](t *testing.T, st store.Store, key store.Key, into T) T {
	t.Helper()
	err := st.Get(context.Background(), key, into)
	if errors.Is(err, store.ErrNotFound) {
		var zero T
		return zero
	}
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	return into
}

func zoneKey(name string) store.Key { return store.Key{Kind: "DNSZone", Name: name} }

func nsKey(kind, ns, name string) store.Key {
	return store.Key{Kind: store.Kind(kind), Namespace: ns, Name: name}
}

func zone(t *testing.T, st store.Store, name string) *v1alpha1.DNSZone {
	t.Helper()
	return get(t, st, zoneKey(name), &v1alpha1.DNSZone{})
}

// owner returns the managedBy of key, or "" when absent.
func owner(t *testing.T, st store.Store, key store.Key) string {
	t.Helper()
	obj, err := newObject(v1alpha1.GroupVersion.WithKind(string(key.Kind)))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Get(context.Background(), key, obj); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ""
		}
		t.Fatal(err)
	}
	return store.ManagedBy(obj)
}

// findDiag returns the first diagnosis with reason r, or fails.
func findDiag(t *testing.T, diags []Diagnosis, r Reason) Diagnosis {
	t.Helper()
	for _, d := range diags {
		if d.Reason == r {
			return d
		}
	}
	t.Fatalf("no %s diagnosis in %v", r, diags)
	return Diagnosis{}
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
