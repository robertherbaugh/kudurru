package dhcp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/netip"
	"reflect"
	"strings"
	"time"
)

// ErrReplicaRequired prevents bypassing consensus on a replica-owned store.
var ErrReplicaRequired = errors.New("dhcp: lease mutation requires replicated command")

// ReplicaStorageError marks a fatal local persistence failure during apply.
// Deterministic command validation and ownership errors have their usual types.
type ReplicaStorageError struct{ Err error }

func (e *ReplicaStorageError) Error() string { return e.Err.Error() }
func (e *ReplicaStorageError) Unwrap() error { return e.Err }
func replicaStorage(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if replicaCommandFrom(ctx) == nil {
		return err
	}
	var storage *ReplicaStorageError
	if errors.As(err, &storage) {
		return err
	}
	return &ReplicaStorageError{err}
}

// ReplicaCommand contains every decision input supplied by the consensus leader.
// At is UTC at second precision; index, not a replica clock, orders new claims.
type ReplicaCommand struct {
	Operation   string
	At          time.Time
	Client      Client
	Address     netip.Addr
	Mode        RequestMode
	Revision    uint64
	Options     PoolOptions
	Reservation Reservation
	MAC, Format string
	Data        []byte
}

// ReplicaResult contains the committed response for a replicated lease command.
type ReplicaResult struct {
	Lease  Lease
	Import ImportResult
}
type replicaCommandContext struct {
	index   uint64
	command ReplicaCommand
}
type replicaCommandKey struct{}

func replicaCommandFrom(ctx context.Context) *replicaCommandContext {
	v, _ := ctx.Value(replicaCommandKey{}).(*replicaCommandContext)
	return v
}

// OpenReplicaLeases opens without expiring rows or applying local pool defaults
// over persisted settings. Only ApplyReplica may mutate the resulting store.
func OpenReplicaLeases(path string, cfg Config) (*LeaseStore, error) {
	return openLeasesMode(path, cfg, nil, true)
}

// ReplicaInfo reads committed metadata only. hasRows includes expired rows,
// offers, and quarantines so an unadopted follower cannot discard local history.
func (s *LeaseStore) ReplicaInfo() (appliedIndex uint64, hasRows bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appliedIndex, len(s.leaseByIP) > 0
}

// ReplicaAppliedIndex reads committed progress without locking or database I/O.
// The index becomes visible after the transaction and its memory publication
// finish. Snapshot restore may replace it with an earlier committed index.
func (s *LeaseStore) ReplicaAppliedIndex() uint64 { return s.replicaApplied.Load() }

// ApplyReplica serializes deterministic commands and their durable applied index.
// Replayed indexes are no-ops; the caller must not need their historical result.
func (s *LeaseStore) ApplyReplica(ctx context.Context, index uint64, cmd ReplicaCommand) (ReplicaResult, error) {
	s.replicaMu.Lock()
	defer s.replicaMu.Unlock()
	s.mu.Lock()
	if !s.replica {
		s.mu.Unlock()
		return ReplicaResult{}, ErrReplicaRequired
	}
	if s.closed {
		s.mu.Unlock()
		return ReplicaResult{}, &ReplicaStorageError{sql.ErrConnDone}
	}
	if index <= s.appliedIndex {
		s.mu.Unlock()
		return ReplicaResult{}, nil
	}
	s.mu.Unlock()
	if index > math.MaxInt64 {
		return ReplicaResult{}, fmt.Errorf("dhcp: replica index out of range")
	}
	cmd.At = cmd.At.UTC()
	ctx = context.WithValue(ctx, replicaCommandKey{}, &replicaCommandContext{index: index, command: cmd})
	var result ReplicaResult
	var err error
	if cmd.At.Unix() <= 0 || cmd.At.Nanosecond() != 0 || cmd.At.Year() > 9999 {
		err = fmt.Errorf("dhcp: invalid replica command time")
	} else {
		switch cmd.Operation {
		case "offer":
			result.Lease, err = s.Offer(ctx, cmd.Client, cmd.Address)
		case "acknowledge":
			result.Lease, err = s.Acknowledge(ctx, cmd.Client, cmd.Address, cmd.Mode)
		case "release":
			err = s.Release(ctx, cmd.Client, cmd.Address)
		case "decline":
			err = s.Decline(ctx, cmd.Client, cmd.Address)
		case "quarantine":
			err = s.QuarantineOffer(ctx, cmd.Client, cmd.Address)
		case "options":
			err = s.UpdateOptions(ctx, cmd.Revision, cmd.Options)
		case "put-reservation":
			err = s.PutReservation(ctx, cmd.Revision, cmd.Reservation)
		case "delete-reservation":
			err = s.DeleteReservation(ctx, cmd.Revision, cmd.MAC)
		case "import":
			result.Import, err = s.Import(ctx, cmd.Format, cmd.Data)
		case "noop":
			err = s.transactFast(ctx, func(*sql.Tx, []storedLease, time.Time) error { return nil })
		default:
			err = fmt.Errorf("dhcp: unknown replicated operation %q", cmd.Operation)
		}
	}
	if err != nil {
		var storage *ReplicaStorageError
		if errors.As(err, &storage) {
			return ReplicaResult{}, err
		}
		// A rejected command is part of consensus history too. Its rolled-back
		// transaction publishes nothing, and this transaction advances only index.
		if persist := s.recordReplicaRejection(ctx, index); persist != nil {
			return ReplicaResult{}, persist
		}
		return ReplicaResult{}, err
	}
	return result, nil
}
func (s *LeaseStore) recordReplicaRejection(ctx context.Context, index uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return replicaStorage(ctx, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, "UPDATE lease_replica SET applied_index=? WHERE singleton=1", index); err != nil {
		return replicaStorage(ctx, err)
	}
	if err = tx.Commit(); err != nil {
		return replicaStorage(ctx, err)
	}
	s.appliedIndex = index
	s.replicaApplied.Store(index)
	// A failed SQL statement can increment total_changes before rollback. Leave
	// cacheChanges untouched so the next command verifies authoritative rows.
	return nil
}

type replicaLease struct {
	Record LeaseRecord `json:"record"`
	Claim  DNSClaim    `json:"claim"`
}
type replicaSnapshot struct {
	Version      int            `json:"version"`
	Config       Config         `json:"config"`
	Revision     uint64         `json:"revision"`
	SetupToken   string         `json:"setupToken"`
	AppliedIndex uint64         `json:"appliedIndex"`
	Epoch        uint64         `json:"epoch"`
	Leases       []replicaLease `json:"leases"`
}

func leaseRecord(l storedLease) LeaseRecord {
	identity := LeaseIdentity{Kind: "mac", Value: l.MAC}
	if strings.HasPrefix(l.key, "id:") {
		identity = LeaseIdentity{Kind: "client-id", Value: base64.StdEncoding.EncodeToString([]byte(l.ClientID))}
	}
	return LeaseRecord{Address: l.Address, Identity: identity, MAC: l.MAC, Hostname: l.Hostname, ExpiresAt: l.ExpiresAt, State: l.State}
}

// CaptureReplica takes a complete transactionally consistent snapshot without
// pruning by the local clock. Opaque client IDs remain base64 encoded.
func (s *LeaseStore) CaptureReplica(ctx context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, sql.ErrConnDone
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := readStoredLeases(ctx, tx)
	if err != nil {
		return nil, err
	}
	snapshot := replicaSnapshot{Version: 1, Config: cloneLeaseConfig(s.cfg), Revision: s.revision, AppliedIndex: s.appliedIndex, Epoch: s.replicaEpoch, Leases: make([]replicaLease, 0, len(rows))}
	var token string
	err = tx.QueryRowContext(ctx, "SELECT setup_token FROM lease_settings WHERE singleton=1").Scan(&token)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	snapshot.SetupToken = token
	for _, l := range rows {
		snapshot.Leases = append(snapshot.Leases, replicaLease{Record: leaseRecord(l), Claim: DNSClaim{Name: l.claimName, Epoch: l.claimEpoch, Ticket: l.claimTicket, Ordinal: l.claimOrdinal}})
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return nil, err
	}
	if len(data) > 256<<20 {
		return nil, fmt.Errorf("dhcp: replica snapshot too large")
	}
	return data, nil
}
func replicaTopology(cfg Config) Config {
	cfg = cloneLeaseConfig(cfg)
	cfg.Interface = ""
	cfg.ServerIP = netip.Addr{}
	cfg.RangeStart = netip.Addr{}
	cfg.RangeEnd = netip.Addr{}
	cfg.Router = netip.Addr{}
	cfg.DNSServers = nil
	cfg.DomainName = ""
	cfg.LeaseDuration = 0
	cfg.RegisterDNS = false
	cfg.DNSTTL = 0
	cfg.Reservations = nil
	cfg.NTPServers = nil
	cfg.DomainSearch = nil
	cfg.ClasslessRoutes = nil
	cfg.BootServer = netip.Addr{}
	cfg.BootServerName = ""
	cfg.BootFile = ""
	cfg.BootProfiles = nil
	return cfg
}

// RestoreReplica atomically installs rows, settings, claim order and index.
// Local interface/server address remain local; immutable common topology must
// match. No expiry decision is made until a subsequent replicated command.
func (s *LeaseStore) RestoreReplica(ctx context.Context, data []byte) error {
	if len(data) > 256<<20 {
		return fmt.Errorf("dhcp: replica snapshot too large")
	}
	var snapshot replicaSnapshot
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("dhcp: expected one replica snapshot")
	}
	if snapshot.Version != 1 || snapshot.Revision == 0 || snapshot.Revision > math.MaxInt64 || snapshot.AppliedIndex > math.MaxInt64 || snapshot.Epoch > math.MaxInt64 {
		return fmt.Errorf("dhcp: invalid replica snapshot metadata")
	}
	s.replicaMu.Lock()
	defer s.replicaMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return sql.ErrConnDone
	}
	if !reflect.DeepEqual(replicaTopology(snapshot.Config), replicaTopology(s.cfg)) {
		return fmt.Errorf("dhcp: replica snapshot topology mismatch")
	}
	cfg := cloneLeaseConfig(snapshot.Config)
	cfg.Interface, cfg.ServerIP = s.cfg.Interface, s.cfg.ServerIP
	if err := cfg.Validate(); err != nil {
		return err
	}
	claimReservations := make(map[string]Reservation, len(cfg.Reservations))
	for _, r := range cfg.Reservations {
		claimReservations[r.MAC] = r
	}
	rows := make([]storedLease, 0, len(snapshot.Leases))
	var floor DNSClaim
	seenIP := map[netip.Addr]bool{}
	seenKey := map[string]bool{}
	for _, record := range snapshot.Leases {
		l, err := importedLease(record.Record)
		if err != nil {
			return err
		}
		if seenIP[l.Address] || l.State != "declined" && seenKey[l.key] {
			return fmt.Errorf("dhcp: duplicate replica lease")
		}
		seenIP[l.Address] = true
		if l.State != "declined" {
			seenKey[l.key] = true
		}
		claim := record.Claim
		if claim.Epoch > math.MaxInt64 || claim.Ticket > math.MaxInt64 || claim.Ordinal > math.MaxInt64 || claim.Ticket == 0 && (claim.Name != "" || claim.Epoch != 0 || claim.Ordinal != 0) {
			return fmt.Errorf("dhcp: invalid replica DNS claim")
		}
		if claim.Ticket > 0 {
			name, valid := dnsClaimName(l, cfg, claimReservations)
			if !valid || name != claim.Name || claim.Epoch > snapshot.Epoch {
				return fmt.Errorf("dhcp: invalid replica DNS owner")
			}
			l.claimName, l.claimEpoch, l.claimTicket, l.claimOrdinal = claim.Name, claim.Epoch, claim.Ticket, claim.Ordinal
			if claim.Epoch > floor.Epoch || claim.Epoch == floor.Epoch && claim.Ticket > floor.Ticket {
				floor = claim
			}
		}
		rows = append(rows, l)
	}
	if err := validateLeaseCandidate(cfg, rows); err != nil {
		return err
	}
	gate, err := lockDNSClaims(ctx, s.registryPath+".lock")
	if err != nil {
		return err
	}
	defer func() { _ = gate.Close() }()
	// Advancing the standalone registry first can leave only a harmless gap on
	// rollback. It prevents future standalone adoption from reissuing old ranks.
	floor.Epoch = max(floor.Epoch, snapshot.Epoch)
	if _, err := s.claimSequence(ctx, floor, false); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, "DELETE FROM lease_dns_claims; DELETE FROM leases"); err != nil {
		return err
	}
	for _, l := range rows {
		if err = saveLease(ctx, tx, l); err != nil {
			return err
		}
	}
	if err = writeLeaseSettings(ctx, tx, cfg, snapshot.Revision); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE lease_settings SET setup_token=? WHERE singleton=1", snapshot.SetupToken); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE lease_replica SET applied_index=?,epoch=?,active=? WHERE singleton=1", snapshot.AppliedIndex, snapshot.Epoch, s.replica); err != nil {
		return err
	}
	var dataVersion int64
	if err := tx.QueryRowContext(ctx, "PRAGMA data_version").Scan(&dataVersion); err != nil {
		return err
	}
	var changes int64
	if err = tx.QueryRowContext(ctx, "SELECT total_changes()").Scan(&changes); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.setLeaseConfig(cfg)
	s.revision = snapshot.Revision
	s.appliedIndex = snapshot.AppliedIndex
	s.replicaEpoch = snapshot.Epoch
	s.replaceLeaseCache(rows)
	s.cacheChanges = changes
	s.cacheDataVersion = dataVersion
	s.publishDNS(rows)
	s.publishConfiguration()
	s.replicaApplied.Store(snapshot.AppliedIndex)
	return nil
}
