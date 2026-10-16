package dhcpha

import (
	"encoding/json"
	"net/netip"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"github.com/robertherbaugh/kudurru/internal/dhcp"
)

func TestFSMClampsDelayedLeaderTime(t *testing.T) {
	c := clusterFixture(t)
	store, err := dhcp.OpenReplicaLeasesForSetup(c.paths[0], c.leaseConfigs[0], "initial", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	m := &machine{stores: map[string]*dhcp.LeaseStore{"default": store}, names: []string{"default"}, fingerprint: "test"}
	m.initialized.Store(true)
	committedAt := time.Now().UTC().Truncate(time.Second)
	m.lastTime.Store(committedAt.Unix())
	apply := func(index uint64, operation string, address netip.Addr) dhcp.Lease {
		t.Helper()
		// A new leader can enqueue an old timestamp before the previous leader's
		// latest committed command reaches its FSM. Apply must clamp on all nodes.
		cmd := command{Scope: "default", Fingerprint: "test", Lease: dhcp.ReplicaCommand{Operation: operation, At: committedAt.Add(-time.Hour), Client: client(1), Address: address, Mode: dhcp.Selecting}}
		data, err := json.Marshal(cmd)
		if err != nil {
			t.Fatal(err)
		}
		result := m.Apply(&raft.Log{Index: index, Data: data}).(applyResult)
		if result.Err != nil {
			t.Fatal(result.Err)
		}
		return result.Result.Lease
	}
	offer := apply(1, "offer", netip.Addr{})
	bound := apply(2, "acknowledge", offer.Address)
	if want := committedAt.Add(c.leaseConfigs[0].LeaseDuration); !bound.ExpiresAt.Equal(want) || bound.GrantedDuration != c.leaseConfigs[0].LeaseDuration || m.lastTime.Load() != committedAt.Unix() {
		t.Fatalf("backward leader time changed committed lease lifetime: %+v; want expiry %v", bound, want)
	}
}
