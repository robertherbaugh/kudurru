package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func TestPersistenceAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)
	s, err := Open(path, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	info := s.Info()
	created := mustApply(t, s, zone("home", "home.example."), store.ManagerFile("zones.yaml")).(*v1alpha1.DNSZone)
	created.Status.Serial = 2026101101
	if err := s.UpdateStatus(ctx, created); err != nil {
		t.Fatal(err)
	}
	mustApply(t, s, recordSet("apps", "nas", map[string]string{"team": "infra"}), store.ManagerAPI)
	rvBefore := s.ResourceVersion()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = openAt(t, path)
	if got := s.Info(); got.ClusterID != info.ClusterID || got.Epoch != info.Epoch || got.ClusterID == "" {
		t.Fatalf("cluster identity changed across reopen: %+v then %+v", info, got)
	}
	if s.ResourceVersion() != rvBefore {
		t.Fatalf("resourceVersion counter not restored: %s, want %s", s.ResourceVersion(), rvBefore)
	}
	var z v1alpha1.DNSZone
	if err := s.Get(ctx, store.Key{Kind: "DNSZone", Name: "home"}, &z); err != nil {
		t.Fatal(err)
	}
	if z.Spec.ZoneName != "home.example." || z.Status.Serial != 2026101101 || store.ManagedBy(&z) != "file:zones.yaml" ||
		z.UID != created.UID || z.Generation != 1 || z.Spec.DefaultTTL != 300 {
		t.Fatalf("object not persisted intact: %+v", z)
	}
	if z.APIVersion != "kudurru.io/v1alpha1" || z.Kind != "DNSZone" {
		t.Fatalf("TypeMeta not set: %q %q", z.APIVersion, z.Kind)
	}
	next := mustApply(t, s, recordSet("apps", "web", nil), store.ManagerAPI)
	before, _ := strconv.ParseUint(rvBefore, 10, 64)
	if got, _ := strconv.ParseUint(next.GetResourceVersion(), 10, 64); got != before+1 {
		t.Fatalf("resourceVersion after reopen = %d, want %d", got, before+1)
	}
}

func TestWALLayoutAndPragmas(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", FileName)
	s, err := Open(path, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	mustApply(t, s, zone("home", "home.example."), store.ManagerAPI)
	for _, f := range []string{path, path + "-wal", path + "-shm", path + ".lock"} {
		fi, err := os.Stat(f)
		if err != nil {
			t.Fatalf("expected %s while open: %v", filepath.Base(f), err)
		}
		if perm := fi.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("%s is readable by others: %v", filepath.Base(f), perm)
		}
	}
	if fi, err := os.Stat(filepath.Dir(path)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("data directory mode = %v (%v), want 0700", fi.Mode().Perm(), err)
	}
	for _, db := range []*sql.DB{s.w, s.r} {
		pragmas := map[string]string{"journal_mode": "wal", "synchronous": "1", "foreign_keys": "1", "busy_timeout": "5000"}
		if db == s.w {
			pragmas["synchronous"] = "2"
		}
		for p, want := range pragmas {
			var got string
			if err := db.QueryRowContext(ctx, "PRAGMA "+p).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("PRAGMA %s = %s, want %s", p, got, want)
			}
		}
	}
	var qo string
	if err := s.r.QueryRowContext(ctx, "PRAGMA query_only").Scan(&qo); err != nil || qo != "1" {
		t.Errorf("reader pool must be query_only: %q %v", qo, err)
	}
	var tables []string
	rows, err := s.r.QueryContext(ctx, `SELECT name FROM sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		tables = append(tables, n)
	}
	_ = rows.Close()
	if got := strings.Join(tables, ","); got != "audit,changes,meta,objects,replication_quarantine,replication_sections" {
		t.Errorf("tables = %s", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path + "-wal"); err == nil && fi.Size() != 0 {
		t.Errorf("WAL not checkpointed on close: %d bytes left", fi.Size())
	}
	// The database alone (no WAL) holds every committed write.
	s = openAt(t, path)
	var z v1alpha1.DNSZone
	if err := s.Get(ctx, store.Key{Kind: "DNSZone", Name: "home"}, &z); err != nil {
		t.Fatal(err)
	}
}

func TestSecondProcessIsLockedOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	s := openAt(t, path)
	if _, err := Open(path, testOptions()); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Open must be ErrLocked, got %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path, testOptions())
	if err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
	_ = s2.Close()
}

func TestSchemaNewerThanBinaryRefusesToStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	s, err := Open(path, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.w.Exec(`UPDATE meta SET value = ? WHERE key = 'schema_version'`, strconv.Itoa(store.SchemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	_, err = Open(path, testOptions())
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("expected ErrSchemaTooNew, got %v", err)
	}
	want := "state.db is at schema version " + strconv.Itoa(store.SchemaVersion+1)
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error must name the versions: %v", err)
	}
	// The refusal leaves the database untouched and the lock released.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var v string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&v); err != nil || v != strconv.Itoa(store.SchemaVersion+1) {
		t.Fatalf("schema_version changed by a refused open: %q %v", v, err)
	}
	if _, err := acquireLock(path + ".lock"); err != nil {
		t.Fatalf("refused open kept the lock: %v", err)
	}
}

func TestMigrationsCoverSchemaVersion(t *testing.T) {
	for i, m := range migrations {
		if m.version != i+1 {
			t.Fatalf("migrations[%d].version = %d, want %d", i, m.version, i+1)
		}
	}
	if last := migrations[len(migrations)-1].version; last != store.SchemaVersion {
		t.Fatalf("last migration is %d, store.SchemaVersion is %d", last, store.SchemaVersion)
	}
}

func TestMigrationFramework(t *testing.T) {
	ctx := context.Background()
	now := func() time.Time { return time.Unix(1_800_000_000, 0) }
	open := func(t *testing.T) *sql.DB {
		db, err := sql.Open("sqlite", dsn(filepath.Join(t.TempDir(), FileName), time.Second, true))
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	tableExists := func(t *testing.T, db *sql.DB, name string) bool {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = ?`, name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	schemaVersion := func(t *testing.T, db *sql.DB) string {
		var v string
		if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	step := func(v int, table string) migration {
		return migration{version: v, name: table, up: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `CREATE TABLE `+table+` (x INTEGER) STRICT`)
			return err
		}}
	}
	failing := migration{version: 3, name: "boom", up: func(context.Context, *sql.Tx) error { return errors.New("boom") }}

	t.Run("UpgradesStepByStep", func(t *testing.T) {
		db := open(t)
		if err := migrate(ctx, db, []migration{step(1, "one")}, 1, now); err != nil {
			t.Fatal(err)
		}
		var id string
		_ = db.QueryRow(`SELECT value FROM meta WHERE key = 'cluster_id'`).Scan(&id)
		if err := migrate(ctx, db, []migration{step(1, "one"), step(2, "two")}, 2, now); err != nil {
			t.Fatal(err)
		}
		if !tableExists(t, db, "two") || schemaVersion(t, db) != "2" {
			t.Fatal("migration 2 not applied")
		}
		var id2 string
		_ = db.QueryRow(`SELECT value FROM meta WHERE key = 'cluster_id'`).Scan(&id2)
		if id == "" || id != id2 {
			t.Fatalf("cluster_id must be generated once: %q then %q", id, id2)
		}
	})
	t.Run("FailedStepRollsBackEverything", func(t *testing.T) {
		db := open(t)
		if err := migrate(ctx, db, []migration{step(1, "one")}, 1, now); err != nil {
			t.Fatal(err)
		}
		err := migrate(ctx, db, []migration{step(1, "one"), step(2, "two"), failing}, 3, now)
		if err == nil || !strings.Contains(err.Error(), "migration 3 (boom)") {
			t.Fatalf("expected the failing step to be named, got %v", err)
		}
		if tableExists(t, db, "two") || schemaVersion(t, db) != "1" {
			t.Fatal("a failed upgrade must leave the database at its old version")
		}
	})
	t.Run("MissingStepIsAnError", func(t *testing.T) {
		db := open(t)
		if err := migrate(ctx, db, []migration{step(1, "one")}, 2, now); err == nil {
			t.Fatal("expected an error when no migration reaches the target")
		}
		if tableExists(t, db, "meta") {
			t.Fatal("nothing may be committed when the target is unreachable")
		}
	})
}

func TestApplyIdentityAndValidation(t *testing.T) {
	ctx := context.Background()
	fixed := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s := openTemp(t, func(o *Options) { o.Now = func() time.Time { return fixed } })

	stamped := zone("stamped", "stamped.example.")
	stamped.UID = "11111111-2222-3333-4444-555555555555"
	stamped.CreationTimestamp = metav1.NewTime(time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC))
	stamped.Generation = 42
	stamped.ResourceVersion = "999"
	out := mustApply(t, s, stamped, store.ManagerAPI)
	if out.GetUID() != stamped.UID || !out.GetCreationTimestamp().Time.Equal(stamped.CreationTimestamp.Time) || out.GetGeneration() != 1 {
		t.Fatalf("caller-stamped identity must be stored verbatim (generation is the driver's): %+v", out)
	}
	if stamped.ResourceVersion != "999" || stamped.Spec.DefaultTTL != 0 {
		t.Fatal("Apply must not modify its argument")
	}

	gen := mustApply(t, s, zone("generated", "generated.example."), store.ManagerAPI)
	if gen.GetUID() == "" || !gen.GetCreationTimestamp().Time.Equal(fixed) {
		t.Fatalf("absent identity must be generated: uid=%q created=%v", gen.GetUID(), gen.GetCreationTimestamp())
	}

	// On update the stored identity wins over whatever the caller sends.
	again := zone("stamped", "stamped.example.")
	again.UID = "other"
	again.Labels = map[string]string{"tier": "lab"}
	upd := mustApply(t, s, again, store.ManagerAPI)
	if upd.GetUID() != stamped.UID || upd.GetGeneration() != 1 {
		t.Fatalf("update must keep uid and not bump generation for a label change: %+v", upd)
	}
	if upd.GetResourceVersion() == out.GetResourceVersion() {
		t.Fatal("a metadata change is a write")
	}

	rv := s.ResourceVersion()
	bad := zone("bad", "no-trailing-dot.example")
	_, err := s.Apply(ctx, bad, store.ApplyOptions{Manager: store.ManagerAPI})
	if err == nil || !strings.Contains(err.Error(), "spec.zoneName") {
		t.Fatalf("validation must reject the zone name: %v", err)
	}
	cluster := zone("ns", "ns.example.")
	cluster.Namespace = "apps"
	if _, err := s.Apply(ctx, cluster, store.ApplyOptions{Manager: store.ManagerAPI}); err == nil {
		t.Fatal("a namespace on a cluster-scoped kind must be rejected")
	}
	if s.ResourceVersion() != rv {
		t.Fatal("rejected writes must not consume a resourceVersion")
	}
	if n := countRows(t, s, "audit"); n != 3 {
		t.Fatalf("audit rows = %d, want 3 (rejections are not store mutations)", n)
	}
}

func TestApplyExpectedVersionOnMissingObject(t *testing.T) {
	s := openTemp(t)
	_, err := s.Apply(context.Background(), zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI, ExpectedResourceVersion: "5"})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict for a precondition on a missing object, got %v", err)
	}
}

// TestReturnedObjectsMatchStoredObjects pins that what Apply returns and
// what watchers receive is exactly what a later Get decodes, including
// timestamps the JSON encoding truncates to seconds.
func TestReturnedObjectsMatchStoredObjects(t *testing.T) {
	ctx := context.Background()
	nanos := time.Date(2026, 10, 11, 12, 0, 0, 123456789, time.UTC)
	s := openTemp(t, func(o *Options) { o.Now = func() time.Time { return nanos } })
	key := store.Key{Kind: "DNSZone", Name: "home"}

	stamped := zone("stamped", "stamped.example.")
	stamped.CreationTimestamp = metav1.NewTime(nanos.Add(-time.Hour))
	for _, in := range []*v1alpha1.DNSZone{zone("home", "home.example."), stamped} {
		out := mustApply(t, s, in, store.ManagerAPI)
		var got v1alpha1.DNSZone
		if err := s.Get(ctx, store.KeyOf(in), &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(out, &got) {
			t.Fatalf("Apply returned\n%+v\nGet returned\n%+v", out, &got)
		}
	}

	w, err := s.Watch(ctx, "DNSZone", store.WatchOptions{ResourceVersion: s.ResourceVersion()})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	if ev := next(t, w); ev.Type != store.Bookmark {
		t.Fatalf("got %v", ev.Type)
	}
	fin := zone("home", "home.example.")
	fin.Finalizers = []string{"kudurru.io/publish"}
	mustApply(t, s, fin, store.ManagerAPI)
	_ = next(t, w)
	if err := s.Delete(ctx, key, store.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	ev := next(t, w)
	var got v1alpha1.DNSZone
	if err := s.Get(ctx, key, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ev.Object, &got) {
		t.Fatalf("watch event\n%+v\nGet\n%+v", ev.Object, &got)
	}
}

func TestUpdateStatusPreconditionAndNoop(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	z := mustApply(t, s, zone("home", "home.example."), store.ManagerAPI).(*v1alpha1.DNSZone)
	z.Status.Serial = 1
	if err := s.UpdateStatus(ctx, z); err != nil {
		t.Fatal(err)
	}
	// z still carries the old resourceVersion: stale.
	z.Status.Serial = 2
	if err := s.UpdateStatus(ctx, z); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale resourceVersion on UpdateStatus must be ErrConflict, got %v", err)
	}
	z.ResourceVersion = ""
	z.Status.Serial = 1
	rv := s.ResourceVersion()
	if err := s.UpdateStatus(ctx, z); err != nil {
		t.Fatal(err)
	}
	if s.ResourceVersion() != rv {
		t.Fatal("an unchanged status must not write")
	}
	if err := s.UpdateStatus(ctx, zone("missing", "missing.")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("UpdateStatus of a missing object must be ErrNotFound, got %v", err)
	}
}

// TestObjectSizeCap: an object that encodes past store.MaxObjectBytes (the
// etcd request cap) is refused with store.ErrTooLarge on Apply and on
// UpdateStatus, and nothing is written.
func TestObjectSizeCap(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	inline := func(bytes int) []string {
		entry := strings.Repeat("a", 60) + ".example.com"
		out := make([]string, 0, bytes/len(entry)+1)
		for n := 0; n < bytes; n += len(entry) + 3 {
			out = append(out, entry)
		}
		return out
	}
	list := func(bytes int) *v1alpha1.BlockList {
		return &v1alpha1.BlockList{
			ObjectMeta: metav1.ObjectMeta{Name: "big", Namespace: "default"},
			Spec:       v1alpha1.BlockListSpec{Sources: []v1alpha1.ListSource{{Inline: inline(bytes)}}},
		}
	}

	rv := s.ResourceVersion()
	if _, err := s.Apply(ctx, list(store.MaxObjectBytes), store.ApplyOptions{Manager: store.ManagerAPI}); !errors.Is(err, store.ErrTooLarge) {
		t.Fatalf("Apply past the cap: got %v, want ErrTooLarge", err)
	}
	if s.ResourceVersion() != rv {
		t.Fatal("a refused Apply moved the resourceVersion")
	}
	if err := s.Get(ctx, store.Key{Kind: store.KindOf(&v1alpha1.BlockList{}), Namespace: "default", Name: "big"}, &v1alpha1.BlockList{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a refused create was stored: %v", err)
	}

	stored := mustApply(t, s, list(store.MaxObjectBytes-64<<10), store.ManagerAPI).(*v1alpha1.BlockList)
	if _, err := s.Apply(ctx, list(store.MaxObjectBytes), store.ApplyOptions{Manager: store.ManagerAPI}); !errors.Is(err, store.ErrTooLarge) {
		t.Fatalf("Apply growing past the cap: got %v, want ErrTooLarge", err)
	}
	stored.ResourceVersion = ""
	for i := range 2_000 {
		stored.Status.Sources = append(stored.Status.Sources, v1alpha1.SourceStatus{Name: "source-" + strconv.Itoa(i), Error: strings.Repeat("e", 64)})
	}
	if err := s.UpdateStatus(ctx, stored); !errors.Is(err, store.ErrTooLarge) {
		t.Fatalf("UpdateStatus past the cap: got %v, want ErrTooLarge", err)
	}
	var got v1alpha1.BlockList
	if err := s.Get(ctx, store.KeyOf(stored), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.Sources) != 0 || len(got.Spec.Sources[0].Inline) != len(stored.Spec.Sources[0].Inline) {
		t.Fatal("a refused write changed the stored object")
	}
}

func TestFinalizers(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	key := store.Key{Kind: "DNSZone", Name: "home"}
	z := zone("home", "home.example.")
	z.Finalizers = []string{"kudurru.io/publish"}
	mustApply(t, s, z, store.ManagerAPI)

	w, err := s.Watch(ctx, "DNSZone", store.WatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	if ev := next(t, w); ev.Type != store.Added {
		t.Fatalf("got %v", ev.Type)
	}
	if ev := next(t, w); ev.Type != store.Bookmark {
		t.Fatalf("got %v", ev.Type)
	}

	if err := s.Delete(ctx, key, store.DeleteOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	var got v1alpha1.DNSZone
	if err := s.Get(ctx, key, &got); err != nil {
		t.Fatalf("an object with finalizers stays visible: %v", err)
	}
	if got.DeletionTimestamp == nil {
		t.Fatal("deletionTimestamp not set")
	}
	if ev := next(t, w); ev.Type != store.Modified || ev.Object.GetDeletionTimestamp() == nil {
		t.Fatalf("expected Modified with deletionTimestamp, got %v", ev.Type)
	}
	rv := s.ResourceVersion()
	if err := s.Delete(ctx, key, store.DeleteOptions{}); err != nil || s.ResourceVersion() != rv {
		t.Fatalf("a second delete is a no-op: %v", err)
	}

	// An apply that keeps the finalizer keeps the object (and the
	// deletionTimestamp, which the caller cannot clear).
	keep := zone("home", "home.example.")
	keep.Finalizers = []string{"kudurru.io/publish"}
	keep.Spec.DefaultTTL = 60
	out := mustApply(t, s, keep, store.ManagerAPI)
	if out.GetDeletionTimestamp() == nil {
		t.Fatal("deletionTimestamp must survive an apply")
	}
	if ev := next(t, w); ev.Type != store.Modified {
		t.Fatalf("got %v", ev.Type)
	}

	// Clearing the last finalizer removes the object.
	cleared := zone("home", "home.example.")
	cleared.Spec.DefaultTTL = 60
	if _, err := s.Apply(ctx, cleared, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	if err := s.Get(ctx, key, &got); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("object must be gone once its finalizers are cleared, got %v", err)
	}
	if ev := next(t, w); ev.Type != store.Deleted || ev.Object.GetName() != "home" {
		t.Fatalf("expected Deleted, got %v", ev.Type)
	}
	entries, err := s.ReadAudit(ctx, AuditQuery{Kind: "DNSZone", Name: "home"})
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, e := range entries {
		actions = append(actions, e.Action)
	}
	if got := strings.Join(actions, ","); got != "dnszone.create,dnszone.delete,dnszone.update,dnszone.finalize" {
		t.Fatalf("audit actions = %s", got)
	}
}

func cleanupIntegration(name, policy string) *v1alpha1.Integration {
	return &v1alpha1.Integration{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "apps"},
		Spec: v1alpha1.IntegrationSpec{
			Type:           "unifi",
			Credentials:    map[string]v1alpha1.CredentialReference{"apiKey": {SecretRef: &v1alpha1.CredentialSecretRef{Name: "unifi", Key: "token"}}},
			UniFi:          &v1alpha1.UniFiIntegration{Endpoint: "https://network.example"},
			DeletionPolicy: policy,
		},
	}
}

// TestFinalizersReservedIntegrationCleanup: admission decides the cleanup
// finalizer of an Integration from its spec (D367). The caller's document
// can neither set nor strip it, so a Delete policy cannot silently become
// Retain through an edited or re-declared document.
func TestFinalizersReservedIntegrationCleanup(t *testing.T) {
	ctx := context.Background()
	const reserved = store.IntegrationCleanupFinalizer
	file := store.ManagerFile("integrations.yaml")
	key := store.Key{Kind: "Integration", Namespace: "apps", Name: "cleanup"}
	get := func(t *testing.T, s *Store) *v1alpha1.Integration {
		t.Helper()
		var got v1alpha1.Integration
		if err := s.Get(ctx, key, &got); err != nil {
			t.Fatal(err)
		}
		return &got
	}
	// objectEvent is the next object event of a watch resumed at a
	// resourceVersion, past its bookmark.
	objectEvent := func(t *testing.T, w store.Watcher) store.Event {
		t.Helper()
		for {
			if ev := next(t, w); ev.Type != store.Bookmark {
				return ev
			}
		}
	}

	t.Run("ArmedInTheSameWrite", func(t *testing.T) {
		s := openTemp(t)
		before := s.ResourceVersion()
		out := mustApply(t, s, cleanupIntegration("cleanup", "Delete"), file)
		if !reflect.DeepEqual(out.GetFinalizers(), []string{reserved}) {
			t.Fatalf("finalizers = %v", out.GetFinalizers())
		}
		was, _ := strconv.ParseUint(before, 10, 64)
		if now, _ := strconv.ParseUint(s.ResourceVersion(), 10, 64); now != was+1 || out.GetResourceVersion() != s.ResourceVersion() {
			t.Fatalf("arming took %d writes", now-was)
		}
		entries, err := s.ReadAudit(ctx, AuditQuery{Kind: "Integration", Name: "cleanup"})
		if err != nil || len(entries) != 1 || entries[0].Action != "integration.create" || !strings.Contains(string(entries[0].After), reserved) {
			t.Fatalf("the create audit row must carry the armed object: %+v %v", entries, err)
		}
		// The default policy and an explicit Retain are never armed, even when
		// the document lists the reserved name itself.
		for name, policy := range map[string]string{"defaulted": "", "retained": "Retain"} {
			claimed := cleanupIntegration(name, policy)
			claimed.Finalizers = []string{reserved}
			if got := mustApply(t, s, claimed, file); len(got.GetFinalizers()) != 0 || got.(*v1alpha1.Integration).Spec.DeletionPolicy != "Retain" {
				t.Fatalf("%s: finalizers=%v policy=%q", name, got.GetFinalizers(), got.(*v1alpha1.Integration).Spec.DeletionPolicy)
			}
		}
		// Other kinds keep ordinary caller-owned finalizers under that name.
		z := zone("home", "home.example.")
		z.Finalizers = []string{reserved}
		if got := mustApply(t, s, z, file); !reflect.DeepEqual(got.GetFinalizers(), []string{reserved}) {
			t.Fatalf("zone finalizers = %v", got.GetFinalizers())
		}
	})

	t.Run("NoApplyStripsIt", func(t *testing.T) {
		s := openTemp(t)
		armed := mustApply(t, s, cleanupIntegration("cleanup", "Delete"), file)
		// Identical document without finalizers: nothing is written.
		same := mustApply(t, s, cleanupIntegration("cleanup", "Delete"), file)
		if same.GetResourceVersion() != armed.GetResourceVersion() || !store.HasFinalizer(same, reserved) {
			t.Fatalf("identical re-apply wrote: rv %s then %s, finalizers %v", armed.GetResourceVersion(), same.GetResourceVersion(), same.GetFinalizers())
		}
		// The document's own finalizers are kept, the reserved one stays last.
		own := cleanupIntegration("cleanup", "Delete")
		own.Finalizers = []string{reserved, "kudurru.io/operator-hold"}
		if got := mustApply(t, s, own, file); !reflect.DeepEqual(got.GetFinalizers(), []string{"kudurru.io/operator-hold", reserved}) {
			t.Fatalf("finalizers = %v", got.GetFinalizers())
		}
		edited := cleanupIntegration("cleanup", "Delete")
		edited.Spec.Suspend = true
		if got := mustApply(t, s, edited, file); !reflect.DeepEqual(got.GetFinalizers(), []string{reserved}) || got.GetGeneration() != 2 {
			t.Fatalf("edited apply: finalizers=%v generation=%d", got.GetFinalizers(), got.GetGeneration())
		}
		forced, err := s.Apply(ctx, cleanupIntegration("cleanup", "Delete"), store.ApplyOptions{Manager: store.ManagerAPI, Force: true})
		if err != nil {
			t.Fatal(err)
		}
		if !store.HasFinalizer(forced, reserved) || store.ManagedBy(forced) != store.ManagerAPI {
			t.Fatalf("Force takeover: finalizers=%v managedBy=%q", forced.GetFinalizers(), store.ManagedBy(forced))
		}

		if err := s.Delete(ctx, key, store.DeleteOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
		deleting := get(t, s)
		if deleting.DeletionTimestamp == nil || !store.HasFinalizer(deleting, reserved) {
			t.Fatalf("delete of an armed Integration: deletionTimestamp=%v finalizers=%v", deleting.DeletionTimestamp, deleting.Finalizers)
		}
		// Applies during termination, Force included, neither finalize nor
		// clear the deletion.
		for _, force := range []bool{false, true} {
			redeclared := cleanupIntegration("cleanup", "Delete")
			redeclared.Spec.Suspend = !force
			out, err := s.Apply(ctx, redeclared, store.ApplyOptions{Manager: store.ManagerAPI, Force: force})
			if err != nil {
				t.Fatal(err)
			}
			if out.GetDeletionTimestamp() == nil || !store.HasFinalizer(out, reserved) {
				t.Fatalf("apply during termination (force=%v): deletionTimestamp=%v finalizers=%v", force, out.GetDeletionTimestamp(), out.GetFinalizers())
			}
		}
		if got := get(t, s); got.UID != armed.GetUID() || got.DeletionTimestamp == nil {
			t.Fatal("the terminating object was replaced or revived")
		}
	})

	t.Run("RetainDisarmsAndReleases", func(t *testing.T) {
		s := openTemp(t)
		mustApply(t, s, cleanupIntegration("cleanup", "Delete"), file)
		// Live: the change to Retain removes the obligation with the spec.
		retained := mustApply(t, s, cleanupIntegration("cleanup", "Retain"), file)
		if len(retained.GetFinalizers()) != 0 || retained.GetGeneration() != 2 {
			t.Fatalf("Retain on a live object: finalizers=%v generation=%d", retained.GetFinalizers(), retained.GetGeneration())
		}
		// And back: armed again in the write that changes the spec.
		rearmed := mustApply(t, s, cleanupIntegration("cleanup", "Delete"), file)
		if !store.HasFinalizer(rearmed, reserved) || rearmed.GetGeneration() != 3 {
			t.Fatalf("re-arming: finalizers=%v generation=%d", rearmed.GetFinalizers(), rearmed.GetGeneration())
		}
		w, err := s.Watch(ctx, "Integration", store.WatchOptions{ResourceVersion: s.ResourceVersion()})
		if err != nil {
			t.Fatal(err)
		}
		defer w.Stop()
		if err := s.Delete(ctx, key, store.DeleteOptions{Manager: file}); err != nil {
			t.Fatal(err)
		}
		if ev := objectEvent(t, w); ev.Type != store.Modified || ev.Object.GetDeletionTimestamp() == nil {
			t.Fatalf("expected Modified with deletionTimestamp, got %v", ev.Type)
		}
		// Terminating: the change to Retain is the release.
		released := mustApply(t, s, cleanupIntegration("cleanup", "Retain"), file)
		if !store.Terminated(released) {
			t.Fatalf("release did not report the removed object: %+v", released)
		}
		if ev := objectEvent(t, w); ev.Type != store.Deleted || ev.Object.GetUID() != rearmed.GetUID() {
			t.Fatalf("expected Deleted, got %v", ev.Type)
		}
		var gone v1alpha1.Integration
		if err := s.Get(ctx, key, &gone); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("released object: %v", err)
		}
		entries, err := s.ReadAudit(ctx, AuditQuery{Kind: "Integration", Name: "cleanup"})
		if err != nil {
			t.Fatal(err)
		}
		var actions []string
		for _, e := range entries {
			actions = append(actions, e.Action)
		}
		if got := strings.Join(actions, ","); got != "integration.create,integration.update,integration.update,integration.delete,integration.finalize" {
			t.Fatalf("audit actions = %s", got)
		}

		// With another finalizer left the object stays, and a later Delete
		// policy cannot arm an object that is already being deleted.
		held := cleanupIntegration("held", "Delete")
		held.Finalizers = []string{"kudurru.io/operator-hold"}
		mustApply(t, s, held, file)
		heldKey := store.Key{Kind: "Integration", Namespace: "apps", Name: "held"}
		if err := s.Delete(ctx, heldKey, store.DeleteOptions{Manager: file}); err != nil {
			t.Fatal(err)
		}
		toRetain := cleanupIntegration("held", "Retain")
		toRetain.Finalizers = []string{"kudurru.io/operator-hold"}
		if got := mustApply(t, s, toRetain, file); !reflect.DeepEqual(got.GetFinalizers(), []string{"kudurru.io/operator-hold"}) || got.GetDeletionTimestamp() == nil {
			t.Fatalf("release with another finalizer: %v", got.GetFinalizers())
		}
		if got := mustApply(t, s, held, file); !reflect.DeepEqual(got.GetFinalizers(), []string{"kudurru.io/operator-hold"}) {
			t.Fatalf("an object being deleted was armed again: %v", got.GetFinalizers())
		}
	})

	t.Run("MetadataOnlyRemoval", func(t *testing.T) {
		s := openTemp(t)
		created := mustApply(t, s, cleanupIntegration("cleanup", "Delete"), file).(*v1alpha1.Integration)
		created.Status.Clients = 7
		created.ResourceVersion = ""
		if err := s.UpdateStatus(ctx, created); err != nil {
			t.Fatal(err)
		}
		live := get(t, s)
		opts := func(uid, rv string, remove bool, name string) store.FinalizerOptions {
			return store.FinalizerOptions{Finalizer: name, Remove: remove, ExpectedUID: uid, ExpectedResourceVersion: rv}
		}
		for name, o := range map[string]store.FinalizerOptions{
			"stale resourceVersion": opts(string(live.UID), created.ResourceVersion+"0", true, reserved),
			"wrong UID":             opts("another-incarnation", live.ResourceVersion, true, reserved),
		} {
			if _, err := s.UpdateFinalizer(ctx, key, o); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("%s: got %v, want ErrConflict", name, err)
			}
		}
		for name, o := range map[string]store.FinalizerOptions{
			"no UID":             opts("", live.ResourceVersion, true, reserved),
			"no resourceVersion": opts(string(live.UID), "", true, reserved),
			"unqualified name":   opts(string(live.UID), live.ResourceVersion, false, "cleanup"),
		} {
			if _, err := s.UpdateFinalizer(ctx, key, o); err == nil || errors.Is(err, store.ErrConflict) {
				t.Fatalf("%s: got %v, want a validation error", name, err)
			}
		}
		if _, err := s.UpdateFinalizer(ctx, store.Key{Kind: "Integration", Namespace: "apps", Name: "absent"}, opts("uid", "1", true, reserved)); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing object: %v", err)
		}
		if got := get(t, s); got.ResourceVersion != live.ResourceVersion {
			t.Fatal("a refused finalizer change wrote")
		}
		// Adding another name writes metadata.finalizers and nothing else.
		added, err := s.UpdateFinalizer(ctx, key, opts(string(live.UID), live.ResourceVersion, false, "kudurru.io/operator-hold"))
		if err != nil {
			t.Fatal(err)
		}
		want := live.DeepCopy()
		want.Finalizers = []string{reserved, "kudurru.io/operator-hold"}
		want.ResourceVersion = added.GetResourceVersion()
		if !reflect.DeepEqual(get(t, s), want) || added.GetResourceVersion() == live.ResourceVersion {
			t.Fatalf("the finalizer write changed more than metadata.finalizers:\n got %+v\nwant %+v", get(t, s), want)
		}
		// Already present: nothing is written.
		if again, err := s.UpdateFinalizer(ctx, key, opts(string(live.UID), added.GetResourceVersion(), false, reserved)); err != nil || again.GetResourceVersion() != added.GetResourceVersion() {
			t.Fatalf("no-op add: %v", err)
		}
		// Removing the reserved name from a live object leaves it live. (The
		// cleanup owner only does this for an object being deleted; the next
		// apply of the unchanged Delete policy arms it again.)
		disarmed, err := s.UpdateFinalizer(ctx, key, opts(string(live.UID), added.GetResourceVersion(), true, reserved))
		if err != nil || store.HasFinalizer(disarmed, reserved) || disarmed.GetDeletionTimestamp() != nil {
			t.Fatalf("live removal: %v %v", err, disarmed)
		}
		if got := mustApply(t, s, cleanupIntegration("cleanup", "Delete"), file); !store.HasFinalizer(got, reserved) {
			t.Fatalf("the next apply did not arm the Delete policy again: %v", got.GetFinalizers())
		}

		w, err := s.Watch(ctx, "Integration", store.WatchOptions{ResourceVersion: s.ResourceVersion()})
		if err != nil {
			t.Fatal(err)
		}
		defer w.Stop()
		if err := s.Delete(ctx, key, store.DeleteOptions{Manager: file}); err != nil {
			t.Fatal(err)
		}
		if ev := objectEvent(t, w); ev.Type != store.Modified {
			t.Fatalf("got %v", ev.Type)
		}
		deleting := get(t, s)
		if _, err := s.UpdateFinalizer(ctx, key, opts(string(deleting.UID), deleting.ResourceVersion, false, "kudurru.io/late")); !errors.Is(err, store.ErrTerminating) {
			t.Fatalf("adding during termination: got %v, want ErrTerminating", err)
		}
		if _, err := s.UpdateFinalizer(ctx, key, opts(string(deleting.UID), live.ResourceVersion, true, reserved)); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("stale release: got %v, want ErrConflict", err)
		}
		removed, err := s.UpdateFinalizer(ctx, key, opts(string(deleting.UID), deleting.ResourceVersion, true, reserved))
		if err != nil || !store.Terminated(removed) {
			t.Fatalf("release: %v %+v", err, removed)
		}
		if ev := objectEvent(t, w); ev.Type != store.Deleted || ev.Object.GetUID() != live.UID || ev.Object.(*v1alpha1.Integration).Status.Clients != 7 {
			t.Fatalf("expected Deleted with the last stored object, got %v", ev.Type)
		}
		var gone v1alpha1.Integration
		if err := s.Get(ctx, key, &gone); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("released object: %v", err)
		}
		entries, err := s.ReadAudit(ctx, AuditQuery{Kind: "Integration", Name: "cleanup"})
		if err != nil {
			t.Fatal(err)
		}
		last := entries[len(entries)-1]
		if last.Action != "integration.finalize" || last.Reason != "removed "+reserved || last.Manager != file || len(last.After) != 0 {
			t.Fatalf("finalize audit row = %+v", last)
		}
	})

	t.Run("CommandTransaction", func(t *testing.T) {
		s := openTemp(t)
		armed := mustApply(t, s, cleanupIntegration("cleanup", "Delete"), file)
		if err := s.Delete(ctx, key, store.DeleteOptions{Manager: file}); err != nil {
			t.Fatal(err)
		}
		deleting := get(t, s)
		release := store.FinalizerOptions{Finalizer: reserved, Remove: true, ExpectedUID: string(armed.GetUID()), ExpectedResourceVersion: deleting.ResourceVersion}
		refused := errors.New("later step refused")
		// The release shares the command's fate: a later failure keeps the
		// obligation, and a stale identity fails the whole command.
		err := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
			out, err := tx.UpdateFinalizer(ctx, key, release)
			if err != nil || !store.Terminated(out) {
				t.Errorf("staged release: %v", err)
			}
			if err := tx.Get(ctx, key, &v1alpha1.Integration{}); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("the transaction still reads the released object: %v", err)
			}
			return refused
		})
		if !errors.Is(err, refused) {
			t.Fatal(err)
		}
		if got := get(t, s); got.ResourceVersion != deleting.ResourceVersion || !store.HasFinalizer(got, reserved) {
			t.Fatal("a rolled-back command released the object")
		}
		stale := release
		stale.ExpectedResourceVersion = armed.GetResourceVersion()
		err = s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
			_, err := tx.UpdateFinalizer(ctx, key, stale)
			if !errors.Is(err, store.ErrConflict) {
				t.Errorf("stale release in a command: %v", err)
			}
			return nil // an ignored refusal still fails the command
		})
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("command with a refused release: %v", err)
		}
		marker := zone("released", "released.example.")
		err = s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
			if _, err := tx.UpdateFinalizer(ctx, key, release); err != nil {
				return err
			}
			_, err := tx.Apply(ctx, marker, store.ApplyOptions{Manager: store.ManagerSystem})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		var gone v1alpha1.Integration
		if err := s.Get(ctx, key, &gone); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("released object: %v", err)
		}
		if err := s.Get(ctx, store.Key{Kind: "DNSZone", Name: "released"}, &v1alpha1.DNSZone{}); err != nil {
			t.Fatalf("the command's other write: %v", err)
		}
	})

	t.Run("SurvivesReopenAndRestore", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), FileName)
		s := openAt(t, path)
		armed := mustApply(t, s, cleanupIntegration("cleanup", "Delete"), file)
		if err := s.Delete(ctx, key, store.DeleteOptions{Manager: file}); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		s = openAt(t, path)
		got := get(t, s)
		if got.UID != armed.GetUID() || got.DeletionTimestamp == nil || !store.HasFinalizer(got, reserved) {
			t.Fatalf("the terminating object did not survive a restart: %+v", got.ObjectMeta)
		}
		// A safe export carries no finalizers; importing it arms the policy.
		fresh := openTemp(t)
		exported := cleanupIntegration("cleanup", "Delete")
		exported.TypeMeta = metav1.TypeMeta{APIVersion: "kudurru.io/v1alpha1", Kind: "Integration"}
		raw, err := json.Marshal(exported)
		if err != nil {
			t.Fatal(err)
		}
		if err := fresh.Restore(ctx, strings.NewReader(string(raw))); err != nil {
			t.Fatal(err)
		}
		if got := get(t, fresh); !store.HasFinalizer(got, reserved) {
			t.Fatalf("restore did not arm the Delete policy: %v", got.Finalizers)
		}
	})
}

func TestErrorsAndClosedStore(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	if _, err := s.List(ctx, "Nope", store.ListOptions{}); err == nil {
		t.Fatal("unknown kind must be an error")
	}
	if err := s.Get(ctx, store.Key{Kind: "DNSZone", Name: "x"}, &v1alpha1.Policy{}); err == nil {
		t.Fatal("Get into the wrong type must be an error")
	}
	if _, err := s.List(ctx, "DNSZone", store.ListOptions{Continue: "garbage"}); err == nil {
		t.Fatal("a bad continue token must be an error")
	}
	page, err := s.List(ctx, "DNSZone", store.ListOptions{})
	if err != nil || page.Items == nil || len(page.Items) != 0 {
		t.Fatalf("empty list: %+v %v", page, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close must be idempotent: %v", err)
	}
	if _, err := s.Apply(ctx, zone("a", "a."), store.ApplyOptions{Manager: store.ManagerAPI}); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("Apply after Close: %v", err)
	}
	if err := s.Get(ctx, store.Key{Kind: "DNSZone", Name: "a"}, &v1alpha1.DNSZone{}); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("Get after Close: %v", err)
	}
	if _, err := s.Watch(ctx, "DNSZone", store.WatchOptions{}); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("Watch after Close: %v", err)
	}
}

func TestElectorAlwaysLeads(t *testing.T) {
	s := openTemp(t)
	e := s.Elector("controller")
	if !e.IsLeader() || e.Leader() != "test-node" || e.(*Elector).Name() != "controller" {
		t.Fatalf("elector: leader=%v %q", e.IsLeader(), e.Leader())
	}
	ctx, cancel := context.WithCancel(context.Background())
	started, stopped := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx, func(context.Context) { close(started) }, func() { close(stopped) }) }()
	<-started
	cancel()
	<-stopped
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if s.Info().Leader != "test-node" || s.Info().Driver != "sqlite" || s.Info().SchemaVersion != store.SchemaVersion {
		t.Fatalf("info: %+v", s.Info())
	}
}

func countRows(t testing.TB, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.r.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
