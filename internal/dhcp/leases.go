package dhcp

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // registers the cgo-free SQLite driver
)

// Client identifies a client by opaque option 61 ID, or by MAC when ID is empty.
// MAC is always retained and checked when changing an existing lease.
type Client struct{ ID, MAC, Hostname string }

// Lease is a live address allocation, offer hold, or decline quarantine.
type Lease struct {
	Address                 netip.Addr
	ClientID, MAC, Hostname string
	ExpiresAt               time.Time
	State                   string // offered, bound, declined
	// GrantedDuration is response-only: the duration promised by this decision.
	// It is never stored, exported, or included in a committed DNS snapshot.
	GrantedDuration time.Duration `json:"grantedDuration,omitempty"`
}

// RequestMode distinguishes DHCPREQUEST states and their ownership requirements.
type RequestMode int

// DHCPREQUEST modes require an existing offer (Selecting) or binding.
const (
	Selecting RequestMode = iota
	InitReboot
	Renewing
)

var (
	// ErrNoAddress means every usable dynamic address is held.
	ErrNoAddress = errors.New("dhcp: no address available")
	// ErrUnknownLease means this client has no matching live lease.
	ErrUnknownLease = errors.New("dhcp: unknown lease")
	// ErrUnavailable means a known request conflicts with allocation policy or ownership.
	ErrUnavailable = errors.New("dhcp: address unavailable")
)

const (
	offerHold   = time.Minute
	declineHold = 10 * time.Minute
	leaseSchema = 4
	leaseAppID  = 0x4b444834 // KDH4; reject unrelated SQLite databases.
)

// LeaseStore has one exclusive owner and serializes transactions. SQLite is
// authoritative: an allocation is returned only after a FULL-sync commit, and
// no mutable allocation cache can get ahead of a failed write.
type LeaseStore struct {
	mu                sync.Mutex
	db                *sql.DB
	lock              *os.File
	cfg               Config
	byMAC             map[string]Reservation
	byIP              map[netip.Addr]string
	now               func() time.Time
	closed            bool
	closeErr          error
	revision          uint64
	management        atomic.Pointer[ManagementState]
	dns               atomic.Pointer[DNSSnapshot]
	generation        uint64
	setup             *leaseSetup
	registry          *sql.DB
	registryPath      string
	replica           bool
	appliedIndex      uint64
	replicaApplied    atomic.Uint64
	replicaEpoch      uint64
	replicaMu         sync.Mutex
	leaseByIP         map[netip.Addr]storedLease
	leaseByKey        map[string]storedLease
	cacheChanges      int64
	cacheDataVersion  int64
	cacheReady        bool
	activeTransaction context.Context
	external          ExternalPersister
}

// OpenLeases opens private durable SQLite storage with exclusive ownership.
// Incompatible live leases or an unsupported schema prevent startup.
func OpenLeases(path string, cfg Config) (*LeaseStore, error) {
	return openLeasesMode(path, cfg, nil, false)
}

func openLeasesMode(path string, cfg Config, setup *leaseSetup, replica bool) (*LeaseStore, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if path == "" || path == ":memory:" {
		return nil, fmt.Errorf("dhcp: a durable lease database path is required")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil { // #nosec G703 -- operator-configured database path
		return nil, fmt.Errorf("dhcp: create lease directory: %w", err)
	}
	// Resolve parent aliases before SQLite chooses the WAL filename.
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	path = filepath.Join(dir, filepath.Base(path))
	lock, err := lockLeaseFile(path)
	if err != nil {
		return nil, err
	}
	s := &LeaseStore{lock: lock, now: time.Now, setup: setup, replica: replica, registryPath: filepath.Join(filepath.Dir(path), ".dhcp-dns-claims.sqlite")}
	s.setLeaseConfig(normalizeLeaseConfig(cfg))
	fail := func(err error) (*LeaseStore, error) {
		_ = s.Close()
		return nil, err
	}
	// Existing sidecars contain the same private client data as the main file.
	for _, suffix := range []string{"-wal", "-shm"} {
		f, err := privateLeaseFile(path+suffix, false)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fail(err)
		}
		if err := f.Close(); err != nil {
			return fail(err)
		}
	}
	q := url.Values{}
	q.Set("_txlock", "immediate")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(FULL)")
	q.Add("_pragma", "foreign_keys(ON)")
	s.db, err = sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?"+q.Encode())
	if err != nil {
		return fail(err)
	}
	s.db.SetMaxOpenConns(1)
	s.db.SetMaxIdleConns(1)
	if err := s.initialize(); err != nil {
		return fail(err)
	}
	return s, nil
}

func (s *LeaseStore) initialize() error {
	if !s.replica {
		gate, err := lockDNSClaims(context.Background(), s.registryPath+".lock")
		if err != nil {
			return err
		}
		defer func() { _ = gate.Close() }()
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("dhcp: open lease database: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var version, app int
	if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if err := tx.QueryRow("PRAGMA application_id").Scan(&app); err != nil {
		return err
	}
	if version == 0 && app == 0 {
		var tables int
		if err := tx.QueryRow("SELECT count(*) FROM sqlite_schema WHERE name NOT GLOB 'sqlite_*'").Scan(&tables); err != nil {
			return err
		}
		if tables != 0 {
			return fmt.Errorf("dhcp: refusing to initialize an unrelated database")
		}
		if _, err := tx.Exec(`
CREATE TABLE leases (
    address INTEGER PRIMARY KEY CHECK(address BETWEEN 0 AND 4294967295),
    client_key TEXT NOT NULL,
    client_id TEXT NOT NULL,
    mac TEXT NOT NULL,
    hostname TEXT NOT NULL,
    expires_at INTEGER NOT NULL CHECK(expires_at > 0),
    state TEXT NOT NULL CHECK(state IN ('offered', 'bound', 'declined'))
);
CREATE UNIQUE INDEX leases_client ON leases(client_key) WHERE state != 'declined';
PRAGMA application_id = 1262766132;
PRAGMA user_version = 1;
`); err != nil {
			return fmt.Errorf("dhcp: initialize lease schema: %w", err)
		}
		app, version = leaseAppID, 1
	}
	if app != leaseAppID || version < 1 || version > leaseSchema {
		return fmt.Errorf("dhcp: unsupported lease database application/schema version %d/%d", app, version)
	}
	if version == 1 {
		if _, err := tx.Exec(`CREATE TABLE lease_settings (
singleton INTEGER PRIMARY KEY CHECK(singleton=1),
revision INTEGER NOT NULL CHECK(revision > 0),
options TEXT NOT NULL,
reservations TEXT NOT NULL
); PRAGMA user_version = 2;`); err != nil {
			return fmt.Errorf("dhcp: migrate lease management schema: %w", err)
		}
		version = 2
	}
	if version == 2 {
		if _, err := tx.Exec(`ALTER TABLE lease_settings ADD COLUMN setup_token TEXT NOT NULL DEFAULT ''; PRAGMA user_version = 3;`); err != nil {
			return fmt.Errorf("dhcp: migrate lease setup schema: %w", err)
		}
		version = 3
	}
	if version == 3 {
		if _, err := tx.Exec(`CREATE TABLE lease_dns_claims (
 address INTEGER PRIMARY KEY REFERENCES leases(address) ON DELETE CASCADE,
 name TEXT NOT NULL, epoch INTEGER NOT NULL CHECK(epoch >= 0), ticket INTEGER NOT NULL CHECK(ticket > 0), ordinal INTEGER NOT NULL DEFAULT 0 CHECK(ordinal >= 0));
 CREATE TABLE lease_replica (singleton INTEGER PRIMARY KEY CHECK(singleton=1), applied_index INTEGER NOT NULL CHECK(applied_index>=0), epoch INTEGER NOT NULL CHECK(epoch>=0), active INTEGER NOT NULL CHECK(active IN (0,1)));
 INSERT INTO lease_replica VALUES(1,0,0,0);
 CREATE INDEX leases_expiry ON leases(expires_at);
 PRAGMA user_version = 4;`); err != nil {
			return fmt.Errorf("dhcp: migrate DNS ownership schema: %w", err)
		}
	}
	var active int
	if err := tx.QueryRow("SELECT applied_index,epoch,active FROM lease_replica WHERE singleton=1").Scan(&s.appliedIndex, &s.replicaEpoch, &active); err != nil {
		return err
	}
	if s.appliedIndex > 0 && (!s.replica || s.setup != nil) {
		return fmt.Errorf("%w: existing replicated ownership cannot be opened standalone or locally restaged", ErrReplicaRequired)
	}
	cfg, revision, err := s.initializeManagement(tx)
	if err != nil {
		return err
	}
	// Keep migration and first API seed atomic with live-state validation. A
	// rejected first startup must not persist incompatible seed options that
	// would then override corrected file defaults on the next attempt.
	if !s.replica {
		if _, err := tx.Exec("DELETE FROM leases WHERE expires_at <= ?", s.now().Unix()); err != nil {
			return err
		}
	}
	leases, err := readStoredLeases(context.Background(), tx)
	if err != nil {
		return err
	}
	if err := validateLeaseCandidate(cfg, leases); err != nil {
		return err
	}
	s.setLeaseConfig(cfg)
	if !s.replica {
		if err := s.openClaimRegistry(context.Background()); err != nil {
			return err
		}
	}
	if s.replica && active == 0 {
		for _, l := range leases {
			s.replicaEpoch = max(s.replicaEpoch, l.claimEpoch)
		}
		s.replicaEpoch++
	}
	if _, err := tx.Exec("UPDATE lease_replica SET epoch=?,active=? WHERE singleton=1", s.replicaEpoch, s.replica); err != nil {
		return err
	}
	if !s.replica {
		floor := DNSClaim{Epoch: s.replicaEpoch}
		for _, l := range leases {
			if l.claimEpoch > floor.Epoch || l.claimEpoch == floor.Epoch && l.claimTicket > floor.Ticket {
				floor = DNSClaim{Epoch: l.claimEpoch, Ticket: l.claimTicket}
			}
		}
		if floor.Epoch > 0 || floor.Ticket > 0 {
			if _, err := s.claimSequence(context.Background(), floor, false); err != nil {
				return err
			}
		}
	}
	for i, l := range leases {
		var next storedLease
		if s.replica {
			next = l
			if next.State == "bound" && next.claimTicket == 0 && cfg.RegisterDNS {
				hostname := l.Hostname
				if r, ok := s.byMAC[l.MAC]; ok && r.Hostname != "" {
					hostname = r.Hostname
				}
				if name, valid := leaseDNSName(hostname, cfg.DomainName); valid {
					next.claimName = name
					next.claimTicket = uint64(addressNumber(l.Address)) + 1
				}
			}
		} else {
			var err error
			next, err = s.prepareClaim(context.Background(), l, cfg)
			if err != nil {
				return err
			}
		}
		if next != l {
			if err := writeClaim(context.Background(), tx, next); err != nil {
				return err
			}
			leases[i] = next
		}
	}
	if err := tx.QueryRow("SELECT total_changes()").Scan(&s.cacheChanges); err != nil {
		return err
	}
	if err := tx.QueryRow("PRAGMA data_version").Scan(&s.cacheDataVersion); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.revision = revision
	s.replaceLeaseCache(leases)
	s.publishDNS(leases)
	s.publishConfiguration()
	s.replicaApplied.Store(s.appliedIndex)
	return nil
}

// Close releases SQLite and then the exclusive owner lock; it is idempotent.
func (s *LeaseStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	s.dns.Store(nil)
	if s.db != nil {
		s.closeErr = s.db.Close()
	}
	if s.registry != nil {
		s.closeErr = errors.Join(s.closeErr, s.registry.Close())
	}
	if s.lock != nil {
		s.closeErr = errors.Join(s.closeErr, s.lock.Close())
	}
	return s.closeErr
}

type storedLease struct {
	Lease
	key                                   string
	claimName                             string
	claimEpoch, claimTicket, claimOrdinal uint64
}

func normalizeClient(c Client) (Client, string, error) {
	mac, err := canonicalMAC(c.MAC)
	if err != nil {
		return Client{}, "", fmt.Errorf("dhcp: client: %w", err)
	}
	if len(c.ID) > 1024 || !validHostname(c.Hostname) {
		return Client{}, "", fmt.Errorf("dhcp: invalid client identity or hostname")
	}
	c.MAC = mac
	if c.ID != "" {
		return c, "id:" + c.ID, nil
	}
	return c, "mac:" + mac, nil
}

func clientID(c Client) string {
	if c.ID != "" {
		return c.ID
	}
	return c.MAC
}

func (s *LeaseStore) reservation(mac string) (Reservation, bool) {
	r, ok := s.byMAC[mac]
	return r, ok
}

func (s *LeaseStore) allowed(ip netip.Addr, mac string) bool {
	if !s.cfg.usable(ip) || s.cfg.infrastructure(ip) {
		return false
	}
	if r, ok := s.byMAC[mac]; ok {
		return ip == r.IP
	}
	if _, reserved := s.byIP[ip]; reserved {
		return false
	}
	return s.cfg.inPool(ip)
}

// moving reports a live lease, including a quarantine, whose client is now
// reserved at another address; no other reservation may name its address.
// An offer or binding is not renewed, and the client's next DISCOVER frees
// it and offers the reservation (Offer). A quarantine guards its address,
// not the client, so it stays until declineHold ends.
func (s *LeaseStore) moving(l storedLease) bool {
	r, ok := s.byMAC[l.MAC]
	_, reserved := s.byIP[l.Address]
	return ok && r.IP != l.Address && !reserved && s.cfg.usable(l.Address) && !s.cfg.infrastructure(l.Address)
}

func (s *LeaseStore) compatible(l storedLease) error {
	return s.checkLease(l, s.allowed(l.Address, l.MAC))
}

// compatibleOrMoving also accepts a lease its client's reservation moves.
func (s *LeaseStore) compatibleOrMoving(l storedLease) error {
	return s.checkLease(l, s.allowed(l.Address, l.MAC) || s.moving(l))
}

func (s *LeaseStore) checkLease(l storedLease, allowed bool) error {
	mac, err := canonicalMAC(l.MAC)
	if err != nil || mac != l.MAC || !validHostname(l.Hostname) || !allowed {
		return ErrUnavailable
	}
	// ClientID is exposed as the hardware address for MAC-only clients; the
	// separate key namespace prevents collision with an option 61 of that text.
	macIdentity := l.key == "mac:"+l.MAC && l.ClientID == l.MAC
	if l.key != "id:"+l.ClientID && !macIdentity {
		return fmt.Errorf("invalid persisted client identity")
	}
	return nil
}

// transact cleans expired rows and performs every read/decision/write in one
// immediate transaction. Even List fails closed on persistence errors.
func (s *LeaseStore) transact(ctx context.Context, fn func(*sql.Tx, []storedLease, time.Time) error) error {
	return s.transaction(ctx, fn, nil)
}

func (s *LeaseStore) transaction(ctx context.Context, fn func(*sql.Tx, []storedLease, time.Time) error, committed func()) error {
	return s.runLeaseTransaction(ctx, fn, committed)
}

func readStoredLeases(ctx context.Context, tx *sql.Tx) ([]storedLease, error) {
	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return nil, err
	}
	query := "SELECT address,client_key,client_id,mac,hostname,expires_at,state,'',0,0,0 FROM leases ORDER BY address"
	if version >= 4 {
		query = "SELECT l.address,client_key,client_id,mac,hostname,expires_at,state,coalesce(c.name,''),coalesce(c.epoch,0),coalesce(c.ticket,0),coalesce(c.ordinal,0) FROM leases l LEFT JOIN lease_dns_claims c ON c.address=l.address ORDER BY l.address"
	}
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	var leases []storedLease
	for rows.Next() {
		var l storedLease
		var addr uint32
		var expires int64
		if err := rows.Scan(&addr, &l.key, &l.ClientID, &l.MAC, &l.Hostname, &expires, &l.State, &l.claimName, &l.claimEpoch, &l.claimTicket, &l.claimOrdinal); err != nil {
			_ = rows.Close()
			return nil, err
		}
		var bytes [4]byte
		binary.BigEndian.PutUint32(bytes[:], addr)
		l.Address = netip.AddrFrom4(bytes)
		l.ExpiresAt = time.Unix(expires, 0).UTC()
		leases = append(leases, l)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	return leases, nil
}

func addressNumber(ip netip.Addr) uint32 {
	b := ip.As4()
	return binary.BigEndian.Uint32(b[:])
}

func saveLease(ctx context.Context, tx *sql.Tx, l storedLease) error {
	state := leaseTransactionFrom(ctx)
	if state != nil {
		var err error
		l, err = state.store.prepareClaim(ctx, l, state.store.cfg)
		if err != nil {
			return err
		}
	}
	if _, err := leaseExec(ctx, tx, `INSERT INTO leases(address, client_key, client_id, mac, hostname, expires_at, state)
VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(address) DO UPDATE SET
client_key = excluded.client_key, client_id = excluded.client_id, mac = excluded.mac,
hostname = excluded.hostname, expires_at = excluded.expires_at, state = excluded.state`,
		addressNumber(l.Address), l.key, l.ClientID, l.MAC, l.Hostname, l.ExpiresAt.Unix(), l.State); err != nil {
		return fmt.Errorf("dhcp: persist lease: %w", err)
	}
	if err := writeClaim(ctx, tx, l); err != nil {
		return err
	}
	if state != nil {
		state.changed[l.Address] = l
	}
	return nil
}

// Offer durably holds an eligible address, preferring requested when possible.
// A repeated DISCOVER preserves an existing binding and its expiration.
func (s *LeaseStore) Offer(ctx context.Context, c Client, requested netip.Addr) (Lease, error) {
	c, key, err := normalizeClient(c)
	if err != nil {
		return Lease{}, err
	}
	var result Lease
	err = s.transactFast(ctx, func(tx *sql.Tx, _ []storedLease, now time.Time) error {
		ctx = s.transactionContext(ctx)
		state := leaseTransactionFrom(ctx)
		if l, ok := state.client(key); ok && l.MAC == c.MAC && !s.allowed(l.Address, l.MAC) {
			// A client in INIT no longer uses an address the settings now
			// refuse it, e.g. after a reservation moved it: free that address
			// and allocate again.
			if err := deleteLease(ctx, tx, l.Address); err != nil {
				return err
			}
		} else if ok {
			if l.MAC != c.MAC {
				return ErrUnavailable
			}
			if l.State == "offered" {
				l.ExpiresAt = now.Add(offerHold)
				if err := saveLease(ctx, tx, l); err != nil {
					return err
				}
			}
			result = l.Lease
			result.GrantedDuration = s.cfg.LeaseDuration
			return nil
		}
		occupied := func(ip netip.Addr) bool { _, ok := state.address(ip); return ok }
		var chosen netip.Addr
		r, reserved := s.reservation(c.MAC)
		if reserved {
			if occupied(r.IP) {
				return ErrUnavailable
			}
			chosen = r.IP
		} else if s.allowed(requested, c.MAC) && !occupied(requested) {
			chosen = requested
		} else {
			for ip := s.cfg.RangeStart; ip.IsValid() && ip.Compare(s.cfg.RangeEnd) <= 0; ip = ip.Next() {
				if err := ctx.Err(); err != nil {
					return err
				}
				if s.allowed(ip, c.MAC) && !occupied(ip) {
					chosen = ip
					break
				}
			}
		}
		if !chosen.IsValid() {
			return ErrNoAddress
		}
		hostname := c.Hostname
		if reserved && r.Hostname != "" {
			hostname = r.Hostname
		}
		l := storedLease{Lease: Lease{Address: chosen, ClientID: clientID(c), MAC: c.MAC,
			Hostname: hostname, ExpiresAt: now.Add(offerHold), State: "offered"}, key: key}
		if err := saveLease(ctx, tx, l); err != nil {
			return err
		}
		result = l.Lease
		result.GrantedDuration = s.cfg.LeaseDuration
		return nil
	})
	if err != nil {
		return Lease{}, err
	}
	return result, nil
}

// Acknowledge durably binds an existing offer or renews an existing binding.
// Unknown INIT-REBOOT/renewing clients return ErrUnknownLease regardless of IP.
// A binding's existing expiration remains a lower bound even if its ACK is lost.
func (s *LeaseStore) Acknowledge(ctx context.Context, c Client, address netip.Addr, mode RequestMode) (Lease, error) {
	c, key, err := normalizeClient(c)
	if err != nil {
		return Lease{}, err
	}
	if mode < Selecting || mode > Renewing {
		return Lease{}, fmt.Errorf("dhcp: invalid request mode")
	}
	var result Lease
	err = s.transactFast(ctx, func(tx *sql.Tx, _ []storedLease, now time.Time) error {
		ctx = s.transactionContext(ctx)
		state := leaseTransactionFrom(ctx)
		var own *storedLease
		if l, ok := state.client(key); ok && l.MAC == c.MAC {
			own = &l
		}
		// An unknown INIT-REBOOT or renewal must stay silent, including
		// requests outside our pool or for another client's live address.
		if mode != Selecting && (own == nil || own.State != "bound") {
			return ErrUnknownLease
		}
		if !s.allowed(address, c.MAC) {
			return ErrUnavailable
		}
		if l, ok := state.address(address); ok {
			if l.State == "declined" || l.key != key || l.MAC != c.MAC {
				return ErrUnavailable
			}
			if mode != Selecting && l.State != "bound" {
				return ErrUnknownLease
			}
			expires := now.Add(s.cfg.LeaseDuration)
			// The client may keep using its previously acknowledged lease if
			// this ACK is lost. A shorter configured duration or a backward
			// clock adjustment must not make that address available early.
			if l.State == "bound" && l.ExpiresAt.After(expires) {
				expires = l.ExpiresAt
			}
			l.State, l.ExpiresAt = "bound", expires
			if c.Hostname != "" {
				l.Hostname = c.Hostname
			}
			if r, ok := s.reservation(c.MAC); ok && r.Hostname != "" {
				l.Hostname = r.Hostname
			}
			if err := saveLease(ctx, tx, l); err != nil {
				return err
			}
			result = l.Lease
			result.GrantedDuration = expires.Sub(now)
			return nil
		}
		if own != nil {
			return ErrUnavailable
		}
		return ErrUnknownLease
	})
	if err != nil {
		return Lease{}, err
	}
	return result, nil
}

// Release frees only the client's own live offer or binding, never quarantine.
func (s *LeaseStore) Release(ctx context.Context, c Client, address netip.Addr) error {
	return s.finish(ctx, c, address, false, false)
}

// Decline quarantines only the client's own live offer or binding for ten minutes.
func (s *LeaseStore) Decline(ctx context.Context, c Client, address netip.Addr) error {
	return s.finish(ctx, c, address, true, false)
}

// QuarantineOffer records a detected conflict only for the exact owner of a
// live offer. A stale probe can never quarantine an acknowledged binding.
func (s *LeaseStore) QuarantineOffer(ctx context.Context, c Client, address netip.Addr) error {
	return s.finish(ctx, c, address, true, true)
}

func (s *LeaseStore) finish(ctx context.Context, c Client, address netip.Addr, decline, offeredOnly bool) error {
	c, key, err := normalizeClient(c)
	if err != nil {
		return err
	}
	return s.transactFast(ctx, func(tx *sql.Tx, _ []storedLease, now time.Time) error {
		ctx = s.transactionContext(ctx)
		state := leaseTransactionFrom(ctx)
		if l, ok := state.address(address); ok {
			if l.key != key || l.MAC != c.MAC || l.State == "declined" || offeredOnly && l.State != "offered" {
				return ErrUnknownLease
			}
			if decline {
				l.State, l.ExpiresAt = "declined", now.Add(declineHold)
				return saveLease(ctx, tx, l)
			}
			return deleteLease(ctx, tx, address)
		}
		return ErrUnknownLease
	})
}

// List returns live leases (including holds and quarantines) in address order.
func (s *LeaseStore) List(ctx context.Context) ([]Lease, error) {
	result := []Lease{}
	err := s.readLeases(ctx, func(_ *sql.Tx, leases []storedLease, _ time.Time) error {
		result = make([]Lease, 0, len(leases))
		for _, l := range leases {
			result = append(result, l.Lease)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
