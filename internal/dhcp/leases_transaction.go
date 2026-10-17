package dhcp

import (
	"context"
	"database/sql"
	"fmt"
	"net/netip"
	"slices"
	"time"
)

type leaseTransactionKey struct{}
type leaseFastKey struct{}
type leaseTransaction struct {
	store        *LeaseStore
	cfg          Config
	reservations map[string]Reservation
	rows         map[netip.Addr]storedLease
	byKey        map[string]storedLease
	changed      map[netip.Addr]storedLease
	expired      map[netip.Addr]bool
	writes       int64
	claimsIssued uint64
	now          time.Time
}

func leaseTransactionFrom(ctx context.Context) *leaseTransaction {
	v, _ := ctx.Value(leaseTransactionKey{}).(*leaseTransaction)
	return v
}
func leaseExec(ctx context.Context, tx *sql.Tx, query string, args ...any) (sql.Result, error) {
	result, err := tx.ExecContext(ctx, query, args...)
	if err == nil {
		if state := leaseTransactionFrom(ctx); state != nil {
			n, e := result.RowsAffected()
			if e != nil {
				return nil, e
			}
			state.writes += n
		}
	}
	return result, replicaStorage(ctx, err)
}
func (t *leaseTransaction) address(ip netip.Addr) (storedLease, bool) {
	l, ok := t.changed[ip]
	if !ok {
		l, ok = t.rows[ip]
	}
	return l, ok && l.Address.IsValid() && t.now.Before(l.ExpiresAt)
}
func (t *leaseTransaction) client(key string) (storedLease, bool) {
	for _, l := range t.changed {
		if l.key == key && l.State != "declined" && t.now.Before(l.ExpiresAt) {
			return l, true
		}
	}
	l, ok := t.byKey[key]
	if !ok {
		return storedLease{}, false
	}
	current, ok := t.address(l.Address)
	return current, ok && current.key == key && current.State != "declined"
}
func (t *leaseTransaction) all() []storedLease {
	rows := make([]storedLease, 0, len(t.rows))
	for ip := range t.rows {
		if l, ok := t.address(ip); ok {
			rows = append(rows, l)
		}
	}
	slices.SortFunc(rows, func(a, b storedLease) int { return a.Address.Compare(b.Address) })
	return rows
}
func (s *LeaseStore) replaceLeaseCache(rows []storedLease) {
	s.leaseByIP = make(map[netip.Addr]storedLease, len(rows))
	s.leaseByKey = make(map[string]storedLease, len(rows))
	for _, l := range rows {
		s.leaseByIP[l.Address] = l
		if l.State != "declined" {
			s.leaseByKey[l.key] = l
		}
	}
	s.cacheReady = true
}
func (s *LeaseStore) transactFast(ctx context.Context, fn func(*sql.Tx, []storedLease, time.Time) error) error {
	return s.transact(context.WithValue(ctx, leaseFastKey{}, true), fn)
}
func deleteLease(ctx context.Context, tx *sql.Tx, address netip.Addr) error {
	if _, err := leaseExec(ctx, tx, "DELETE FROM lease_dns_claims WHERE address=?", addressNumber(address)); err != nil {
		return err
	}
	if _, err := leaseExec(ctx, tx, "DELETE FROM leases WHERE address=?", addressNumber(address)); err != nil {
		return err
	}
	if state := leaseTransactionFrom(ctx); state != nil {
		state.changed[address] = storedLease{}
	}
	return nil
}
func (s *LeaseStore) runLeaseTransaction(ctx context.Context, fn func(*sql.Tx, []storedLease, time.Time) error, committed func()) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return sql.ErrConnDone
	}
	replica := replicaCommandFrom(ctx)
	if s.replica && replica == nil {
		return ErrReplicaRequired
	}
	if !s.replica {
		gate, err := lockDNSClaims(ctx, s.registryPath+".lock")
		if err != nil {
			return err
		}
		defer func() { _ = gate.Close() }()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return replicaStorage(ctx, fmt.Errorf("dhcp: begin lease transaction: %w", err))
	}
	defer func() { _ = tx.Rollback() }()
	var now time.Time
	if replica != nil {
		now = replica.command.At
	} else {
		now = s.now().UTC().Truncate(time.Second)
	}
	var before int64
	if err := tx.QueryRowContext(ctx, "SELECT total_changes()").Scan(&before); err != nil {
		return replicaStorage(ctx, err)
	}
	var dataVersion int64
	if err := tx.QueryRowContext(ctx, "PRAGMA data_version").Scan(&dataVersion); err != nil {
		return replicaStorage(ctx, err)
	}
	rows := s.leaseByIP
	fallback := !s.cacheReady || before != s.cacheChanges || dataVersion != s.cacheDataVersion
	if fallback {
		all, err := readStoredLeases(ctx, tx)
		if err != nil {
			return replicaStorage(ctx, err)
		}
		rows = make(map[netip.Addr]storedLease, len(all))
		for _, l := range all {
			rows[l.Address] = l
		}
	}
	byKey := s.leaseByKey
	if fallback {
		byKey = make(map[string]storedLease, len(rows))
		for _, l := range rows {
			if l.State != "declined" {
				byKey[l.key] = l
			}
		}
	}
	state := &leaseTransaction{store: s, cfg: s.cfg, reservations: s.byMAC, rows: rows, byKey: byKey, changed: map[netip.Addr]storedLease{}, now: now}
	ctx = context.WithValue(ctx, leaseTransactionKey{}, state)
	// Clean claims explicitly, avoiding foreign-key-trigger counts in the normal
	// path. Unexpected trigger writes force a complete authoritative reread.
	if _, err := leaseExec(ctx, tx, "DELETE FROM lease_dns_claims WHERE address IN (SELECT address FROM leases WHERE expires_at<=?)", now.Unix()); err != nil {
		return replicaStorage(ctx, err)
	}
	result, err := leaseExec(ctx, tx, "DELETE FROM leases WHERE expires_at<=?", now.Unix())
	if err != nil {
		return replicaStorage(ctx, fmt.Errorf("dhcp: expire leases: %w", err))
	}
	expired, _ := result.RowsAffected()
	if expired > 0 {
		state.expired = map[netip.Addr]bool{}
		for ip, l := range rows {
			if !now.Before(l.ExpiresAt) {
				state.changed[ip] = storedLease{}
				state.expired[ip] = true
			}
		}
	}
	var leases []storedLease
	if fast, _ := ctx.Value(leaseFastKey{}).(bool); !fast {
		leases = state.all()
	}
	// Pass the transaction's tracking context through the callback without
	// changing the package-private callback shape used by existing callers.
	s.activeTransaction = ctx
	defer func() { s.activeTransaction = nil }()
	if err := fn(tx, leases, now); err != nil {
		return err
	}
	if replica != nil {
		if _, err := leaseExec(ctx, tx, "UPDATE lease_replica SET applied_index=? WHERE singleton=1", replica.index); err != nil {
			return replicaStorage(ctx, err)
		}
	}
	var after int64
	if err := tx.QueryRowContext(ctx, "SELECT total_changes()").Scan(&after); err != nil {
		return replicaStorage(ctx, err)
	}
	fallback = fallback || after-before != state.writes
	var complete []storedLease
	if fallback {
		complete, err = readStoredLeases(ctx, tx)
		if err != nil {
			return replicaStorage(ctx, err)
		}
		for i, l := range complete {
			next, err := s.prepareClaim(ctx, l, state.cfg)
			if err != nil {
				return err
			}
			if next != l {
				if err := writeClaim(ctx, tx, next); err != nil {
					return err
				}
				complete[i] = next
			}
		}
		if err := tx.QueryRowContext(ctx, "SELECT total_changes()").Scan(&after); err != nil {
			return replicaStorage(ctx, err)
		}
	}
	if s.external != nil {
		// The external store is authoritative: commit there first, so a
		// failed write rolls back the local transaction and no reply is sent.
		if changes := externalChanges(state, rows, complete, fallback); len(changes) > 0 {
			if err := s.external(ctx, changes); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return replicaStorage(ctx, fmt.Errorf("dhcp: commit lease transaction: %w", err))
	}
	if committed != nil {
		committed()
	}
	if replica != nil {
		s.appliedIndex = replica.index
	}
	if fallback {
		s.replaceLeaseCache(complete)
		s.publishDNS(complete)
	} else {
		// Remove all old indexes first. Expiry plus a replacement at another
		// address can touch the same identity twice; map iteration order must
		// never delete the newly committed client's index.
		for ip := range state.changed {
			if old, ok := s.leaseByIP[ip]; ok && old.State != "declined" {
				delete(s.leaseByKey, old.key)
			}
		}
		for ip, l := range state.changed {
			if !l.Address.IsValid() {
				delete(s.leaseByIP, ip)
			} else {
				s.leaseByIP[ip] = l
				if l.State != "declined" {
					s.leaseByKey[l.key] = l
				}
			}
		}
		if committed != nil {
			s.publishDNS(state.all())
		} else if len(state.changed) > 0 {
			s.publishDNSChanges(state.changed)
		}
	}
	s.cacheChanges = after
	s.cacheDataVersion = dataVersion
	if committed != nil {
		s.publishConfiguration()
	}
	if replica != nil {
		s.replicaApplied.Store(replica.index)
	}
	return nil
}

// transactionContext returns the tracking context installed while mu is held.
func (s *LeaseStore) transactionContext(ctx context.Context) context.Context {
	if s.activeTransaction != nil {
		return s.activeTransaction
	}
	return ctx
}

// readLeases preserves standalone cleanup semantics. Replica reads filter their
// committed rows at the reader clock without making a local state transition.
func (s *LeaseStore) readLeases(ctx context.Context, fn func(*sql.Tx, []storedLease, time.Time) error) error {
	if !s.replica {
		return s.transact(ctx, fn)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return sql.ErrConnDone
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var changes, dataVersion int64
	if err := tx.QueryRowContext(ctx, "SELECT total_changes()").Scan(&changes); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, "PRAGMA data_version").Scan(&dataVersion); err != nil {
		return err
	}
	now := s.now().UTC().Truncate(time.Second)
	var live []storedLease
	if s.cacheReady && changes == s.cacheChanges && dataVersion == s.cacheDataVersion {
		live = (&leaseTransaction{rows: s.leaseByIP, changed: map[netip.Addr]storedLease{}, now: now}).all()
	} else {
		rows, err := readStoredLeases(ctx, tx)
		if err != nil {
			return err
		}
		live = rows[:0]
		for _, l := range rows {
			if now.Before(l.ExpiresAt) {
				live = append(live, l)
			}
		}
	}
	if err := fn(tx, live, now); err != nil {
		return err
	}
	return tx.Commit()
}
