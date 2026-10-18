// Package localdb is the last-good config snapshot in local.db
// (dev-docs/design-record.md §4.4, §4.5, decisions D7 and D33).
//
// It lives beside the drivers rather than inside the kube driver because
// its writer and reader is internal/zonecache, which is driver-independent
// (D79).
//
// local.db also holds named blobs (blob.go, BlobStore): opaque node-local
// state that should survive a restart, such as the response cache dump
// written by internal/cache/persist.
//
// zonecache writes the complete materialised object set (every kudurru.io
// kind as JSON) together with the resourceVersion high-water mark after
// every successful sync, and then at most every 30 s when it changed. A pod
// that starts while the apiserver is unreachable loads the snapshot,
// reports Degraded{ServingFromSnapshot} and serves from it until the
// informers sync; Snapshot.Age gates readiness against
// store.kube.snapshot.maxAge.
//
// The package is deliberately free of client-go and controller-runtime so
// the cold-start reader does not need a Kubernetes client. It uses the
// cgo-free modernc.org/sqlite driver (CGO_ENABLED=0 holds).
package localdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// FileName is the conventional name of the database under node.dataDir.
const FileName = "local.db"

// Sentinel errors.
var (
	// ErrNoSnapshot means local.db holds no snapshot yet (first start).
	ErrNoSnapshot = errors.New("localdb: no snapshot")
	// ErrSchemaTooNew means local.db was written by a newer binary; the
	// process must refuse to start (§4.3 schema versioning, D11).
	ErrSchemaTooNew = errors.New("localdb: on-disk schema is newer than this binary")
)

// schemaV1 creates the snapshot tables. The meta table is shared by every
// user of local.db; the kube_snapshot* tables belong to this package.
const schemaV1 = `
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
) STRICT;
CREATE TABLE IF NOT EXISTS kube_snapshot (
	id               INTEGER PRIMARY KEY CHECK (id = 1),
	written_at_ns    INTEGER NOT NULL,
	resource_version TEXT    NOT NULL,
	cluster_id       TEXT    NOT NULL,
	objects          INTEGER NOT NULL
) STRICT;
CREATE TABLE IF NOT EXISTS kube_snapshot_kinds (
	kind             TEXT PRIMARY KEY,
	resource_version TEXT    NOT NULL,
	objects          INTEGER NOT NULL
) STRICT, WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS kube_snapshot_objects (
	kind             TEXT NOT NULL,
	namespace        TEXT NOT NULL,
	name             TEXT NOT NULL,
	api_version      TEXT NOT NULL,
	resource_version TEXT NOT NULL,
	body             BLOB NOT NULL,
	PRIMARY KEY (kind, namespace, name)
) STRICT, WITHOUT ROWID;
`

// Snapshot is one materialised object set.
type Snapshot struct {
	// Objects are typed kudurru.io objects with TypeMeta set.
	Objects []store.Object
	// ResourceVersions holds, per kind, the list resourceVersion the
	// object set is at least as fresh as (the informer's last-synced
	// resourceVersion).
	ResourceVersions map[store.Kind]string
	// ClusterID is the cluster the snapshot came from (kube-system UID).
	ClusterID string
	// WrittenAt is when the snapshot was taken.
	WrittenAt time.Time
	// Skipped lists "<apiVersion>/<kind> <namespace>/<name>" of rows whose
	// kind this binary does not know; they are not served.
	Skipped []string
}

// HighWaterMark returns the largest per-kind resourceVersion. Kubernetes
// resourceVersions are etcd revisions of one counter, so the maximum is
// meaningful; it returns "" when any value is not a decimal integer.
func (s *Snapshot) HighWaterMark() string {
	var hi uint64
	found := false
	for _, rv := range s.ResourceVersions {
		n, err := strconv.ParseUint(rv, 10, 64)
		if err != nil {
			return ""
		}
		if !found || n > hi {
			hi, found = n, true
		}
	}
	if !found {
		return ""
	}
	return strconv.FormatUint(hi, 10)
}

// Age is how old the snapshot is at now (kudurru_store_snapshot_age_seconds).
func (s *Snapshot) Age(now time.Time) time.Duration { return now.Sub(s.WrittenAt) }

// Fresh reports whether the snapshot may back primary zones: younger than
// maxAge (store.kube.snapshot.maxAge, 24 h). Older snapshots still back
// forwarding; primary zones answer SERVFAIL (§4.4).
func (s *Snapshot) Fresh(maxAge time.Duration, now time.Time) bool { return s.Age(now) <= maxAge }

// ByKind returns the objects of one kind, in snapshot order.
func (s *Snapshot) ByKind(kind store.Kind) []store.Object {
	var out []store.Object
	for _, o := range s.Objects {
		if store.KindOf(o) == kind {
			out = append(out, o)
		}
	}
	return out
}

// SnapshotStore reads and writes the snapshot tables of local.db.
type SnapshotStore struct {
	db     *sql.DB
	scheme *runtime.Scheme
}

// OpenSnapshotStore opens (creating if needed) the SQLite database at path
// and migrates it to store.SchemaVersion in one transaction. A database
// written by a newer binary returns ErrSchemaTooNew.
func OpenSnapshotStore(ctx context.Context, path string) (*SnapshotStore, error) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	// Create the file 0600 before SQLite does (it would use 0644): local.db
	// carries the configuration snapshot and the cache dump.
	if f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600); err == nil { // #nosec G304 -- operator-supplied data path
		_ = f.Close()
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("localdb: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(4)
	s := &SnapshotStore{db: db, scheme: scheme}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// dsn builds a modernc.org/sqlite URI: WAL so the cold-start reader never
// blocks the writer, NORMAL sync (durable at checkpoints; a lost last
// snapshot only makes the next one older) and a busy timeout. Write's
// first statement is a write, so its deferred transaction takes the write
// lock up front and waits on the busy timeout instead of failing an
// upgrade.
func dsn(path string) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	return "file:" + (&url.URL{Path: path}).EscapedPath() + "?" + q.Encode()
}

func (s *SnapshotStore) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("localdb: begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL) STRICT`); err != nil {
		return fmt.Errorf("localdb: create meta: %w", err)
	}
	version := 0
	var v string
	switch err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&v); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("localdb: read schema_version: %w", err)
	default:
		if version, err = strconv.Atoi(v); err != nil {
			return fmt.Errorf("localdb: bad schema_version %q: %w", v, err)
		}
	}
	if version > store.SchemaVersion {
		return fmt.Errorf("%w: local.db is at schema %d, binary supports %d", ErrSchemaTooNew, version, store.SchemaVersion)
	}
	if err := migrateLayout(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES ('schema_version', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		strconv.Itoa(store.SchemaVersion)); err != nil {
		return fmt.Errorf("localdb: write schema_version: %w", err)
	}
	return tx.Commit()
}

// LayoutVersion is the version of local.db's own table layout, recorded
// as meta.layout_version. It is separate from meta.schema_version, which
// stays store.SchemaVersion: that one versions the API objects stored in
// the snapshot, this one the tables (D11 applies to both: a newer on-disk
// value refuses to open, an older one migrates in the same transaction).
const LayoutVersion = 2

// layoutSteps[i] migrates the layout from version i to i+1. Step 0 is
// idempotent (CREATE ... IF NOT EXISTS) because databases written before
// layout_version existed already hold its tables.
var layoutSteps = []string{
	schemaV1,
	schemaV2Blobs,
}

// migrateLayout applies the layout steps after meta.layout_version.
func migrateLayout(ctx context.Context, tx *sql.Tx) error {
	layout := 0
	var v string
	switch err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'layout_version'`).Scan(&v); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("localdb: read layout_version: %w", err)
	default:
		var perr error
		if layout, perr = strconv.Atoi(v); perr != nil {
			return fmt.Errorf("localdb: bad layout_version %q: %w", v, perr)
		}
	}
	if layout > LayoutVersion {
		return fmt.Errorf("%w: local.db table layout is at version %d, binary supports %d", ErrSchemaTooNew, layout, LayoutVersion)
	}
	for i := layout; i < len(layoutSteps); i++ {
		if _, err := tx.ExecContext(ctx, layoutSteps[i]); err != nil {
			return fmt.Errorf("localdb: apply layout v%d: %w", i+1, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES ('layout_version', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		strconv.Itoa(LayoutVersion)); err != nil {
		return fmt.Errorf("localdb: write layout_version: %w", err)
	}
	return nil
}

// Write replaces the stored snapshot with snap in one transaction, so a
// reader sees either the previous snapshot or the new one. A zero
// snap.WrittenAt is stamped with the current time.
func (s *SnapshotStore) Write(ctx context.Context, snap *Snapshot) error {
	writtenAt := snap.WrittenAt
	if writtenAt.IsZero() {
		writtenAt = time.Now()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("localdb: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{`DELETE FROM kube_snapshot_objects`, `DELETE FROM kube_snapshot_kinds`} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("localdb: clear snapshot: %w", err)
		}
	}
	ins, err := tx.PrepareContext(ctx, `INSERT INTO kube_snapshot_objects(kind, namespace, name, api_version, resource_version, body) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("localdb: prepare: %w", err)
	}
	defer func() { _ = ins.Close() }()
	counts := map[store.Kind]int{}
	for _, o := range snap.Objects {
		gvk, err := s.gvkOf(o)
		if err != nil {
			return err
		}
		body, err := json.Marshal(o)
		if err != nil {
			return fmt.Errorf("localdb: encode %s: %w", store.KeyOf(o), err)
		}
		if _, err := ins.ExecContext(ctx, gvk.Kind, o.GetNamespace(), o.GetName(), gvk.GroupVersion().String(), o.GetResourceVersion(), body); err != nil {
			return fmt.Errorf("localdb: insert %s: %w", store.KeyOf(o), err)
		}
		counts[store.Kind(gvk.Kind)]++
	}
	kinds := make([]store.Kind, 0, len(snap.ResourceVersions)+len(counts))
	seen := map[store.Kind]bool{}
	for k := range snap.ResourceVersions {
		kinds, seen[k] = append(kinds, k), true
	}
	for k := range counts {
		if !seen[k] {
			kinds = append(kinds, k)
		}
	}
	for _, k := range kinds {
		if _, err := tx.ExecContext(ctx, `INSERT INTO kube_snapshot_kinds(kind, resource_version, objects) VALUES (?, ?, ?)`,
			string(k), snap.ResourceVersions[k], counts[k]); err != nil {
			return fmt.Errorf("localdb: insert kind %s: %w", k, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO kube_snapshot(id, written_at_ns, resource_version, cluster_id, objects) VALUES (1, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET written_at_ns = excluded.written_at_ns, resource_version = excluded.resource_version,
		   cluster_id = excluded.cluster_id, objects = excluded.objects`,
		writtenAt.UnixNano(), snap.HighWaterMark(), snap.ClusterID, len(snap.Objects)); err != nil {
		return fmt.Errorf("localdb: write snapshot header: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("localdb: commit: %w", err)
	}
	return nil
}

// Touch records that the stored snapshot is still current at writtenAt
// without rewriting its objects: zonecache calls it while the live set is
// unchanged, so Snapshot.Age measures time since the set was last known
// current. It returns ErrNoSnapshot when nothing has been written yet.
func (s *SnapshotStore) Touch(ctx context.Context, writtenAt time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE kube_snapshot SET written_at_ns = ? WHERE id = 1`, writtenAt.UnixNano())
	if err != nil {
		return fmt.Errorf("localdb: touch: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("localdb: touch: %w", err)
	}
	if n == 0 {
		return ErrNoSnapshot
	}
	return nil
}

// Load reads the stored snapshot, or returns ErrNoSnapshot. Objects come
// back sorted by kind, namespace and name.
func (s *SnapshotStore) Load(ctx context.Context) (*Snapshot, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("localdb: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	snap := &Snapshot{ResourceVersions: map[store.Kind]string{}}
	var writtenNS int64
	var hwm string
	var count int
	err = tx.QueryRowContext(ctx, `SELECT written_at_ns, resource_version, cluster_id, objects FROM kube_snapshot WHERE id = 1`).
		Scan(&writtenNS, &hwm, &snap.ClusterID, &count)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoSnapshot
	}
	if err != nil {
		return nil, fmt.Errorf("localdb: read snapshot header: %w", err)
	}
	snap.WrittenAt = time.Unix(0, writtenNS)

	krows, err := tx.QueryContext(ctx, `SELECT kind, resource_version FROM kube_snapshot_kinds`)
	if err != nil {
		return nil, fmt.Errorf("localdb: read kinds: %w", err)
	}
	for krows.Next() {
		var k, rv string
		if err := krows.Scan(&k, &rv); err != nil {
			_ = krows.Close()
			return nil, err
		}
		snap.ResourceVersions[store.Kind(k)] = rv
	}
	if err := krows.Close(); err != nil {
		return nil, err
	}

	rows, err := tx.QueryContext(ctx, `SELECT kind, namespace, name, api_version, body FROM kube_snapshot_objects ORDER BY kind, namespace, name`)
	if err != nil {
		return nil, fmt.Errorf("localdb: read objects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	snap.Objects = make([]store.Object, 0, count)
	for rows.Next() {
		var kind, ns, name, apiVersion string
		var body []byte
		if err := rows.Scan(&kind, &ns, &name, &apiVersion, &body); err != nil {
			return nil, err
		}
		gvk := schema.FromAPIVersionAndKind(apiVersion, kind)
		ro, err := s.scheme.New(gvk)
		if err != nil {
			snap.Skipped = append(snap.Skipped, fmt.Sprintf("%s/%s %s/%s", apiVersion, kind, ns, name))
			continue
		}
		obj, ok := ro.(store.Object)
		if !ok {
			snap.Skipped = append(snap.Skipped, fmt.Sprintf("%s/%s %s/%s", apiVersion, kind, ns, name))
			continue
		}
		if err := json.Unmarshal(body, obj); err != nil {
			return nil, fmt.Errorf("localdb: decode %s %s/%s: %w", kind, ns, name, err)
		}
		obj.GetObjectKind().SetGroupVersionKind(gvk)
		snap.Objects = append(snap.Objects, obj)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return snap, nil
}

// Close closes the database.
func (s *SnapshotStore) Close() error { return s.db.Close() }

// gvkOf returns the object's GroupVersionKind from TypeMeta or the scheme.
func (s *SnapshotStore) gvkOf(o store.Object) (schema.GroupVersionKind, error) {
	if gvk := o.GetObjectKind().GroupVersionKind(); !gvk.Empty() && gvk.Kind != "" {
		return gvk, nil
	}
	gvks, _, err := s.scheme.ObjectKinds(o)
	if err != nil || len(gvks) == 0 {
		return schema.GroupVersionKind{}, fmt.Errorf("localdb: unknown kind for %T: %v", o, err)
	}
	return gvks[0], nil
}
