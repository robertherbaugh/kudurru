package dhcp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/netip"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func replicaTestOpen(t *testing.T, cfg Config) (*LeaseStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "leases.db")
	s, err := OpenReplicaLeases(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}
func replicaTestState(t *testing.T, s *LeaseStore) replicaSnapshot {
	t.Helper()
	b, err := s.CaptureReplica(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var snapshot replicaSnapshot
	if err := json.Unmarshal(b, &snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}
func replicaTestApply(t *testing.T, s *LeaseStore, index uint64, cmd ReplicaCommand) ReplicaResult {
	t.Helper()
	r, err := s.ApplyReplica(context.Background(), index, cmd)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestReplicaAppliedIndexDoesNotWaitForTransaction(t *testing.T) {
	for _, failure := range []string{"none", "rollback", "commit"} {
		t.Run(failure, func(t *testing.T) {
			s, _ := replicaTestOpen(t, managedTestConfig())
			if got := s.ReplicaAppliedIndex(); got != 0 {
				t.Fatalf("new store index = %d", got)
			}
			at := time.Now().UTC().Truncate(time.Second)
			replicaTestApply(t, s, 1, ReplicaCommand{Operation: "noop", At: at})
			if failure == "commit" {
				leaseTestSQL(t, s, `CREATE TABLE progress_parent(id INTEGER PRIMARY KEY); CREATE TABLE progress_child(parent INTEGER REFERENCES progress_parent(id) DEFERRABLE INITIALLY DEFERRED); CREATE TRIGGER fail_progress AFTER UPDATE ON lease_replica BEGIN INSERT INTO progress_child VALUES(1); END`)
			}
			entered, resume := make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(resume) })
			defer release()
			finished := make(chan error, 1)
			rollback := errors.New("injected transaction rollback")
			ctx := context.WithValue(context.Background(), replicaCommandKey{}, &replicaCommandContext{index: 2, command: ReplicaCommand{Operation: "noop", At: at}})
			go func() {
				finished <- s.transactFast(ctx, func(tx *sql.Tx, _ []storedLease, _ time.Time) error {
					// Write durable metadata inside a real transaction, then pause
					// while holding the transaction mutex before any COMMIT.
					if _, err := leaseExec(s.transactionContext(ctx), tx, "UPDATE lease_replica SET applied_index=2 WHERE singleton=1"); err != nil {
						return err
					}
					close(entered)
					<-resume
					if failure == "rollback" {
						return rollback
					}
					return nil
				})
			}()
			select {
			case <-entered:
			case err := <-finished:
				t.Fatalf("transaction did not reach pause: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("transaction did not start")
			}
			read := make(chan uint64, 1)
			go func() { read <- s.ReplicaAppliedIndex() }()
			select {
			case got := <-read:
				if got != 1 {
					t.Fatalf("uncommitted progress became visible: %d", got)
				}
			case <-time.After(time.Second):
				t.Fatal("applied index read blocked behind transaction")
			}
			release()
			err := <-finished
			want := uint64(1)
			switch failure {
			case "none":
				if err != nil {
					t.Fatal(err)
				}
				want = 2
			case "rollback":
				if !errors.Is(err, rollback) {
					t.Fatalf("expected rollback: %v", err)
				}
			case "commit":
				var storage *ReplicaStorageError
				if !errors.As(err, &storage) {
					t.Fatalf("expected failed COMMIT: %v", err)
				}
			}
			if got := s.ReplicaAppliedIndex(); got != want {
				t.Fatalf("committed progress = %d, want %d", got, want)
			}
			if durable, _ := s.ReplicaInfo(); durable != want {
				t.Fatalf("ReplicaInfo progress = %d, want %d", durable, want)
			}
		})
	}
}

func TestReplicaDeterministicClockClaimsAndRestoreLocalIdentity(t *testing.T) {
	ctx := context.Background()
	cfg := managedTestConfig()
	cfg.FailoverServers = []ScopeServer{{NodeID: "a", Address: netip.MustParseAddr("192.0.2.1")}, {NodeID: "b", Address: netip.MustParseAddr("192.0.2.2")}, {NodeID: "c", Address: netip.MustParseAddr("192.0.2.3")}}
	first, _ := replicaTestOpen(t, cfg)
	cfg.Interface = "eth1"
	cfg.ServerIP = cfg.FailoverServers[1].Address
	second, _ := replicaTestOpen(t, cfg)
	first.now = func() time.Time { panic("replica apply read local clock") }
	second.now = first.now
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	client := leaseTestClient(1)
	client.ID = string([]byte{0xff, 0, 0x80})
	client.Hostname = "same"
	offer := ReplicaCommand{Operation: "offer", At: at, Client: client}
	a := replicaTestApply(t, first, 10, offer)
	b := replicaTestApply(t, second, 10, offer)
	if a != b {
		t.Fatalf("replicas allocated differently: %+v %+v", a, b)
	}
	ack := ReplicaCommand{Operation: "acknowledge", At: at, Client: client, Address: a.Lease.Address, Mode: Selecting}
	replicaTestApply(t, first, 11, ack)
	replicaTestApply(t, second, 11, ack)
	r1, _ := first.DNS().Address(a.Lease.Address)
	r2, _ := second.DNS().Address(a.Lease.Address)
	if r1 != r2 || r1.ClaimTicket != 11 || r1.ClaimEpoch == 0 {
		t.Fatalf("replica claim diverged: %+v %+v", r1, r2)
	}
	opts := optionsFor(first.Configuration().Config)
	opts.DomainName = "restored.test"
	replicaTestApply(t, first, 12, ReplicaCommand{Operation: "options", At: at, Revision: 1, Options: opts})
	snapshot, err := first.CaptureReplica(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Restore includes expired leases; local 2026 wall time cannot remove them.
	if err := second.RestoreReplica(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	state := replicaTestState(t, second)
	if state.Config.Interface != "eth1" || state.Config.ServerIP != cfg.ServerIP || len(state.Leases) != 1 || state.AppliedIndex != 12 {
		t.Fatalf("restore replaced local identity or expired history: %+v", state)
	}
	if effective := second.Configuration(); effective.Revision != 2 || effective.Config.DomainName != "restored.test" || !reflect.DeepEqual(effective.Config, state.Config) {
		t.Fatalf("restore did not publish committed configuration: %+v", effective)
	}
	if decoded, err := importedLease(state.Leases[0].Record); err != nil || decoded.ClientID != client.ID {
		t.Fatalf("opaque identity lost: %+v %v", decoded, err)
	}
	if _, err := second.Offer(ctx, client, netip.Addr{}); !errors.Is(err, ErrReplicaRequired) {
		t.Fatalf("local mutation bypassed consensus: %v", err)
	}
}

func TestReplicaReplayRejectionAndWriteCommitFailure(t *testing.T) {
	ctx := context.Background()
	s, path := replicaTestOpen(t, managedTestConfig())
	at := time.Now().UTC().Truncate(time.Second)
	client := leaseTestClient(1)
	offered := replicaTestApply(t, s, 1, ReplicaCommand{Operation: "offer", At: at, Client: client})
	before := replicaTestState(t, s)
	view := s.DNS()
	wrong := leaseTestClient(2)
	_, err := s.ApplyReplica(ctx, 2, ReplicaCommand{Operation: "quarantine", At: at, Client: wrong, Address: offered.Lease.Address})
	if !errors.Is(err, ErrUnknownLease) {
		t.Fatalf("domain rejection lost: %v", err)
	}
	if index, _ := s.ReplicaInfo(); index != 2 || s.ReplicaAppliedIndex() != 2 {
		t.Fatal("rejected command index not durable")
	}
	if s.DNS() != view {
		t.Fatal("rejection published a new DNS view")
	}
	replay := ReplicaCommand{Operation: "release", At: at, Client: client, Address: offered.Lease.Address}
	replicaTestApply(t, s, 1, replay)
	after := replicaTestState(t, s)
	if !reflect.DeepEqual(before.Leases, after.Leases) {
		t.Fatal("replay mutated lease")
	}
	leaseTestSQL(t, s, `CREATE TABLE replica_parent(id INTEGER PRIMARY KEY); CREATE TABLE replica_child(parent INTEGER REFERENCES replica_parent(id) DEFERRABLE INITIALLY DEFERRED); CREATE TRIGGER fail_replica AFTER UPDATE ON leases BEGIN INSERT INTO replica_child VALUES(1); END`)
	_, err = s.ApplyReplica(ctx, 3, ReplicaCommand{Operation: "acknowledge", At: at, Client: client, Address: offered.Lease.Address, Mode: Selecting})
	var storage *ReplicaStorageError
	if !errors.As(err, &storage) {
		t.Fatalf("commit failure not fatal storage error: %v", err)
	}
	if index, _ := s.ReplicaInfo(); index != 2 || s.ReplicaAppliedIndex() != 2 {
		t.Fatal("failed COMMIT advanced applied index")
	}
	if s.DNS() != view {
		t.Fatal("failed COMMIT published")
	}
	leaseTestSQL(t, s, "DROP TRIGGER fail_replica")
	replicaTestApply(t, s, 3, ReplicaCommand{Operation: "acknowledge", At: at, Client: client, Address: offered.Lease.Address, Mode: Selecting})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if standalone, err := OpenLeases(path, managedTestConfig()); !errors.Is(err, ErrReplicaRequired) {
		if standalone != nil {
			_ = standalone.Close()
		}
		t.Fatalf("standalone split brain accepted: %v", err)
	}
	reopened, err := OpenReplicaLeases(path, managedTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if index, rows := reopened.ReplicaInfo(); index != 3 || !rows || reopened.ReplicaAppliedIndex() != 3 {
		t.Fatal("applied index or binding lost on restart")
	}
	settings, _, err := ReadLeaseSettings(ctx, path)
	if err != nil || settings.ReplicaIndex != 3 {
		t.Fatalf("read-only metadata omitted replica index: %+v %v", settings, err)
	}
}

func TestReplicaSnapshotRestoreRollbackAndTopology(t *testing.T) {
	ctx := context.Background()
	s, _ := replicaTestOpen(t, managedTestConfig())
	at := time.Now().UTC().Truncate(time.Second)
	replicaTestApply(t, s, 1, ReplicaCommand{Operation: "offer", At: at, Client: leaseTestClient(1)})
	snapshot, err := s.CaptureReplica(ctx)
	if err != nil {
		t.Fatal(err)
	}
	replicaTestApply(t, s, 2, ReplicaCommand{Operation: "offer", At: at, Client: leaseTestClient(2)})
	before, err := s.CaptureReplica(ctx)
	if err != nil {
		t.Fatal(err)
	}
	view := s.DNS()
	configuration := s.management.Load()
	leaseTestSQL(t, s, `CREATE TRIGGER fail_restore BEFORE DELETE ON leases BEGIN SELECT RAISE(ABORT,'injected restore failure'); END`)
	if err := s.RestoreReplica(ctx, snapshot); err == nil {
		t.Fatal("failed restore succeeded")
	}
	after, err := s.CaptureReplica(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || s.DNS() != view || s.ReplicaAppliedIndex() != 2 || s.management.Load() != configuration {
		t.Fatal("failed restore changed committed state")
	}
	leaseTestSQL(t, s, "DROP TRIGGER fail_restore")
	var malformed replicaSnapshot
	if err := json.Unmarshal(snapshot, &malformed); err != nil {
		t.Fatal(err)
	}
	malformed.Config.Subnet = netip.MustParsePrefix("198.51.100.0/24")
	data, _ := json.Marshal(malformed)
	if err := s.RestoreReplica(ctx, data); err == nil {
		t.Fatal("foreign topology restored")
	}
	if err := s.RestoreReplica(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	if index, rows := s.ReplicaInfo(); index != 1 || !rows || s.ReplicaAppliedIndex() != 1 {
		t.Fatal("snapshot not installed")
	}
}

func TestReplicaOpenPreservesExpiredRowsAndSeedOrder(t *testing.T) {
	cfg := managedTestConfig()
	standalone, path := leaseTestOpen(t, cfg)
	older := leaseTestClient(1)
	older.Hostname = "shared"
	bound := leaseTestBind(t, standalone, older)
	oldClaim, _ := standalone.DNS().Address(bound.Address)
	if err := standalone.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := OpenReplicaLeases(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	at := time.Now().UTC().Truncate(time.Second)
	newer := leaseTestClient(2)
	newer.Hostname = "shared"
	offer := replicaTestApply(t, s, 1, ReplicaCommand{Operation: "offer", At: at, Client: newer})
	replicaTestApply(t, s, 2, ReplicaCommand{Operation: "acknowledge", At: at, Client: newer, Address: offer.Lease.Address, Mode: Selecting})
	next, _ := s.DNS().Address(offer.Lease.Address)
	if next.ClaimEpoch <= oldClaim.ClaimEpoch {
		t.Fatal("new Raft log could outrank existing standalone history")
	}
	snapshot := replicaTestState(t, s)
	snapshot.Leases[0].Record.ExpiresAt = time.Unix(10, 0).UTC()
	data, _ := json.Marshal(snapshot)
	if err := s.RestoreReplica(context.Background(), data); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenReplicaLeases(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if got := replicaTestState(t, s); len(got.Leases) != 2 || got.Leases[0].Record.ExpiresAt.Unix() != 10 {
		t.Fatal("local startup clock pruned replicated history")
	}
}

func TestReplicaBootstrapStagedSetupOnlyBeforeConsensus(t *testing.T) {
	cfg := managedTestConfig()
	path := filepath.Join(t.TempDir(), "leases.db")
	s, err := OpenReplicaLeasesForSetup(path, cfg, "seed-token", 0)
	if err != nil {
		t.Fatal(err)
	}
	settings, _, err := ReadLeaseSettings(context.Background(), path)
	if err != nil || settings.AppliedSetupToken != "seed-token" || settings.Revision != 1 {
		t.Fatalf("staged seed missing: %+v %v", settings, err)
	}
	replicaTestApply(t, s, 1, ReplicaCommand{Operation: "noop", At: time.Now().UTC().Truncate(time.Second)})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if opened, err := OpenReplicaLeasesForSetup(path, cfg, "seed-token", 1); !errors.Is(err, ErrReplicaRequired) {
		if opened != nil {
			_ = opened.Close()
		}
		t.Fatalf("locally restaged consensus-owned settings: %v", err)
	}
	opened, err := OpenReplicaLeases(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
}

func TestReplicaImportUsesDurableOrderWithinOneLogEntry(t *testing.T) {
	ctx := context.Background()
	cfg := managedTestConfig()
	s, _ := replicaTestOpen(t, cfg)
	at := time.Now().UTC().Truncate(time.Second)
	older, newer := leaseTestClient(9), leaseTestClient(1)
	older.Hostname = "same"
	newer.Hostname = "same"
	document := LeaseDocument{Version: 1, Scope: s.Configuration().Config.Name, Subnet: cfg.Subnet, Leases: []LeaseRecord{
		{Address: cfg.RangeStart, Identity: LeaseIdentity{Kind: "mac", Value: newer.MAC}, MAC: newer.MAC, Hostname: "same", ExpiresAt: at.Add(time.Hour), State: "bound", DNSClaim: &DNSClaim{Name: "same.leases.test.", Ticket: 22}},
		{Address: cfg.RangeEnd, Identity: LeaseIdentity{Kind: "mac", Value: older.MAC}, MAC: older.MAC, Hostname: "same", ExpiresAt: at.Add(time.Hour), State: "bound", DNSClaim: &DNSClaim{Name: "same.leases.test.", Ticket: 11}},
	}}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	replicaTestApply(t, s, 100, ReplicaCommand{Operation: "import", At: at, Format: "native", Data: data})
	first, _ := s.DNS().Address(cfg.RangeEnd)
	second, _ := s.DNS().Address(cfg.RangeStart)
	if first.ClaimTicket != 100 || second.ClaimTicket != 100 || first.ClaimOrdinal >= second.ClaimOrdinal {
		t.Fatalf("within-command precedence lost: older=%+v newer=%+v", first, second)
	}
	snapshot, err := s.CaptureReplica(ctx)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := replicaTestOpen(t, cfg)
	if err := other.RestoreReplica(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	restored, _ := other.DNS().Address(cfg.RangeEnd)
	if restored != first {
		t.Fatal("snapshot lost within-command rank")
	}
}

func TestLeaseGrantedDurationUsesCommittedDecisionAndIsNotPersisted(t *testing.T) {
	ctx := context.Background()
	cfg := managedTestConfig()
	s, _ := replicaTestOpen(t, cfg)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	c := leaseTestClient(1)
	offer := replicaTestApply(t, s, 1, ReplicaCommand{Operation: "offer", At: at, Client: c}).Lease
	if offer.GrantedDuration != cfg.LeaseDuration {
		t.Fatalf("offer grant=%s, want %s", offer.GrantedDuration, cfg.LeaseDuration)
	}
	original := replicaTestApply(t, s, 2, ReplicaCommand{Operation: "acknowledge", At: at, Client: c, Address: offer.Address, Mode: Selecting}).Lease
	options := optionsFor(s.Configuration().Config)
	options.LeaseDuration = time.Minute
	replicaTestApply(t, s, 3, ReplicaCommand{Operation: "options", At: at.Add(time.Hour), Revision: 1, Options: options})
	renewed := replicaTestApply(t, s, 4, ReplicaCommand{Operation: "acknowledge", At: at.Add(time.Hour), Client: c, Address: offer.Address, Mode: Renewing}).Lease
	if renewed.ExpiresAt != original.ExpiresAt || renewed.GrantedDuration != original.ExpiresAt.Sub(at.Add(time.Hour)) {
		t.Fatalf("grant lost committed lower bound: %+v original=%+v", renewed, original)
	}
	s.now = func() time.Time { return at.Add(time.Hour) }
	rows, err := s.List(ctx)
	if err != nil || rows[0].GrantedDuration != 0 {
		t.Fatalf("response grant leaked into persisted lease: %+v %v", rows, err)
	}
	snapshot, err := s.CaptureReplica(ctx)
	if err != nil {
		t.Fatal(err)
	}
	document, err := s.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	native, _ := json.Marshal(document)
	if bytes.Contains(snapshot, []byte("grantedDuration")) || bytes.Contains(native, []byte("grantedDuration")) {
		t.Fatal("response grant leaked into durable transfer")
	}
}
