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
	info.Driver = "kube"
	return info
}

// TestColdStartClusterIDCheck: a local.db snapshot from another cluster is
// refused at cold start (not ready until the store syncs); only an independently pinned, matching identity serves the snapshot.
func TestColdStartClusterIDCheck(t *testing.T) {
	tests := []struct {
		name      string
		snapshot  string // ClusterID recorded in local.db
		store     string // Info().ClusterID of the store
		expected  string // independently provisioned host pin
		wantServe bool
	}{
		{"same cluster online", "c1", "c1", "c1", true},
		{"same cluster offline", "c1", "", "c1", true},
		{"other saved cluster", "c-copied", "", "c1", false},
		{"other online cluster", "c1", "c-other", "c1", false},
		{"missing expected identity offline", "c1", "", "", false},
		{"missing expected identity with live observation", "c1", "c1", "", false},
		{"old snapshot without identity", "", "c1", "c1", false},
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
			c := newCache(t, clusterStore{fakeStore: fs, id: tt.store}, Options{LocalDB: db, ExpectedClusterID: tt.expected, StartupTimeout: 20 * time.Millisecond, Debounce: time.Millisecond})
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
				return !ok && strings.Contains(last, "local.db snapshot rejected")
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
			if tt.snapshot == "" {
				eventually(t, 5*time.Second, "the old snapshot to be replaced by identified live state", func() bool {
					saved, err := db.Load(t.Context())
					return err == nil && saved.ClusterID == tt.store
				})
			}
		})
	}
}
