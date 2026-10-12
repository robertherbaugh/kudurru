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
