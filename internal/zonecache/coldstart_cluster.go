package zonecache

import (
	"fmt"

	"github.com/robertherbaugh/kudurru/internal/store/localdb"
)

// checkSnapshotCluster is the cold-start cluster-ID check: a local.db
// snapshot written in another cluster (a data directory copied between
// clusters, a restored volume) must not answer for this one. It compares
// the snapshot's ClusterID with the store's Info().ClusterID and returns
// an error when both are known and differ.
//
// Either side may be unknown: the kube driver learns its cluster ID (the
// kube-system namespace UID) from the apiserver, which is exactly what is
// missing at a cold start during an outage, and snapshots written before
// the ID was known carry none. The snapshot is then served unverified;
// once the store syncs, zonecache replaces it with live data anyway.
func (c *Cache) checkSnapshotCluster(snap *localdb.Snapshot) error {
	want := c.st.Info().ClusterID
	if want == "" || snap.ClusterID == "" {
		return nil
	}
	if snap.ClusterID != want {
		return fmt.Errorf("local.db snapshot belongs to cluster %q but the store reports cluster %q", snap.ClusterID, want)
	}
	return nil
}
