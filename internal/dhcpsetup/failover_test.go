package dhcpsetup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dhcp"
)

func setupFailoverFixture() (config.DHCPFailover, config.DHCPScope) {
	f := config.DHCPFailover{Enabled: true, ClusterID: "test", NodeID: "node2", ListenAddress: "127.0.0.1:19672",
		Peers: []config.DHCPFailoverPeer{{ID: "node1", Address: "127.0.0.1:19671"}, {ID: "node2", Address: "127.0.0.1:19672"}, {ID: "node3", Address: "127.0.0.1:19673"}}}
	s := setupTestScope()
	s.ServerIP = "192.0.2.3"
	s.FailoverServers = []config.DHCPScopeServer{{NodeID: "node1", Address: "192.0.2.2"}, {NodeID: "node2", Address: "192.0.2.3"}, {NodeID: "node3", Address: "192.0.2.4"}}
	return f, s
}

func TestSetupFailoverReplicaOverlayAndLocalRestart(t *testing.T) {
	ctx, dir := context.Background(), t.TempDir()
	f, scope := setupFailoverFixture()
	m := setupTestOpen(t, dir, config.DHCP{}, nil)
	saved, err := m.Save(ctx, 1, true, []config.DHCPScope{scope}, false, nil, f)
	if err != nil {
		t.Fatal(err)
	}
	f.Peers[0].Address = "mutated:1"
	if saved.Failover.Peers[0].Address == f.Peers[0].Address {
		t.Fatal("saved failover aliases input")
	}
	_ = m.Close()
	m = setupTestOpen(t, dir, config.DHCP{}, nil)
	cfgs, err := m.Effective().Configs()
	if err != nil {
		t.Fatal(err)
	}
	// A joining replica inherits the cluster's token, not its own UI token.
	store, err := dhcp.OpenReplicaLeases(filepath.Join(dir, "dhcp.db"), cfgs[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seed, err := dhcp.OpenLeases(filepath.Join(t.TempDir(), "dhcp.db"), cfgs[0])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = seed.Close() }()
	data, err := seed.CaptureReplica(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RestoreReplica(ctx, data); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Second)
	if _, err := store.ApplyReplica(ctx, 1, dhcp.ReplicaCommand{Operation: "noop", At: at}); err != nil {
		t.Fatal(err)
	}
	meta, _, err := dhcp.ReadLeaseSettings(ctx, filepath.Join(dir, "dhcp.db"))
	if err != nil {
		t.Fatal(err)
	}
	options := meta.Options
	options.DomainName = "replicated.test"
	options.DomainSearch = []string{"replicated.test", "search.test"}
	options.BootFile = "https://boot.test/efi"
	if _, err := store.ApplyReplica(ctx, 2, dhcp.ReplicaCommand{Operation: "options", At: at, Revision: meta.Revision, Options: options}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "dhcp-raft"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dhcp-raft", "identity.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	view := setupTestRead(t, m)
	if !view.FailoverInitialized || len(view.PendingScopes) != 0 || view.Scopes[0].DomainName != options.DomainName || !reflect.DeepEqual(view.Scopes[0].DomainSearch, options.DomainSearch) || view.Scopes[0].BootFile != options.BootFile {
		t.Fatalf("replica live options hidden by local setup token: %+v", view)
	}
	next, err := m.Save(ctx, view.Revision, view.Enabled, view.Scopes, false, view.ScopeRevisions)
	if err != nil || next.Revision != view.Revision || next.RestartRequired || len(next.PendingScopes) != 0 {
		t.Fatalf("unchanged replica save refreshed token: %+v, %v", next, err)
	}
	changed := cloneScopes(view.Scopes)
	changed[0].Interface = "replacement0"
	next, err = m.Save(ctx, view.Revision, true, changed, false, view.ScopeRevisions)
	if err != nil || !next.RestartRequired || len(next.PendingScopes) != 0 {
		t.Fatalf("local restart edit: %+v, %v", next, err)
	}
	next, err = m.Save(ctx, next.Revision, true, view.Scopes, false, next.ScopeRevisions)
	if err != nil || next.RestartRequired {
		t.Fatalf("local edit revert failed: %+v, %v", next, err)
	}
	for _, edit := range []func(*State){
		func(s *State) { s.Failover.Enabled = false },
		func(s *State) { s.Failover.ClusterID = "other" },
		func(s *State) { s.Scopes = nil; s.Enabled = false },
		func(s *State) { s.Scopes[0].DomainName = "staged.test" },
		func(s *State) { s.Scopes[0].ConflictDetection = "disabled" },
	} {
		candidate := cloneState(next)
		edit(&candidate)
		if _, err := m.Save(ctx, candidate.Revision, candidate.Enabled, candidate.Scopes, false, candidate.ScopeRevisions, candidate.Failover); !errors.Is(err, ErrInvalidConfiguration) {
			t.Fatalf("unsafe established cluster edit accepted: %v", err)
		}
	}
	disabled, err := m.Save(ctx, next.Revision, false, next.Scopes, false, next.ScopeRevisions)
	if err != nil || disabled.Enabled || !disabled.Failover.Enabled {
		t.Fatalf("safe node disable: %+v, %v", disabled, err)
	}
	_ = store.Close()
	_ = m.Close()
	m = setupTestOpen(t, dir, config.DHCP{}, nil)
	if got := m.Effective(); got.Enabled || !got.Failover.Enabled || got.Scopes[0].DomainName != options.DomainName {
		t.Fatalf("restart lost replicated state: %+v", got)
	}
}
