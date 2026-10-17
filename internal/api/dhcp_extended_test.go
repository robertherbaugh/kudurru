package api

import (
	"reflect"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dhcp"
)

func TestDHCPExtendedOptionsAPI(t *testing.T) {
	h, store := dhcpAPIHarness(t, "api")
	before := dhcpAPIConfig(t, h)
	opts := before.Options
	opts.NTPServers = []string{"192.0.2.5"}
	opts.DomainSearch = []string{"home.arpa", "search.test"}
	opts.ClasslessRoutes = []config.DHCPClasslessRoute{{Destination: "0.0.0.0/0", Router: "192.0.2.1"}, {Destination: "198.51.100.0/24", Router: "192.0.2.6"}}
	opts.BootServer, opts.BootServerName, opts.BootFile = "192.0.2.5", "boot.test", "netboot.efi"
	opts.BootProfiles = []config.DHCPBootProfile{{Architecture: 16, BootFile: "https://boot.test/image.efi"}}
	path := BasePath + dhcpBase + "/scopes/default/config"
	var after DHCPConfig
	h.do("PUT", path, "admin", opts, "If-Match", before.ETag).want(200).json(&after)
	if !reflect.DeepEqual(opts, after.Options) {
		t.Fatalf("advanced options lost: want %+v, got %+v", opts, after.Options)
	}
	got := store.Configuration().Config
	if len(got.ClasslessRoutes) != 2 || got.BootProfiles[0].Architecture != 16 || got.DomainSearch[1] != "search.test" {
		t.Fatal("advanced response did not match committed allocator")
	}
	opts.ClasslessRoutes[0].Destination = "::/0"
	h.do("PUT", path, "admin", opts, "If-Match", after.ETag).want(400)
	if store.Configuration().Revision != 2 {
		t.Fatal("invalid route mutated persisted options")
	}
	h.srv.opts.DHCP.SetReplicationStatus(func() dhcp.ReplicationStatus {
		return dhcp.ReplicationStatus{Enabled: true, NodeID: "node1", ClusterID: "test", Role: "Follower", LeaderID: "node2", Initialized: true, AppliedIndex: 10, CommitIndex: 10}
	})
	var status DHCPStatus
	h.do("GET", BasePath+dhcpBase+"/status", "viewer", nil).want(200).json(&status)
	if status.Failover == nil || status.Failover.Role != "Follower" || status.Failover.AppliedIndex != 10 {
		t.Fatalf("quorum status missing: %+v", status)
	}
}

func TestDHCPExtendedSetupAndFailoverPreservation(t *testing.T) {
	h, _, _ := newDHCPSetupHarness(t, config.DHCP{}, nil)
	scope := testDHCPSetupScope()
	scope.LeaseDuration = "12h0m0s"
	scope.ConflictDetection, scope.ProbeTimeout = "icmp", "250ms"
	scope.TrustedRelays = []DHCPTrustedRelay{{SourceIP: "198.51.100.1", GatewayIP: "192.0.2.1", SourcePort: 1067, LinkSelection: "192.0.2.1", ServerIdentifier: "192.0.2.9"}}
	scope.NTPServers, scope.DomainSearch = []string{"192.0.2.5"}, []string{"home.arpa"}
	scope.ClasslessRoutes = []config.DHCPClasslessRoute{{Destination: "0.0.0.0/0", Router: "192.0.2.1"}}
	scope.BootProfiles = []config.DHCPBootProfile{{Architecture: 7, BootServer: "192.0.2.5", BootFile: "boot.efi"}}
	scope.FailoverServers = []config.DHCPScopeServer{{NodeID: "node1", Address: "192.0.2.2"}, {NodeID: "node2", Address: "192.0.2.3"}, {NodeID: "node3", Address: "192.0.2.4"}}
	failover := config.DHCPFailover{Enabled: true, ClusterID: "test", NodeID: "node1", ListenAddress: "127.0.0.1:19671", Bootstrap: true,
		Peers: []config.DHCPFailoverPeer{{ID: "node1", Address: "127.0.0.1:19671"}, {ID: "node2", Address: "127.0.0.1:19672"}, {ID: "node3", Address: "127.0.0.1:19673"}}}
	initial := getDHCPSetup(t, h)
	body := DHCPSetupRequest{Scopes: []DHCPSetupScope{scope}, ScopeRevisions: map[string]string{"lan": "0"}, Failover: &failover}
	var saved DHCPSetup
	h.do("PUT", dhcpSetupPath, "admin", body, "If-Match", initial.ETag).want(200).json(&saved)
	if len(saved.Scopes) != 1 || !reflect.DeepEqual(saved.Scopes[0], scope) || !reflect.DeepEqual(saved.Failover, failover) {
		t.Fatalf("saved extended setup differs: %+v", saved)
	}
	body.Failover, body.Enabled = nil, true
	h.do("PUT", dhcpSetupPath, "admin", body, "If-Match", saved.ETag).want(200).json(&saved)
	if !saved.Failover.Enabled || !saved.Failover.Bootstrap || len(saved.Failover.Peers) != 3 {
		t.Fatalf("legacy enabled toggle dropped failover: %+v", saved)
	}
	body.Scopes[0].TrustedRelays[0].SourcePort = 68
	h.do("PUT", dhcpSetupPath, "admin", body, "If-Match", saved.ETag).want(400)
}
