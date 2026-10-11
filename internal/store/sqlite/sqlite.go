// Package sqlite is the state.db Store driver (docs/architecture.md §4.3,
// §4.4 "Single Docker" and "Kubernetes standalone", §6.3; decisions D3,
// D9-D12, D30, D70): every kudurru.io object lives as a JSON row in an
// embedded, cgo-free SQLite database (modernc.org/sqlite).
//
// Layout of state.db (schema migration 1):
//
//   - objects: one row per (kind, namespace, name) holding the whole object
//     as JSON plus the columns the driver queries: uid, generation,
//     resource_version, managed_by, created_at, updated_at, spec_hash,
//     labels and deleting.
//   - changes: the change feed (seq, kind, namespace, name, type,
//     resource_version, tombstone). seq is the resourceVersion of the write;
//     Watch tails it.
//   - meta: schema_version (= store.SchemaVersion), cluster_id (generated
//     once), cluster_epoch, the resourceVersion counter and the feed
//     compaction horizon.
//   - audit: one hash-chained row per mutation, written in the same
//     transaction as the mutation (D30, §11.4).
//
// resourceVersion is one database-wide counter, rendered as a decimal
// string; it only increases. Writes are serialised in-process (one writer
// connection, BEGIN IMMEDIATE) and the database file is locked against a
// second process, so the driver is the only writer of state.db.
//
// Apply follows the Apply contract of §4.3: validation through
// internal/store/validator before the write, metadata.managedBy as the single
// writer (store.ErrManagedBy), optimistic concurrency (store.ErrConflict),
// status carried over from the stored row, generation bumped only on a spec
// change, and an identical re-apply is a no-op. Caller-stamped uid and
// creationTimestamp are stored verbatim (D10) and generated only when absent.
//
// Watch delivers the current objects as Added, one Bookmark, and then live
// events from an in-process broadcaster. A watcher that falls behind, or a
// caller that resumes from a resourceVersion, catches up from the change
// feed instead of losing events.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

// DriverName is reported in store.Info.Driver.
const DriverName = "sqlite"

// FileName is the conventional name of the database under node.dataDir.
const FileName = "state.db"

// Defaults for Options.
const (
	DefaultBusyTimeout   = 5 * time.Second
	DefaultFeedRetention = 100_000
	DefaultReadConns     = 4
)

// Sentinel errors.
var (
	// ErrSchemaTooNew means state.db was written by a newer binary. The
	// process must refuse to start (D11); `kudurru doctor` explains.
	ErrSchemaTooNew = errors.New("store/sqlite: on-disk schema is newer than this binary")
	// ErrLocked means another process has state.db open.
	ErrLocked = errors.New("store/sqlite: state.db is in use by another process")
)

// Options configures Open. The zero value is usable.
type Options struct {
	// Validator checks every Apply and UpdateStatus. Default: validator.New()
	// (compiles the CEL rules of the embedded CRDs; build one per process
	// and share it when the process already has one).
	Validator *validator.Validator
	// Scheme must contain kudurru.io/v1alpha1; it decides the kinds the
	// driver serves and decodes Restore input. Default: a fresh scheme
	// with v1alpha1.
	Scheme *runtime.Scheme
	// Identity names this process in Info().Leader and Elector.Leader().
	// Default: the hostname.
	Identity string
	// BusyTimeout is SQLite's busy_timeout. Default 5 s.
	BusyTimeout time.Duration
	// FeedRetention is how many change-feed rows are kept. A watcher that
	// resumes from an older resourceVersion relists instead. Default
	// 100,000.
	FeedRetention int
	// ReadConns bounds the reader pool (WAL readers never block the
	// writer). Default 4.
	ReadConns int
	// InternalKinds are kinds that Snapshot omits when SnapshotOptions.Safe
	// is set (secrets, sessions, tokens; §6.3).
	InternalKinds []store.Kind
	// ActorFromContext names the principal recorded in audit rows. Default:
	// ActorFrom, which reads the value set by WithActor. An empty actor is
	// recorded as the write's Manager.
	ActorFromContext func(context.Context) string
	// Logger receives driver logs. Default slog.Default().
	Logger *slog.Logger
	// Now is the clock (tests). Default time.Now.
	Now func() time.Time
}

func (o *Options) setDefaults() error {
	if o.Scheme == nil {
		o.Scheme = runtime.NewScheme()
		if err := v1alpha1.AddToScheme(o.Scheme); err != nil {
			return err
		}
	}
	if o.Validator == nil {
		v, err := validator.New()
		if err != nil {
			return fmt.Errorf("store/sqlite: build validator: %w", err)
		}
		o.Validator = v
	}
	if o.Identity == "" {
		h, err := os.Hostname()
		if err != nil || h == "" {
			h = "self"
		}
		o.Identity = h
	}
	if o.BusyTimeout <= 0 {
		o.BusyTimeout = DefaultBusyTimeout
	}
	if o.FeedRetention <= 0 {
		o.FeedRetention = DefaultFeedRetention
	}
	if o.ReadConns <= 0 {
		o.ReadConns = DefaultReadConns
	}
	if o.ActorFromContext == nil {
		o.ActorFromContext = ActorFrom
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return nil
}

// Store implements store.Store on state.db.
type Store struct {
	opts     Options
	path     string
	kinds    *registry
	internal map[store.Kind]bool
	log      *slog.Logger

	w    *sql.DB // single writer connection, BEGIN IMMEDIATE
	r    *sql.DB // reader pool, query_only
	lock *fileLock

	// wmu serialises write transactions and the publication of their
	// events, so the broadcaster sees changes in resourceVersion order.
	wmu            sync.Mutex
	sinceCompacted int // writes since the last feed compaction (under wmu)

	head      atomic.Uint64 // last committed resourceVersion
	compacted atomic.Uint64 // the change feed holds every seq > compacted
	clusterID string
	epoch     uint64

	bus *broadcaster

	closeOnce sync.Once
	closed    atomic.Bool
}

var _ store.Store = (*Store)(nil)

// Open opens (creating if needed) the state.db at path, takes the process
// lock, migrates the schema to store.SchemaVersion in one transaction and
// returns the store. A database written by a newer binary returns
// ErrSchemaTooNew; a database held by another process returns ErrLocked.
func Open(path string, opts Options) (*Store, error) {
	if path == "" {
		return nil, errors.New("store/sqlite: empty path")
	}
	if err := opts.setDefaults(); err != nil {
		return nil, err
	}
	kinds, err := newRegistry(opts.Scheme)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("store/sqlite: create %s: %w", filepath.Dir(path), err)
	}
	// Create the file 0600 before SQLite does (it would use 0644); the
	// -wal and -shm files inherit the mode.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) // #nosec G304 -- operator-supplied data path
	if err != nil {
		return nil, fmt.Errorf("store/sqlite: open %s: %w", path, err)
	}
	_ = f.Close()

	lock, err := acquireLock(path + ".lock")
	if err != nil {
		return nil, err
	}
	s := &Store{
		opts:     opts,
		path:     path,
		kinds:    kinds,
		internal: map[store.Kind]bool{},
		log:      opts.Logger,
		lock:     lock,
		bus:      newBroadcaster(),
	}
	for _, k := range opts.InternalKinds {
		s.internal[k] = true
	}
	fail := func(err error) (*Store, error) {
		_ = s.closeDBs()
		return nil, err
	}
	if s.w, err = sql.Open("sqlite", dsn(path, opts.BusyTimeout, true)); err != nil {
		return fail(fmt.Errorf("store/sqlite: open %s: %w", path, err))
	}
	s.w.SetMaxOpenConns(1)
	s.w.SetMaxIdleConns(1)
	s.w.SetConnMaxLifetime(0)
	s.w.SetConnMaxIdleTime(0)

	ctx := context.Background()
	// Migrate before the reader pool exists: query_only connections must
	// not race the first creation of the WAL.
	if err := migrate(ctx, s.w, migrations, store.SchemaVersion, opts.Now); err != nil {
		return fail(err)
	}
	if s.r, err = sql.Open("sqlite", dsn(path, opts.BusyTimeout, false)); err != nil {
		return fail(fmt.Errorf("store/sqlite: open %s: %w", path, err))
	}
	s.r.SetMaxOpenConns(opts.ReadConns)
	s.r.SetMaxIdleConns(opts.ReadConns)
	if err := s.loadMeta(ctx); err != nil {
		return fail(err)
	}
	return s, nil
}

// dsn builds a modernc.org/sqlite URI with the pragmas every connection
// needs: WAL (readers never block the writer), synchronous=NORMAL (durable
// at checkpoints; the last commits before a power cut may be lost, never
// corrupted), a busy timeout and foreign keys. The writer begins every
// transaction IMMEDIATE so the write lock is taken up front; readers are
// query_only.
func dsn(path string, busy time.Duration, writer bool) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout("+strconv.FormatInt(busy.Milliseconds(), 10)+")")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(1)")
	if writer {
		q.Set("_txlock", "immediate")
	} else {
		q.Add("_pragma", "query_only(1)")
	}
	return "file:" + (&url.URL{Path: path}).EscapedPath() + "?" + q.Encode()
}

// loadMeta reads the identity and counters recorded by migrate.
func (s *Store) loadMeta(ctx context.Context) error {
	m, err := readMeta(ctx, s.r)
	if err != nil {
		return err
	}
	s.clusterID = m[metaClusterID]
	if s.epoch, err = strconv.ParseUint(m[metaClusterEpoch], 10, 64); err != nil {
		return fmt.Errorf("store/sqlite: bad %s %q: %w", metaClusterEpoch, m[metaClusterEpoch], err)
	}
	rv, err := strconv.ParseUint(m[metaResourceVersion], 10, 64)
	if err != nil {
		return fmt.Errorf("store/sqlite: bad %s %q: %w", metaResourceVersion, m[metaResourceVersion], err)
	}
	s.head.Store(rv)
	c, err := strconv.ParseUint(m[metaFeedCompacted], 10, 64)
	if err != nil {
		return fmt.Errorf("store/sqlite: bad %s %q: %w", metaFeedCompacted, m[metaFeedCompacted], err)
	}
	s.compacted.Store(c)
	return nil
}

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

// Info implements store.Store.
func (s *Store) Info() store.Info {
	return store.Info{
		Driver:        DriverName,
		SchemaVersion: store.SchemaVersion,
		ClusterID:     s.clusterID,
		Epoch:         s.epoch,
		Leader:        s.opts.Identity,
	}
}

// ResourceVersion returns the last committed resourceVersion.
func (s *Store) ResourceVersion() string { return strconv.FormatUint(s.head.Load(), 10) }

// Close implements store.Store: watchers are stopped, the WAL is
// checkpointed into state.db and the process lock is released. It is
// idempotent.
func (s *Store) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.wmu.Lock()
		s.closed.Store(true)
		s.wmu.Unlock()
		s.bus.close()
		if _, cerr := s.w.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); cerr != nil {
			s.log.Warn("state.db checkpoint on close failed", "err", cerr)
		}
		err = s.closeDBs()
	})
	return err
}

func (s *Store) closeDBs() error {
	var errs []error
	if s.r != nil {
		errs = append(errs, s.r.Close())
	}
	if s.w != nil {
		errs = append(errs, s.w.Close())
	}
	if s.lock != nil {
		errs = append(errs, s.lock.release())
	}
	return errors.Join(errs...)
}

func (s *Store) isClosed() bool { return s.closed.Load() }

// Elector implements store.Store: one process owns state.db, so it is
// always the leader. The election name is recorded on the elector.
func (s *Store) Elector(name string) store.Elector {
	return &Elector{name: name, identity: s.opts.Identity, s: s}
}

// Elector is the sqlite driver's store.Elector: always the leader.
type Elector struct {
	name     string
	identity string
	s        *Store
}

var _ store.Elector = (*Elector)(nil)

// Name returns the election name the elector was created with.
func (e *Elector) Name() string { return e.name }

// Run calls onStarted, blocks until ctx ends, then calls onStopped. It
// never loses leadership and returns nil when ctx ends.
func (e *Elector) Run(ctx context.Context, onStarted, onStopped func()) error {
	if e.s.isClosed() {
		return store.ErrClosed
	}
	if onStarted != nil {
		onStarted()
	}
	<-ctx.Done()
	if onStopped != nil {
		onStopped()
	}
	return nil
}

// IsLeader reports true while the store is open.
func (e *Elector) IsLeader() bool { return !e.s.isClosed() }

// Leader returns this process's identity.
func (e *Elector) Leader() string { return e.identity }
