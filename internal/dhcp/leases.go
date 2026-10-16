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
	leaseSchema = 3
	leaseAppID  = 0x4b444834 // KDH4; reject unrelated SQLite databases.
)

// LeaseStore has one exclusive owner and serializes transactions. SQLite is
// authoritative: an allocation is returned only after a FULL-sync commit, and
// no mutable allocation cache can get ahead of a failed write.
type LeaseStore struct {
	mu         sync.Mutex
	db         *sql.DB
	lock       *os.File
	cfg        Config
	byMAC      map[string]Reservation
	byIP       map[netip.Addr]string
	now        func() time.Time
	closed     bool
	closeErr   error
	revision   uint64
	dns        atomic.Pointer[DNSSnapshot]
	generation uint64
	setup      *leaseSetup
}

// OpenLeases opens private durable SQLite storage with exclusive ownership.
// Incompatible live leases or an unsupported schema prevent startup.
func OpenLeases(path string, cfg Config) (*LeaseStore, error) {
	return openLeases(path, cfg, nil)
}

func openLeases(path string, cfg Config, setup *leaseSetup) (*LeaseStore, error) {
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
	s := &LeaseStore{lock: lock, now: time.Now, setup: setup}
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
	// A configuration change must never silently drop a live binding or a
	// quarantine. Reject conflicts and leave the old database intact.
	if err := s.transact(context.Background(), func(_ *sql.Tx, leases []storedLease, _ time.Time) error {
		for _, l := range leases {
			if err := s.compatible(l); err != nil {
				return fmt.Errorf("dhcp: active lease %s is incompatible with configuration: %w", l.Address, err)
			}
		}
		return nil
	}); err != nil {
		return fail(err)
	}
	return s, nil
}

func (s *LeaseStore) initialize() error {
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
	}
	cfg, revision, err := s.initializeManagement(tx)
	if err != nil {
		return err
	}
	// Keep migration and first API seed atomic with live-state validation. A
	// rejected first startup must not persist incompatible seed options that
	// would then override corrected file defaults on the next attempt.
	if _, err := tx.Exec("DELETE FROM leases WHERE expires_at <= ?", s.now().Unix()); err != nil {
		return err
	}
	leases, err := readStoredLeases(context.Background(), tx)
	if err != nil {
		return err
	}
	if err := validateLeaseCandidate(cfg, leases); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.setLeaseConfig(cfg)
	s.revision = revision
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
	if s.lock != nil {
		s.closeErr = errors.Join(s.closeErr, s.lock.Close())
	}
	return s.closeErr
}

type storedLease struct {
	Lease
	key string
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

func (s *LeaseStore) compatible(l storedLease) error {
	mac, err := canonicalMAC(l.MAC)
	if err != nil || mac != l.MAC || !validHostname(l.Hostname) || !s.allowed(l.Address, l.MAC) {
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return sql.ErrConnDone
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("dhcp: begin lease transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := s.now().UTC().Truncate(time.Second)
	if _, err := tx.ExecContext(ctx, "DELETE FROM leases WHERE expires_at <= ?", now.Unix()); err != nil {
		return fmt.Errorf("dhcp: expire leases: %w", err)
	}
	leases, err := readStoredLeases(ctx, tx)
	if err != nil {
		return err
	}
	if err := fn(tx, leases, now); err != nil {
		return err
	}
	// Read the resulting state before COMMIT so a failed read/write cannot
	// leave an acknowledged allocation absent from the published DNS view.
	leases, err = readStoredLeases(ctx, tx)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		// modernc rolls back a failed COMMIT before returning its connection
		// to database/sql, including deferred-constraint failures.
		return fmt.Errorf("dhcp: commit lease transaction: %w", err)
	}
	if committed != nil {
		committed()
	}
	s.publishDNS(leases)
	return nil
}

func readStoredLeases(ctx context.Context, tx *sql.Tx) ([]storedLease, error) {
	rows, err := tx.QueryContext(ctx, "SELECT address, client_key, client_id, mac, hostname, expires_at, state FROM leases ORDER BY address")
	if err != nil {
		return nil, err
	}
	var leases []storedLease
	for rows.Next() {
		var l storedLease
		var addr uint32
		var expires int64
		if err := rows.Scan(&addr, &l.key, &l.ClientID, &l.MAC, &l.Hostname, &expires, &l.State); err != nil {
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
	_, err := tx.ExecContext(ctx, `INSERT INTO leases(address, client_key, client_id, mac, hostname, expires_at, state)
VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(address) DO UPDATE SET
client_key = excluded.client_key, client_id = excluded.client_id, mac = excluded.mac,
hostname = excluded.hostname, expires_at = excluded.expires_at, state = excluded.state`,
		addressNumber(l.Address), l.key, l.ClientID, l.MAC, l.Hostname, l.ExpiresAt.Unix(), l.State)
	if err != nil {
		return fmt.Errorf("dhcp: persist lease: %w", err)
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
	err = s.transact(ctx, func(tx *sql.Tx, leases []storedLease, now time.Time) error {
		occupied := make(map[netip.Addr]bool, len(leases))
		for _, l := range leases {
			occupied[l.Address] = true
			if l.key != key || l.State == "declined" {
				continue
			}
			if l.MAC != c.MAC {
				return ErrUnavailable
			}
			if l.State == "offered" {
				l.ExpiresAt = now.Add(offerHold)
				if err := saveLease(ctx, tx, l); err != nil {
					return err
				}
			}
			// DISCOVER never shortens or changes an existing binding.
			result = l.Lease
			return nil
		}
		var chosen netip.Addr
		r, reserved := s.reservation(c.MAC)
		if reserved {
			if occupied[r.IP] {
				return ErrUnavailable
			}
			chosen = r.IP
		} else if s.allowed(requested, c.MAC) && !occupied[requested] {
			chosen = requested
		} else {
			for ip := s.cfg.RangeStart; ip.IsValid() && ip.Compare(s.cfg.RangeEnd) <= 0; ip = ip.Next() {
				if err := ctx.Err(); err != nil {
					return err
				}
				if s.allowed(ip, c.MAC) && !occupied[ip] {
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
	err = s.transact(ctx, func(tx *sql.Tx, leases []storedLease, now time.Time) error {
		var own *storedLease
		for i := range leases {
			if leases[i].key == key && leases[i].MAC == c.MAC && leases[i].State != "declined" {
				own = &leases[i]
				break
			}
		}
		// An unknown INIT-REBOOT or renewal must stay silent, including
		// requests outside our pool or for another client's live address.
		if mode != Selecting && (own == nil || own.State != "bound") {
			return ErrUnknownLease
		}
		if !s.allowed(address, c.MAC) {
			return ErrUnavailable
		}
		for _, l := range leases {
			if l.Address != address {
				continue
			}
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
	return s.finish(ctx, c, address, false)
}

// Decline quarantines only the client's own live offer or binding for ten minutes.
func (s *LeaseStore) Decline(ctx context.Context, c Client, address netip.Addr) error {
	return s.finish(ctx, c, address, true)
}

func (s *LeaseStore) finish(ctx context.Context, c Client, address netip.Addr, decline bool) error {
	c, key, err := normalizeClient(c)
	if err != nil {
		return err
	}
	return s.transact(ctx, func(tx *sql.Tx, leases []storedLease, now time.Time) error {
		for _, l := range leases {
			if l.Address != address {
				continue
			}
			if l.key != key || l.MAC != c.MAC || l.State == "declined" {
				return ErrUnknownLease
			}
			if decline {
				l.State, l.ExpiresAt = "declined", now.Add(declineHold)
				return saveLease(ctx, tx, l)
			}
			_, err := tx.ExecContext(ctx, "DELETE FROM leases WHERE address = ?", addressNumber(address))
			return err
		}
		return ErrUnknownLease
	})
}

// List returns live leases (including holds and quarantines) in address order.
func (s *LeaseStore) List(ctx context.Context) ([]Lease, error) {
	result := []Lease{}
	err := s.transact(ctx, func(_ *sql.Tx, leases []storedLease, _ time.Time) error {
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
