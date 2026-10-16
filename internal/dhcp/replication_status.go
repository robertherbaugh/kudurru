package dhcp

import "time"

// ReplicationStatus describes the committed allocator replica independently of
// the consensus implementation. Ready never substitutes for a quorum commit.
type ReplicationStatus struct {
	Enabled       bool          `json:"enabled"`
	NodeID        string        `json:"nodeID"`
	ClusterID     string        `json:"clusterID"`
	Role          string        `json:"role"`
	LeaderID      string        `json:"leaderID"`
	LeaderAddress string        `json:"leaderAddress"`
	Ready         bool          `json:"ready"`
	Initialized   bool          `json:"initialized"`
	AppliedIndex  uint64        `json:"appliedIndex"`
	CommitIndex   uint64        `json:"commitIndex"`
	LastContact   time.Duration `json:"lastContact"`
	Error         string        `json:"error"`
}

// SetReplicationStatus installs a read-only health provider before startup.
func (s *Service) SetReplicationStatus(provider func() ReplicationStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replicationStatus = provider
}

// Replication returns current quorum health without affecting lease ownership.
func (s *Service) Replication() ReplicationStatus {
	s.mu.Lock()
	provider := s.replicationStatus
	s.mu.Unlock()
	if provider == nil {
		return ReplicationStatus{}
	}
	return provider()
}
