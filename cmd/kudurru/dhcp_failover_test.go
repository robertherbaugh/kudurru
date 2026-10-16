package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dhcp"
)

// DHCP listeners are injected; only authenticated peer TCP on loopback is real.
func TestDHCPFailoverSetupRuntime(t *testing.T) {
	t.Setenv("KUDURRU_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901")))
	t.Setenv("KUDURRU_ENCRYPTION_KEY_PREVIOUS", "")
	ctx := context.Background()
	peers := make([]config.DHCPFailoverPeer, 3)
	servers := make([]config.DHCPScopeServer, 3)
	var listeners [3]net.Listener
	for i := range peers {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		peers[i] = config.DHCPFailoverPeer{ID: fmt.Sprintf("node%d", i+1), Address: listener.Addr().String()}
		listeners[i] = listener
		t.Cleanup(func() { _ = listener.Close() })
		servers[i] = config.DHCPScopeServer{NodeID: peers[i].ID, Address: fmt.Sprintf("192.0.2.%d", i+2)}
	}
	// Release the reserved ports only after all peers have distinct addresses.
	for _, listener := range listeners {
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
	}
	var configs [3]*config.Config
	var nodes [3]*setupAcceptanceRuntime
	for i := range nodes {
		cfg := dhcpTestConfig(t)
		cfg.DHCP = config.DHCP{}
		configs[i] = cfg
		node := startSetupAcceptance(t, cfg, nil)
		scope := setupAcceptanceScope()
		scope.Interface, scope.ServerIP = fmt.Sprintf("fixture%d", i), servers[i].Address
		scope.ConflictDetection, scope.FailoverServers = "disabled", servers
		failover := config.DHCPFailover{Enabled: true, ClusterID: "acceptance", NodeID: peers[i].ID, ListenAddress: peers[i].Address, Peers: peers, Bootstrap: i == 0}
		view := readSetupAcceptance(t, node.setup)
		if _, err := node.setup.Save(ctx, view.Revision, true, []config.DHCPScope{scope}, false, view.ScopeRevisions, failover); err != nil {
			t.Fatal(err)
		}
		node.close()
		nodes[i] = startSetupAcceptance(t, cfg, nil)
		if nodes[i].failure != "" || nodes[i].service == nil {
			t.Fatalf("start node%d: %s", i+1, nodes[i].failure)
		}
	}
	waitLeader := func() int {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			for i, n := range nodes {
				if n != nil {
					s := n.service.Replication()
					if s.Role == "Leader" && s.Ready && s.Initialized {
						return i
					}
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("no ready quorum leader")
		return -1
	}
	leader := waitLeader()
	follower := (leader + 1) % 3
	store := setupAcceptanceStore(t, nodes[follower].service, "default")
	// A follower may still be applying the seed after the leader is ready.
	deadline := time.Now().Add(10 * time.Second)
	for {
		view := readSetupAcceptance(t, nodes[follower].setup)
		if len(view.PendingScopes) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("follower setup never exposed committed replica")
		}
		time.Sleep(20 * time.Millisecond)
	}
	options := setupAcceptanceOptions(store.Configuration().Config)
	options.DomainName, options.BootFile = "replicated.home.arpa", "https://boot.test/efi"
	options.DomainSearch = []string{"replicated.home.arpa", "search.test"}
	if err := nodes[follower].service.UpdateOptions(ctx, "default", store.Configuration().Revision, options); err != nil {
		t.Fatal(err)
	}
	if got := store.Configuration().Config; got.DomainName != options.DomainName || got.BootFile != options.BootFile {
		t.Fatal("forwarded write returned before local SQLite applied")
	}
	called := false
	err := nodes[follower].service.MutateScope(ctx, "default", func(context.Context, *dhcp.LeaseStore) error { called = true; return nil })
	if !errors.Is(err, dhcp.ErrReplicatedMutation) || called {
		t.Fatalf("consensus callback bypass: %v, called=%v", err, called)
	}
	if _, err := store.Offer(ctx, dhcp.Client{MAC: "02:00:00:00:00:09"}, netip.Addr{}); !errors.Is(err, dhcp.ErrReplicaRequired) {
		t.Fatalf("local allocator bypass: %v", err)
	}
	expires := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	doc := dhcp.LeaseDocument{Version: 1, Scope: "default", Subnet: netip.MustParsePrefix("192.0.2.0/24"), ExportedAt: time.Now(),
		Leases: []dhcp.LeaseRecord{{Address: netip.MustParseAddr("192.0.2.100"), Identity: dhcp.LeaseIdentity{Kind: "mac", Value: "02:00:00:00:00:01"}, MAC: "02:00:00:00:00:01", Hostname: "retained", ExpiresAt: expires, State: "bound"}}}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := nodes[follower].service.Import(ctx, "default", "native", data); err != nil || result.Imported != 1 {
		t.Fatalf("forwarded import: %+v %v", result, err)
	}
	nodes[leader].close()
	nodes[leader] = nil
	newLeader := waitLeader()
	if newLeader == leader {
		t.Fatal("closed leader retained authority")
	}
	nodes[leader] = startSetupAcceptance(t, configs[leader], nil)
	if nodes[leader].failure != "" {
		t.Fatal(nodes[leader].failure)
	}
	// Full process restart must load live replicated settings, never replay the
	// independently generated UI seeds or erase already committed client state.
	for _, node := range nodes {
		node.close()
	}
	for i := range nodes {
		nodes[i] = startSetupAcceptance(t, configs[i], nil)
		if nodes[i].failure != "" {
			t.Fatal(nodes[i].failure)
		}
	}
	waitLeader()
	for i, node := range nodes {
		store := setupAcceptanceStore(t, node.service, "default")
		deadline := time.Now().Add(10 * time.Second)
		for {
			leases, err := store.List(ctx)
			if err == nil && len(leases) == 1 && leases[0].ExpiresAt.Equal(expires) && store.Configuration().Config.DomainName == options.DomainName {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("node%d restart lost settings/lease: %+v %v", i+1, leases, err)
			}
			time.Sleep(20 * time.Millisecond)
		}
		view := readSetupAcceptance(t, node.setup)
		if !view.FailoverInitialized || len(view.PendingScopes) != 0 || view.Scopes[0].Interface != fmt.Sprintf("fixture%d", i) || view.Scopes[0].ServerIP != servers[i].Address {
			t.Fatalf("replication changed local topology or pending state: %+v", view)
		}
	}
}

func TestDHCPFailoverTLSAndIndependentAllocatorGuard(t *testing.T) {
	t.Setenv("KUDURRU_ENCRYPTION_KEY", "")
	t.Setenv("KUDURRU_ENCRYPTION_KEY_PREVIOUS", "")
	if _, err := dhcpFailoverTLS(config.DHCPFailover{NodeID: "node1"}); err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("missing credentials accepted: %v", err)
	}
	cfg := dhcpTestConfig(t)
	if err := os.MkdirAll(filepath.Join(cfg.Node.DataDir, "dhcp-raft"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.Node.DataDir, "dhcp-raft", "identity.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	service, closeService, err := newDHCP(cfg, nil)
	closeService()
	if err == nil || service != nil || !strings.Contains(err.Error(), "independent allocator") {
		t.Fatalf("established node escaped quorum: %v", err)
	}
	cfg.DHCP.Enabled = false
	service, closeService, err = newDHCP(cfg, nil)
	closeService()
	if err != nil || service != nil {
		t.Fatalf("safe node disable rejected: %v", err)
	}
}
