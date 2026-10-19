package dhcp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"reflect"
	"slices"
	"time"
)

// ExternalLease is one lease row of an external authoritative store, such as
// a Kubernetes DHCPLease object: the lossless transfer record plus its DNS
// claim rank (zero Ticket means no claim).
type ExternalLease struct {
	Record LeaseRecord
	Claim  DNSClaim
}

// ExternalChange is one row changed by a lease transaction. A nil Lease
// deletes the row. Expired rows that a transaction cleans up locally are not
// reported; the external store expires them on its own schedule.
type ExternalChange struct {
	Address netip.Addr
	Lease   *ExternalLease
}

// ExternalPersister commits changes to the authoritative store before the
// local SQLite commit. An error rolls the local transaction back, so no reply
// or API result is produced for a change the external store did not accept.
type ExternalPersister func(ctx context.Context, changes []ExternalChange) error

// OpenExternalLeases opens a local working copy of leases owned by an
// external store. The database must be new or a copy previously opened by
// OpenExternalLeases; the caller replaces its rows with ResetExternal before
// serving. Settings come from SetExternalConfig. Every allocation, release,
// quarantine and import commits through persist first.
func OpenExternalLeases(path string, cfg Config, persist ExternalPersister) (*LeaseStore, error) {
	if persist == nil {
		return nil, errors.New("dhcp: an external lease persister is required")
	}
	cfg.Management = "api"
	s, err := openLeasesMode(path, cfg, nil, false)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.external = persist
	s.mu.Unlock()
	return s, nil
}

func externalLease(l storedLease) *ExternalLease {
	return &ExternalLease{Record: leaseRecord(l), Claim: DNSClaim{Name: l.claimName, Epoch: l.claimEpoch, Ticket: l.claimTicket, Ordinal: l.claimOrdinal}}
}

// externalChanges is the exact row delta of a transaction about to commit,
// in address order. before is the committed cache the transaction started
// from; complete is the full reread on the fallback path.
func externalChanges(state *leaseTransaction, before map[netip.Addr]storedLease, complete []storedLease, fallback bool) []ExternalChange {
	after := map[netip.Addr]storedLease{}
	addresses := map[netip.Addr]bool{}
	if fallback {
		for _, l := range complete {
			after[l.Address] = l
			addresses[l.Address] = true
		}
		for ip := range before {
			addresses[ip] = true
		}
	} else {
		for ip, l := range state.changed {
			addresses[ip] = true
			if l.Address.IsValid() {
				after[ip] = l
			}
		}
	}
	ordered := make([]netip.Addr, 0, len(addresses))
	for ip := range addresses {
		ordered = append(ordered, ip)
	}
	slices.SortFunc(ordered, func(a, b netip.Addr) int { return a.Compare(b) })
	var changes []ExternalChange
	for _, ip := range ordered {
		old, existed := before[ip]
		next, exists := after[ip]
		switch {
		case exists && existed && old == next:
		case exists:
			changes = append(changes, ExternalChange{Address: ip, Lease: externalLease(next)})
		case !existed:
		case !state.now.Before(old.ExpiresAt):
			// Local expiry cleanup only; the external store keeps the row
			// until its janitor removes it, and every reader filters expiry.
		default:
			changes = append(changes, ExternalChange{Address: ip})
		}
	}
	return changes
}

func externalRow(lease ExternalLease) (storedLease, error) {
	l, err := importedLease(lease.Record)
	if err != nil {
		return storedLease{}, err
	}
	claim := lease.Claim
	if claim.Epoch > math.MaxInt64 || claim.Ticket > math.MaxInt64 || claim.Ordinal > math.MaxInt64 || claim.Ticket == 0 && (claim.Name != "" || claim.Epoch != 0 || claim.Ordinal != 0) {
		return storedLease{}, fmt.Errorf("%w: invalid DNS claim", ErrInvalidImport)
	}
	if claim.Ticket > 0 {
		if claim.Name == "" || !validHostname(claim.Name) {
			return storedLease{}, fmt.Errorf("%w: invalid DNS claim name", ErrInvalidImport)
		}
		l.claimName, l.claimEpoch, l.claimTicket, l.claimOrdinal = claim.Name, claim.Epoch, claim.Ticket, claim.Ordinal
	}
	return l, nil
}

// SyncExternal applies committed rows from the external store verbatim, for
// example from a watch. It makes no allocation decision and calls no
// persister. A row outside this scope's subnet is ignored. When an upsert
// gives a client a different live address, the external store is the
// authority: the client's previous local row is removed.
func (s *LeaseStore) SyncExternal(ctx context.Context, upserts []ExternalLease, deletes []netip.Addr) error {
	rows := make([]storedLease, 0, len(upserts))
	for _, lease := range upserts {
		l, err := externalRow(lease)
		if err != nil {
			return err
		}
		rows = append(rows, l)
	}
	return s.replaceExternal(ctx, func(ctx context.Context, tx *sql.Tx) error {
		for _, ip := range deletes {
			if err := deleteLease(ctx, tx, ip); err != nil {
				return err
			}
		}
		for _, l := range rows {
			if !s.cfg.Subnet.Contains(l.Address) {
				continue
			}
			if l.State != "declined" {
				if _, err := tx.ExecContext(ctx, "DELETE FROM leases WHERE client_key=? AND state!='declined' AND address!=?", l.key, addressNumber(l.Address)); err != nil {
					return err
				}
			}
			if err := writeExternalRow(ctx, tx, l); err != nil {
				return err
			}
		}
		return nil
	}, rows)
}

// ResetExternal replaces every local row with a complete, consistent list
// from the external store, as a new allocator does before it serves. Rows
// outside the subnet are ignored. Duplicate live client identities keep the
// row that expires last; the external store's janitor removes the others.
func (s *LeaseStore) ResetExternal(ctx context.Context, leases []ExternalLease) error {
	byAddress := map[netip.Addr]storedLease{}
	for _, lease := range leases {
		l, err := externalRow(lease)
		if err != nil {
			return err
		}
		if !s.cfg.Subnet.Contains(l.Address) {
			continue
		}
		if old, ok := byAddress[l.Address]; ok && !l.ExpiresAt.After(old.ExpiresAt) {
			continue
		}
		byAddress[l.Address] = l
	}
	byKey := map[string]storedLease{}
	for _, l := range byAddress {
		if l.State == "declined" {
			continue
		}
		if old, ok := byKey[l.key]; ok {
			// Keep the later expiry; a tie keeps the lower address.
			if l.ExpiresAt.Before(old.ExpiresAt) || l.ExpiresAt.Equal(old.ExpiresAt) && !l.Address.Less(old.Address) {
				delete(byAddress, l.Address)
				continue
			}
			delete(byAddress, old.Address)
		}
		byKey[l.key] = l
	}
	rows := make([]storedLease, 0, len(byAddress))
	for _, l := range byAddress {
		rows = append(rows, l)
	}
	return s.replaceExternal(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM lease_dns_claims; DELETE FROM leases"); err != nil {
			return err
		}
		for _, l := range rows {
			if err := writeExternalRow(ctx, tx, l); err != nil {
				return err
			}
		}
		return nil
	}, rows)
}

func writeExternalRow(ctx context.Context, tx *sql.Tx, l storedLease) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO leases(address, client_key, client_id, mac, hostname, expires_at, state)
VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(address) DO UPDATE SET
client_key = excluded.client_key, client_id = excluded.client_id, mac = excluded.mac,
hostname = excluded.hostname, expires_at = excluded.expires_at, state = excluded.state`,
		addressNumber(l.Address), l.key, l.ClientID, l.MAC, l.Hostname, l.ExpiresAt.Unix(), l.State); err != nil {
		return fmt.Errorf("dhcp: persist external lease: %w", err)
	}
	return writeClaim(ctx, tx, l)
}

// replaceExternal runs a verbatim write and republishes the complete local
// state. Claim ranks issued here later continue above every rank seen.
func (s *LeaseStore) replaceExternal(ctx context.Context, write func(context.Context, *sql.Tx) error, seen []storedLease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return sql.ErrConnDone
	}
	if s.external == nil {
		return errors.New("dhcp: lease store is not externally owned")
	}
	gate, err := lockDNSClaims(ctx, s.registryPath+".lock")
	if err != nil {
		return err
	}
	defer func() { _ = gate.Close() }()
	var floor DNSClaim
	for _, l := range seen {
		if l.claimEpoch > floor.Epoch || l.claimEpoch == floor.Epoch && l.claimTicket > floor.Ticket {
			floor = DNSClaim{Epoch: l.claimEpoch, Ticket: l.claimTicket}
		}
	}
	if floor.Ticket > 0 {
		if _, err := s.claimSequence(ctx, floor, false); err != nil {
			return err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := write(ctx, tx); err != nil {
		return err
	}
	complete, err := readStoredLeases(ctx, tx)
	if err != nil {
		return err
	}
	var changes, dataVersion int64
	if err := tx.QueryRowContext(ctx, "SELECT total_changes()").Scan(&changes); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, "PRAGMA data_version").Scan(&dataVersion); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.replaceLeaseCache(complete)
	s.cacheChanges = changes
	s.cacheDataVersion = dataVersion
	s.publishDNS(complete)
	return nil
}

// externalTopology is the part of a scope that SetExternalConfig cannot
// change: a different value requires a new store and service.
func externalTopology(cfg Config) Config {
	cfg = replicaTopology(normalizeLeaseConfig(cfg))
	cfg.Interface, cfg.ServerIP, cfg.ServerIdentifier = "", netip.Addr{}, netip.Addr{}
	cfg.Management = ""
	return cfg
}

// SameExternalTopology reports whether SetExternalConfig can move a store from
// a to b, or whether the scope needs a new store and listener.
func SameExternalTopology(a, b Config) bool {
	a, b = normalizedConfig(a), normalizedConfig(b)
	return a.Interface == b.Interface && a.ServerIP == b.ServerIP && a.ServerIdentifier == b.ServerIdentifier &&
		reflect.DeepEqual(externalTopology(a), externalTopology(b))
}

// SetExternalConfig installs options and reservations defined by the external
// store, at its revision, without rejecting live leases: a lease the new
// settings no longer allow is not renewed and ends at expiry. With
// rankClaims, DNS claims are recomputed under the new settings and
// persisted; only the serving allocator does that. Other replicas receive
// the resulting rows through SyncExternal.
func (s *LeaseStore) SetExternalConfig(ctx context.Context, cfg Config, revision uint64, rankClaims bool) error {
	cfg.Management = "api"
	cfg = normalizeLeaseConfig(cfg)
	if err := cfg.Validate(); err != nil {
		return err
	}
	if revision == 0 || revision > math.MaxInt64 {
		return errors.New("dhcp: invalid external configuration revision")
	}
	s.mu.Lock()
	current, external := s.cfg, s.external != nil
	s.mu.Unlock()
	if !external {
		return errors.New("dhcp: lease store is not externally owned")
	}
	if !SameExternalTopology(current, cfg) {
		return errors.New("dhcp: scope topology changed; open a new store")
	}
	if rankClaims {
		return s.transaction(ctx, func(tx *sql.Tx, leases []storedLease, _ time.Time) error {
			ctx = s.transactionContext(ctx)
			state := leaseTransactionFrom(ctx)
			state.cfg = cfg
			state.reservations = make(map[string]Reservation, len(cfg.Reservations))
			for _, r := range cfg.Reservations {
				state.reservations[r.MAC] = r
			}
			if err := writeLeaseSettings(ctx, tx, cfg, revision); err != nil {
				return err
			}
			for _, l := range leases {
				next, err := s.prepareClaim(ctx, l, cfg)
				if err != nil {
					return err
				}
				if next != l {
					if err := writeClaim(ctx, tx, next); err != nil {
						return err
					}
					state.changed[l.Address] = next
				}
			}
			return nil
		}, func() {
			s.setLeaseConfig(cfg)
			s.revision = revision
		})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return sql.ErrConnDone
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := writeLeaseSettings(ctx, tx, cfg, revision); err != nil {
		return err
	}
	var changes int64
	if err := tx.QueryRowContext(ctx, "SELECT total_changes()").Scan(&changes); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.setLeaseConfig(cfg)
	s.revision = revision
	s.cacheChanges = changes
	rows := make([]storedLease, 0, len(s.leaseByIP))
	for _, l := range s.leaseByIP {
		rows = append(rows, l)
	}
	s.publishDNS(rows)
	s.publishConfiguration()
	return nil
}

// CheckExternalConfig reports whether cfg could replace this scope's settings
// without invalidating a live lease, as dashboard edits require.
func (s *LeaseStore) CheckExternalConfig(cfg Config) error {
	cfg.Management = "api"
	s.mu.Lock()
	now := s.now().UTC().Truncate(time.Second)
	live := (&leaseTransaction{rows: s.leaseByIP, changed: map[netip.Addr]storedLease{}, now: now}).all()
	s.mu.Unlock()
	return validateLeaseCandidate(cfg, live)
}

// ConflictingLeases counts live leases the current settings no longer allow.
// They are not renewed and end at expiry. A lease its client's reservation
// moves elsewhere is not counted.
func (s *LeaseStore) ConflictingLeases() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC().Truncate(time.Second)
	n := 0
	for _, l := range (&leaseTransaction{rows: s.leaseByIP, changed: map[netip.Addr]storedLease{}, now: now}).all() {
		if s.compatibleOrMoving(l) != nil {
			n++
		}
	}
	return n
}
