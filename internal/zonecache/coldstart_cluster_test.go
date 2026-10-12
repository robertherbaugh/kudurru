package zonecache

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
)

// clusterStore overrides the fake store's cluster ID.
type clusterStore struct {
	*fakeStore
	id string
}

func (c clusterStore) Info() store.Info {
	info := c.fakeStore.Info()
	info.ClusterID = c.id
	return info
}

// TestColdStartClusterIDCheck: a local.db snapshot from another cluster is
// refused at cold start (not ready until the store syncs); a matching or
// unknown cluster ID on either side serves the snapshot.
func TestColdStartClusterIDCheck(t *testing.T) {
	tests := []struct {
		name      string
		snapshot  string // ClusterID recorded in local.db
		store     string // Info().ClusterID of the store
		wantServe bool
	}{
		{"same cluster", "c1", "c1", true},
		{"other cluster", "c-copied", "c1", false},
		{"store does not know its cluster yet", "c-copied", "", true},
		{"snapshot without a cluster ID", "", "c1", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, _ := openDB(t)
			if err := db.Write(context.Background(), &localdb.Snapshot{
				Objects:   []store.Object{zone("home", "home.example.")},
				ClusterID: tt.snapshot,
				WrittenAt: time.Now(),
			}); err != nil {
				t.Fatal(err)
			}
			fs := newFakeStore()
			fs.holdBookmarks()
			c := newCache(t, clusterStore{fakeStore: fs, id: tt.store}, Options{LocalDB: db, StartupTimeout: 20 * time.Millisecond, Debounce: time.Millisecond})
			if err := c.Register(zonesBuilder()); err != nil {
				t.Fatal(err)
			}
			startCache(t, c)
			if tt.wantServe {
				waitState(t, c, FromSnapshot)
				return
			}
			eventually(t, 5*time.Second, "the snapshot to be rejected", func() bool {
				ok, msgs := c.Ready()
				last := ""
				if len(msgs) > 0 {
					last = msgs[len(msgs)-1]
				}
				return !ok && strings.Contains(last, "local.db snapshot rejected") && strings.Contains(last, `"c-copied"`)
			})
			if s := c.State(); s == FromSnapshot {
				t.Fatalf("serving a snapshot from another cluster (state %s)", s)
			}
			select {
			case <-c.Initialized():
				t.Fatal("initialized from a snapshot of another cluster")
			default:
			}
			// The store syncs: live data takes over as usual.
			fs.releaseBookmarks()
			waitState(t, c, Live)
		})
	}
}
