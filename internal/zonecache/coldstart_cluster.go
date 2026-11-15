package zonecache

import "github.com/robertherbaugh/kudurru/internal/store/localdb"

// checkSnapshotCluster never derives the expected identity from saved state.
// Kubernetes needs a host-provisioned pin; other drivers supply their own
// independently known identity. Identity-free old snapshots need a live sync.
func (c *Cache) checkSnapshotCluster(snap *localdb.Snapshot) error {
	info := c.st.Info()
	expected := c.opts.ExpectedClusterID
	if info.Driver != "kube" && expected == "" {
		// Preserve other drivers' existing cold-start contract. Kubernetes
		// always requires an independently provisioned host pin.
		if info.ClusterID == "" || snap.ClusterID == "" {
			return nil
		}
		expected = info.ClusterID
	}
	return localdb.CheckClusterIdentity(expected, info.ClusterID, snap.ClusterID)
}
