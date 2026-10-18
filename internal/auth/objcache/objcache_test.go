package objcache

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
)

func user(name string) *internalv1.LocalUser {
	return &internalv1.LocalUser{
		TypeMeta:   metav1.TypeMeta{APIVersion: "kudurru.io/v1alpha1", Kind: internalv1.KindLocalUser},
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestUpsertDoesNotResurrect is the regression test of a deleted local user
// staying in the cache: the lastLoginAt update after a login read the user
// back just before the deletion and upserted that copy after the watch had
// removed it, so the node kept authenticating the deleted account.
func TestUpsertDoesNotResurrect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := memory.New()
	c := New[*internalv1.LocalUser](st, internalv1.KindLocalUser, Options{})
	go func() { _ = c.Run(ctx) }()
	<-c.Synced()

	key := store.Key{Kind: internalv1.KindLocalUser, Name: "dave"}
	if _, err := st.Apply(ctx, user("dave"), store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the user in the cache", func() bool { _, ok := c.Get(key); return ok })
	stale := &internalv1.LocalUser{}
	if err := st.Get(ctx, key, stale); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, key, store.DeleteOptions{Manager: store.ManagerSystem, Force: true}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the deletion", func() bool { _, ok := c.Get(key); return !ok })

	c.Upsert(stale)
	if _, ok := c.Get(key); ok {
		t.Fatal("an upsert of a copy read before the deletion put the deleted user back")
	}

	// A user re-created with the same name is a newer version and is cached.
	again, err := st.Apply(ctx, user("dave"), store.ApplyOptions{Manager: store.ManagerSystem})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the re-created user", func() bool {
		u, ok := c.Get(key)
		return ok && u.ResourceVersion == again.GetResourceVersion()
	})

	// Remove (a deletion the caller made) refuses the copy it removed.
	cur, _ := c.Get(key)
	c.Remove(key)
	c.Upsert(cur)
	if _, ok := c.Get(key); ok {
		t.Fatal("Upsert after Remove put the removed copy back")
	}
}

// scriptedStore hands the cache a watch whose events the test sends.
type scriptedStore struct {
	store.Store
	events chan store.Event
}

type scriptedWatcher struct{ ch chan store.Event }

func (w scriptedWatcher) ResultChan() <-chan store.Event { return w.ch }
func (w scriptedWatcher) Stop()                          {}

func (s *scriptedStore) Watch(context.Context, store.Kind, store.WatchOptions) (store.Watcher, error) {
	return scriptedWatcher{s.events}, nil
}

func userAt(name, rv string) *internalv1.LocalUser {
	u := user(name)
	u.ResourceVersion = rv
	return u
}

// bookmark is the end of the initial list; rv is the resourceVersion the
// driver read the list at ("" when it does not report one).
func bookmark(rv string) store.Event {
	bm := &internalv1.LocalUser{}
	bm.ResourceVersion = rv
	return store.Event{Type: store.Bookmark, Object: bm}
}

// TestPruneKeepsWritesAfterTheList is the regression test of a new token
// that was unknown on its node: a write (Upsert) between the initial list
// of a watch and its Bookmark is not in the list, and prune dropped it and
// left a tombstone that then ignored the object's own Added event. The
// sqlite and kube drivers report the list's resourceVersion on the
// Bookmark; objects newer than it are kept. Without that version (the
// memory driver) prune drops every object the list did not contain, and
// objects older than the list's version are dropped as before.
func TestPruneKeepsWritesAfterTheList(t *testing.T) {
	for _, tc := range []struct {
		name, listRV string
		keepFresh    bool
	}{
		{"bookmark with the list's version", "10", true},
		{"bookmark without a version", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			st := &scriptedStore{Store: memory.New(), events: make(chan store.Event)}
			c := New[*internalv1.LocalUser](st, internalv1.KindLocalUser, Options{})
			done := make(chan struct{})
			go func() { _ = c.Run(ctx); close(done) }()
			defer func() { cancel(); <-done }()

			// A cached object the new list no longer has (deleted while
			// the watch was down), older than the list.
			c.Upsert(userAt("gone", "5"))
			// The initial list holds "listed"; "fresh" is written by this
			// node after the list was read, before its Bookmark.
			st.events <- store.Event{Type: store.Added, Object: userAt("listed", "9")}
			c.Upsert(userAt("fresh", "11"))
			st.events <- bookmark(tc.listRV)
			<-c.Synced()

			get := func(name string) bool {
				_, ok := c.Get(store.Key{Kind: internalv1.KindLocalUser, Name: name})
				return ok
			}
			// The cache applies events in order: an Added event after the
			// Bookmark shows that the Bookmark was handled.
			st.events <- store.Event{Type: store.Added, Object: userAt("probe", "12")}
			waitFor(t, "the probe object", func() bool { return get("probe") })
			if !get("listed") {
				t.Error("the listed object is missing")
			}
			if get("gone") {
				t.Error("an object older than the list and not in it was kept")
			}
			if got := get("fresh"); got != tc.keepFresh {
				t.Errorf("object written after the list: cached %v, want %v", got, tc.keepFresh)
			}
			// The fresh object's own Added event does not change that: with
			// the list's version it was never dropped; without it, the
			// tombstone is at the object's own version, which the event does
			// not supersede, so it stays out until a newer write (the limit
			// of a driver that reports no list version).
			st.events <- store.Event{Type: store.Added, Object: userAt("fresh", "11")}
			st.events <- store.Event{Type: store.Added, Object: userAt("probe2", "13")}
			waitFor(t, "the second probe", func() bool { return get("probe2") })
			if got := get("fresh"); got != tc.keepFresh {
				t.Errorf("after its Added event: cached %v, want %v", got, tc.keepFresh)
			}
		})
	}
}
