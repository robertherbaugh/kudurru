package dhcp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
)

// DNSClaim is durable establishment order. An epoch separates standalone
// history from a subsequent consensus log whose indexes start again at one.
type DNSClaim struct {
	Name    string `json:"name"`
	Epoch   uint64 `json:"epoch"`
	Ticket  uint64 `json:"ticket"`
	Ordinal uint64 `json:"ordinal,omitempty"`
}

func (s *LeaseStore) openClaimRegistry(ctx context.Context) error {
	if s.registry != nil {
		return nil
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		f, err := privateLeaseFile(s.registryPath+suffix, suffix == "")
		if errors.Is(err, os.ErrNotExist) && suffix != "" {
			continue
		}
		if err != nil {
			return err
		}
		if err = f.Close(); err != nil {
			return err
		}
	}
	q := url.Values{"_txlock": {"immediate"}, "_pragma": {"busy_timeout(5000)", "journal_mode(WAL)", "synchronous(FULL)"}}
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: s.registryPath}).EscapedPath()+"?"+q.Encode())
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	fail := func(err error) error { _ = db.Close(); return err }
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = tx.Rollback() }()
	fail = func(err error) error { _ = tx.Rollback(); _ = db.Close(); return err }
	var app, version int
	if err = tx.QueryRowContext(ctx, "PRAGMA application_id").Scan(&app); err != nil {
		return fail(err)
	}
	if err = tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fail(err)
	}
	if app == 0 && version == 0 {
		var n int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE name NOT GLOB 'sqlite_*'").Scan(&n); err != nil {
			return fail(err)
		}
		if n != 0 {
			return fail(fmt.Errorf("dhcp: unrelated DNS claim registry"))
		}
		if _, err = tx.ExecContext(ctx, `CREATE TABLE sequence(singleton INTEGER PRIMARY KEY CHECK(singleton=1),epoch INTEGER NOT NULL CHECK(epoch>=0),ticket INTEGER NOT NULL CHECK(ticket>=0)); INSERT INTO sequence VALUES(1,0,0); PRAGMA application_id=1262764852; PRAGMA user_version=1;`); err != nil {
			return fail(err)
		}
	} else if app != 1262764852 || version != 1 {
		return fail(fmt.Errorf("dhcp: unsupported DNS claim registry %d/%d", app, version))
	}
	if err = tx.Commit(); err != nil {
		return fail(err)
	}
	s.registry = db
	return nil
}
func (s *LeaseStore) claimSequence(ctx context.Context, floor DNSClaim, mint bool) (DNSClaim, error) {
	if err := s.openClaimRegistry(ctx); err != nil {
		return DNSClaim{}, err
	}
	tx, err := s.registry.BeginTx(ctx, nil)
	if err != nil {
		return DNSClaim{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var current DNSClaim
	if err := tx.QueryRowContext(ctx, "SELECT epoch,ticket FROM sequence WHERE singleton=1").Scan(&current.Epoch, &current.Ticket); err != nil {
		return DNSClaim{}, err
	}
	if floor.Epoch > current.Epoch || floor.Epoch == current.Epoch && floor.Ticket > current.Ticket {
		current = floor
	}
	if mint {
		if current.Ticket >= math.MaxInt64 {
			return DNSClaim{}, fmt.Errorf("dhcp: DNS claim sequence exhausted")
		}
		current.Ticket++
	}
	if _, err := tx.ExecContext(ctx, "UPDATE sequence SET epoch=?,ticket=? WHERE singleton=1", current.Epoch, current.Ticket); err != nil {
		return DNSClaim{}, err
	}
	if err := tx.Commit(); err != nil {
		return DNSClaim{}, err
	}
	return current, nil
}
func (s *LeaseStore) prepareClaim(ctx context.Context, l storedLease, cfg Config) (storedLease, error) {
	reservations := s.byMAC
	if state := leaseTransactionFrom(ctx); state != nil {
		reservations = state.reservations
	}
	name, valid := dnsClaimName(l, cfg, reservations)
	if l.State != "bound" || !cfg.RegisterDNS || !valid {
		l.claimName = ""
		l.claimEpoch = 0
		l.claimTicket = 0
		l.claimOrdinal = 0
		return l, nil
	}
	if l.claimName == name && l.claimTicket > 0 {
		return l, nil
	}
	if state := leaseTransactionFrom(ctx); state != nil {
		if old, ok := state.address(l.Address); ok && old.key == l.key && old.MAC == l.MAC && old.claimName == name && old.claimTicket > 0 {
			l.claimName, l.claimEpoch, l.claimTicket, l.claimOrdinal = old.claimName, old.claimEpoch, old.claimTicket, old.claimOrdinal
			return l, nil
		}
	}
	claim := DNSClaim{Name: name}
	if replica := replicaCommandFrom(ctx); replica != nil {
		claim.Epoch = s.replicaEpoch
		claim.Ticket = replica.index
		if state := leaseTransactionFrom(ctx); state != nil {
			state.claimsIssued++
			claim.Ordinal = state.claimsIssued
		}
	} else {
		var err error
		claim, err = s.claimSequence(ctx, DNSClaim{}, true)
		if err != nil {
			return storedLease{}, replicaStorage(ctx, fmt.Errorf("dhcp: issue DNS claim: %w", err))
		}
	}
	l.claimName, l.claimEpoch, l.claimTicket, l.claimOrdinal = name, claim.Epoch, claim.Ticket, claim.Ordinal
	return l, nil
}
func writeClaim(ctx context.Context, tx *sql.Tx, l storedLease) error {
	if l.claimTicket == 0 {
		_, err := leaseExec(ctx, tx, "DELETE FROM lease_dns_claims WHERE address=?", addressNumber(l.Address))
		return err
	}
	_, err := leaseExec(ctx, tx, `INSERT INTO lease_dns_claims(address,name,epoch,ticket,ordinal) VALUES(?,?,?,?,?) ON CONFLICT(address) DO UPDATE SET name=excluded.name,epoch=excluded.epoch,ticket=excluded.ticket,ordinal=excluded.ordinal`, addressNumber(l.Address), l.claimName, l.claimEpoch, l.claimTicket, l.claimOrdinal)
	return err
}

func dnsClaimName(l storedLease, cfg Config, reservations map[string]Reservation) (string, bool) {
	hostname := l.Hostname
	if r, ok := reservations[l.MAC]; ok && r.Hostname != "" {
		hostname = r.Hostname
	}
	name, valid := leaseDNSName(hostname, cfg.DomainName)
	return name, valid && cfg.RegisterDNS && l.State == "bound"
}
