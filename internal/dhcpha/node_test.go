package dhcpha

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/dhcp"
)

type testCluster struct {
	t            *testing.T
	nodes        [3]*Node
	stores       [3]*dhcp.LeaseStore
	configs      [3]Config
	leaseConfigs [3]dhcp.Config
	paths        [3]string
	partitioned  [3]atomic.Bool
}

func clusterFixture(t *testing.T) *testCluster {
	t.Helper()
	c := &testCluster{t: t}
	root := t.TempDir()
	peers := make([]Peer, 3)
	for i := range peers {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		peers[i] = Peer{ID: fmt.Sprintf("node%d", i+1), Address: l.Addr().String()}
		_ = l.Close()
	}
	servers := []dhcp.ScopeServer{{NodeID: "node1", Address: netip.MustParseAddr("192.0.2.2")}, {NodeID: "node2", Address: netip.MustParseAddr("192.0.2.3")}, {NodeID: "node3", Address: netip.MustParseAddr("192.0.2.4")}}
	for i := range c.nodes {
		credentials, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: []byte("01234567890123456789012345678901"), NodeName: peers[i].ID})
		if err != nil {
			t.Fatal(err)
		}
		tlsConfig, err := TLSFromCredentials(credentials)
		if err != nil {
			t.Fatal(err)
		}
		c.configs[i] = Config{NodeID: peers[i].ID, ClusterID: "test-cluster", ListenAddress: peers[i].Address, DataDir: filepath.Join(root, peers[i].ID, "raft"), Peers: peers, Bootstrap: i == 0, TLS: tlsConfig, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		c.leaseConfigs[i] = dhcp.Config{Name: "default", Management: "api", Interface: fmt.Sprintf("eth%d", i), ServerIP: servers[i].Address, Subnet: netip.MustParsePrefix("192.0.2.0/24"), RangeStart: netip.MustParseAddr("192.0.2.10"), RangeEnd: netip.MustParseAddr("192.0.2.200"), Router: netip.MustParseAddr("192.0.2.1"), DNSServers: []netip.Addr{netip.MustParseAddr("192.0.2.1")}, DomainName: "home.example", LeaseDuration: 12 * time.Hour, RegisterDNS: true, DNSTTL: 30 * time.Second, ConflictDetection: "disabled", FailoverServers: servers}
		c.paths[i] = filepath.Join(root, peers[i].ID, "dhcp.db")
	}
	t.Cleanup(func() {
		for i := range c.nodes {
			c.stop(i)
		}
	})
	return c
}
func (c *testCluster) start(i int) {
	c.t.Helper()
	s, err := dhcp.OpenReplicaLeases(c.paths[i], c.leaseConfigs[i])
	if err != nil {
		c.t.Fatal(err)
	}
	n, err := Open(c.configs[i], map[string]*dhcp.LeaseStore{"default": s})
	if err != nil {
		_ = s.Close()
		c.t.Fatal(err)
	}
	c.stores[i], c.nodes[i] = s, n
	// The flag closure is installed before any partition flag is set. The
	// stream reads it concurrently but never replaces this function.
	n.stream.blocked.Store(&partitionRule{blocked: func(peer string) bool {
		if c.partitioned[i].Load() {
			return true
		}
		for j, p := range c.configs[i].Peers {
			if p.ID == peer {
				return c.partitioned[j].Load()
			}
		}
		return false
	}})
}
func (c *testCluster) startAll() {
	for i := range c.nodes {
		c.start(i)
	}
	c.leader(-1)
}
func (c *testCluster) stop(i int) {
	if c.nodes[i] != nil {
		_ = c.nodes[i].Close()
		c.nodes[i] = nil
	}
	if c.stores[i] != nil {
		_ = c.stores[i].Close()
		c.stores[i] = nil
	}
}
func (c *testCluster) leader(exclude int) int {
	c.t.Helper()
	leader := -1
	eventually(c.t, 12*time.Second, func() bool {
		for i, n := range c.nodes {
			if i == exclude || n == nil {
				continue
			}
			if n.raft.State() == raft.Leader && n.fsm.initialized.Load() {
				ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
				err := n.Ready(ctx)
				cancel()
				if err == nil {
					leader = i
					return true
				}
			}
		}
		return false
	}, "a leader with current quorum authority")
	return leader
}
func eventually(t *testing.T, timeout time.Duration, fn func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}
func (c *testCluster) consistent() {
	c.t.Helper()
	eventually(c.t, 8*time.Second, func() bool {
		var expected any
		for i, s := range c.stores {
			if s == nil {
				continue
			}
			b, err := s.CaptureReplica(context.Background())
			if err != nil {
				return false
			}
			var v map[string]any
			if json.Unmarshal(b, &v) != nil {
				return false
			}
			cfg := v["config"].(map[string]any)
			delete(cfg, "Interface")
			delete(cfg, "ServerIP")
			if expected == nil {
				expected = v
			} else if !reflect.DeepEqual(expected, v) {
				c.t.Logf("replica %d not caught up", i)
				return false
			}
		}
		return true
	}, "identical committed lease/configuration/claim state on replicas")
}
func client(i int) dhcp.Client {
	return dhcp.Client{MAC: fmt.Sprintf("02:00:00:00:00:%02x", i), Hostname: fmt.Sprintf("client%d", i)}
}
func (c *testCluster) offer(leader, i int) dhcp.Lease {
	c.t.Helper()
	l, err := c.nodes[leader].Backend("default").Offer(context.Background(), client(i), netip.Addr{})
	if err != nil {
		c.t.Fatal(err)
	}
	return l
}
func snapshotLeases(t *testing.T, s *dhcp.LeaseStore) []map[string]any {
	t.Helper()
	b, err := s.CaptureReplica(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Leases []map[string]any `json:"leases"`
	}
	if json.Unmarshal(b, &v) != nil {
		t.Fatal("invalid snapshot")
	}
	return v.Leases
}

func TestThreeNodeReplicatedOwnershipAndManagement(t *testing.T) {
	c := clusterFixture(t)
	c.startAll()
	leader := c.leader(-1)
	follower := (leader + 1) % 3
	backend := c.nodes[leader].Backend("default")
	offer := c.offer(leader, 1)
	c.consistent()
	if _, err := c.nodes[follower].Backend("default").Offer(context.Background(), client(9), netip.Addr{}); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("follower allocated: %v", err)
	}
	if err := c.nodes[follower].Backend("default").CheckServing(context.Background()); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("follower served nonmutating DHCP: %v", err)
	}
	bound, err := backend.Acknowledge(context.Background(), client(1), offer.Address, dhcp.Selecting)
	if err != nil || bound.State != "bound" {
		t.Fatalf("ACK: %+v %v", bound, err)
	}
	decline := c.offer(leader, 2)
	if err := backend.Decline(context.Background(), client(2), decline.Address); err != nil {
		t.Fatal(err)
	}
	quarantine := c.offer(leader, 3)
	if err := backend.QuarantineOffer(context.Background(), client(3), quarantine.Address); err != nil {
		t.Fatal(err)
	}
	fb := c.nodes[follower].Backend("default")
	state := fb.Configuration()
	reservation := dhcp.Reservation{MAC: client(4).MAC, IP: netip.MustParseAddr("192.0.2.150"), Hostname: "printer"}
	if err := fb.PutReservation(context.Background(), state.Revision, reservation); err != nil {
		t.Fatal(err)
	}
	if got := fb.Configuration(); got.Revision != state.Revision+1 || len(got.Config.Reservations) != 1 {
		t.Fatalf("forwarded mutation returned before local commit: %+v", got)
	}
	if err := fb.PutReservation(context.Background(), state.Revision, reservation); !errors.Is(err, dhcp.ErrRevisionConflict) {
		t.Fatalf("forwarded conflict lost error type: %v", err)
	}
	if err := fb.DeleteReservation(context.Background(), fb.Configuration().Revision, reservation.MAC); err != nil {
		t.Fatal(err)
	}
	settings := fb.Configuration()
	opts := dhcp.PoolOptions{RangeStart: settings.Config.RangeStart, RangeEnd: settings.Config.RangeEnd, Router: settings.Config.Router, DNSServers: settings.Config.DNSServers, DomainName: settings.Config.DomainName, LeaseDuration: 6 * time.Hour, RegisterDNS: true, DNSTTL: time.Minute}
	if err := fb.UpdateOptions(context.Background(), settings.Revision, opts); err != nil {
		t.Fatal(err)
	}
	if fb.Configuration().Config.LeaseDuration != 6*time.Hour {
		t.Fatal("forwarded options read is stale")
	}
	data := []byte(fmt.Sprintf("%d 02:00:00:00:00:0a 192.0.2.100 imported *\n", time.Now().Add(time.Hour).Unix()))
	if result, err := fb.Import(context.Background(), "dnsmasq", data); err != nil || result.Imported != 1 {
		t.Fatalf("import: %+v %v", result, err)
	}
	c.consistent()
	if len(snapshotLeases(t, c.stores[leader])) != 4 {
		t.Fatal("lost offered/bound/declined/imported states")
	}
	if err := backend.Release(context.Background(), client(1), bound.Address); err != nil {
		t.Fatal(err)
	}
	c.consistent()
	if err := c.nodes[leader].Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.stop(follower)
	c.start(follower)
	c.consistent()
	if c.stores[follower].Configuration().Config.ServerIP != c.leaseConfigs[follower].ServerIP {
		t.Fatal("restore overwrote local server address")
	}
}

func TestLeadershipLossPartitionAndRejoin(t *testing.T) {
	c := clusterFixture(t)
	c.startAll()
	old := c.leader(-1)
	offer := c.offer(old, 1)
	if _, err := c.nodes[old].Backend("default").Acknowledge(context.Background(), client(1), offer.Address, dhcp.Selecting); err != nil {
		t.Fatal(err)
	}
	c.consistent()
	// A bidirectional partition closes pooled streams as well as denying new
	// connections; independent real Raft engines must elect the majority leader.
	c.partitioned[old].Store(true)
	for _, n := range c.nodes {
		n.transport.CloseStreams()
	}
	start := time.Now()
	if _, err := c.nodes[old].Backend("default").Offer(context.Background(), client(2), netip.Addr{}); err == nil {
		t.Fatal("isolated leader returned an OFFER")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("no-quorum request was not bounded")
	}
	majority := c.leader(old)
	fresh := c.offer(majority, 3)
	if fresh.Address == offer.Address {
		t.Fatal("new leader reused live ownership")
	}
	if _, err := c.nodes[old].Backend("default").Acknowledge(context.Background(), client(1), offer.Address, dhcp.Renewing); err == nil {
		t.Fatal("minority acknowledged a renewal")
	}
	c.partitioned[old].Store(false)
	for _, n := range c.nodes {
		n.transport.CloseStreams()
	}
	c.consistent()
	// Lose the leader process, including its Bolt and SQLite handles, then
	// restart it from its private disk and catch up without resetting ownership.
	majority = c.leader(-1)
	c.stop(majority)
	newLeader := c.leader(majority)
	if _, err := c.nodes[newLeader].Backend("default").Acknowledge(context.Background(), client(1), offer.Address, dhcp.Renewing); err != nil {
		t.Fatalf("failover renewal lost binding: %v", err)
	}
	c.start(majority)
	c.consistent()
	// Two unavailable voters must fail closed, while repeated subsequent calls
	// remain bounded and the surviving node stays operational for inspection.
	leader := c.leader(-1)
	for i := range c.nodes {
		if i != leader {
			c.stop(i)
		}
	}
	for i := 0; i < 3; i++ {
		start = time.Now()
		_, err := c.nodes[leader].Backend("default").Offer(context.Background(), client(10+i), netip.Addr{})
		if err == nil || time.Since(start) > 3*time.Second {
			t.Fatalf("quorum loss request %d: %v", i, err)
		}
	}
	if c.nodes[leader].Status().Ready {
		t.Fatal("single survivor reports ready")
	}
}

func TestSnapshotCatchupForOfflineVoter(t *testing.T) {
	c := clusterFixture(t)
	c.startAll()
	leader := c.leader(-1)
	offline := (leader + 1) % 3
	c.stop(offline)
	for i := 1; i <= 90; i++ {
		offer := c.offer(leader, i)
		if i%3 == 0 {
			if _, err := c.nodes[leader].Backend("default").Acknowledge(context.Background(), client(i), offer.Address, dhcp.Selecting); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := c.nodes[leader].Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	first, err := c.nodes[leader].disk.FirstIndex()
	if err != nil || first < 2 {
		t.Fatalf("test did not compact its log: %d %v", first, err)
	}
	c.start(offline)
	c.consistent()
}

func TestUnsafeBootstrapAndForeignOwnershipRejected(t *testing.T) {
	c := clusterFixture(t)
	bad := c.configs[1]
	bad.Bootstrap = true
	if err := bad.validate(); err == nil {
		t.Fatal("second node allowed to bootstrap")
	}
	bad = c.configs[0]
	bad.TLS = bad.TLS.Clone()
	bad.TLS.ClientAuth = tls.NoClientCert
	if err := bad.validate(); err == nil {
		t.Fatal("one-way TLS accepted")
	}
	// Standalone ownership on a joining voter must never be discarded by seed.
	s, err := dhcp.OpenLeases(c.paths[1], c.leaseConfigs[1])
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Offer(context.Background(), client(1), netip.Addr{}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s, err = dhcp.OpenReplicaLeases(c.paths[1], c.leaseConfigs[1])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if n, err := Open(c.configs[1], map[string]*dhcp.LeaseStore{"default": s}); err == nil {
		_ = n.Close()
		t.Fatal("joining follower adopted foreign ownership")
	}
	// A previously initialized identity cannot form a fresh cluster if its
	// durable Raft data goes missing.
	c.start(0)
	c.stop(0)
	if err := os.Remove(filepath.Join(c.configs[0].DataDir, "raft.db")); err != nil {
		t.Fatal(err)
	}
	s, err = dhcp.OpenReplicaLeases(c.paths[0], c.leaseConfigs[0])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if n, err := Open(c.configs[0], map[string]*dhcp.LeaseStore{"default": s}); err == nil {
		_ = n.Close()
		t.Fatal("missing durable Raft store bootstrapped a second cluster")
	}
}

func TestBootstrapRetainsLiveLeasesAndAllVotersRestart(t *testing.T) {
	c := clusterFixture(t)
	s, err := dhcp.OpenLeases(c.paths[0], c.leaseConfigs[0])
	if err != nil {
		t.Fatal(err)
	}
	offer, err := s.Offer(context.Background(), client(1), netip.Addr{})
	if err != nil {
		t.Fatal(err)
	}
	bound, err := s.Acknowledge(context.Background(), client(1), offer.Address, dhcp.Selecting)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	c.startAll()
	leader := c.leader(-1)
	c.consistent()
	if renewed, err := c.nodes[leader].Backend("default").Offer(context.Background(), client(1), netip.Addr{}); err != nil || renewed.Address != bound.Address || !renewed.ExpiresAt.Equal(bound.ExpiresAt) {
		t.Fatalf("bootstrap changed live binding: %+v, %v", renewed, err)
	}
	if err := c.nodes[leader].Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := range c.nodes {
		c.stop(i)
	}
	c.startAll()
	leader = c.leader(-1)
	c.consistent()
	if renewed, err := c.nodes[leader].Backend("default").Acknowledge(context.Background(), client(1), bound.Address, dhcp.Renewing); err != nil || renewed.Address != bound.Address {
		t.Fatalf("full restart lost binding: %+v, %v", renewed, err)
	}
	c.consistent()
}

func TestAuthenticatedTransportRejectsIdentityClusterAndTopologyMismatch(t *testing.T) {
	c := clusterFixture(t)
	c.start(0)
	n := c.nodes[0]
	for _, tc := range []struct{ name, node, cluster, fp string }{
		{"unknown voter", "outsider", n.cfg.ClusterID, n.fsm.fingerprint},
		{"wrong cluster", "node2", "other-cluster", n.fsm.fingerprint},
		{"wrong topology", "node2", n.cfg.ClusterID, "different-topology"},
		{"certificate impersonation", "node3", n.cfg.ClusterID, n.fsm.fingerprint},
	} {
		t.Run(tc.name, func(t *testing.T) {
			certID := tc.node
			if tc.name == "certificate impersonation" {
				certID = "node2"
			}
			credentials, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: []byte("01234567890123456789012345678901"), NodeName: certID})
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := TLSFromCredentials(credentials)
			if err != nil {
				t.Fatal(err)
			}
			conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", n.cfg.ListenAddress, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			if err := writeFrame(conn, hello{1, tc.cluster, tc.node, tc.fp, "raft"}); err != nil {
				return
			}
			var response hello
			if err := readFrame(conn, &response, 4096); err == nil {
				t.Fatal("authenticated transport accepted an invalid peer")
			}
		})
	}
	// A TLS client with no client certificate cannot even enter framing.
	cfg := c.configs[1].TLS.Clone()
	cfg.GetClientCertificate = nil
	cfg.Certificates = nil
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", n.cfg.ListenAddress, cfg)
	if err == nil {
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		_ = writeFrame(conn, hello{1, n.cfg.ClusterID, "node2", n.fsm.fingerprint, "raft"})
		var response hello
		if readFrame(conn, &response, 4096) == nil {
			t.Fatal("transport accepted a client without a certificate")
		}
	}
}

func TestReplicaStorageFailureStopsServingAndRecoversByReplay(t *testing.T) {
	c := clusterFixture(t)
	c.startAll()
	leader := c.leader(-1)
	offer := c.offer(leader, 1)
	c.consistent()
	// Closing just this replica's SQLite handle is a reproducible local disk
	// failure. The quorum log remains durable; the node must stop serving and
	// replay that committed command after repair/restart.
	if err := c.stores[leader].Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.nodes[leader].Backend("default").Acknowledge(context.Background(), client(1), offer.Address, dhcp.Selecting); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("failed SQLite replica acknowledged: %v", err)
	}
	eventually(t, 4*time.Second, func() bool { return c.nodes[leader].Status().Error != "" && !c.nodes[leader].Status().Ready }, "fatal storage health status")
	newLeader := c.leader(leader)
	if renewed, err := c.nodes[newLeader].Backend("default").Acknowledge(context.Background(), client(1), offer.Address, dhcp.Renewing); err != nil || renewed.State != "bound" {
		t.Fatalf("quorum lost committed ownership: %+v %v", renewed, err)
	}
	c.stop(leader)
	c.start(leader)
	c.consistent()
}
