package sqlite

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// The files under testdata/v0.1.0 are state.db as the released 0.1.0 binary
// wrote it (testdata/v0.1.0/README.md): real bytes of the only release this
// one supports upgrading from (D373), not rows written by today's code at an
// old schema number.

// releasedV01 unpacks a golden state.db of the 0.1.0 release into a new
// directory and returns its path.
func releasedV01(t *testing.T, name string) string {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "v0.1.0", name))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(io.LimitReader(zr, 64<<20))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// productionOptions is the registry `kudurru serve` opens state.db with:
// every public and private kind of the server.
func productionOptions(o *Options) {
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		panic(err)
	}
	o.Scheme = scheme
	o.InternalKinds = nil
	for _, k := range runtimekinds.Private() {
		o.InternalKinds = append(o.InternalKinds, store.Kind(k))
	}
}

// rawV01 opens a golden file without migrating it and checks that it is a
// 0.1.0 database: schema 1 and no table of a later schema.
func rawV01(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(path, time.Second, true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if got := rowsOf(t, db, `SELECT value FROM meta WHERE key = 'schema_version'`); !slices.Equal(got, []string{"1"}) {
		t.Fatalf("the golden file is at schema %v, want 1 (0.1.0)", got)
	}
	if got := rowsOf(t, db, `SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`); !slices.Equal(got, []string{"audit", "changes", "meta", "objects", "sqlite_sequence"}) {
		t.Fatalf("the golden file has the tables %v, not those of 0.1.0", got)
	}
	return db
}

func fileSHA256(t *testing.T, path string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // the test's file
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

// publicRows are the rows of the kinds whose stored form did not change
// between the releases: the upgrade must leave their documents alone.
const publicRows = `SELECT kind, namespace, name, uid, generation, resource_version, managed_by, data FROM objects
	WHERE kind IN ('DNSZone', 'DNSRecordSet', 'BlockList', 'ClientGroup', 'Policy') ORDER BY kind, namespace, name`

// TestOpenReleasedV01Golden opens a state.db written by the released 0.1.0
// binary with the production registry: every migration step runs on real
// 0.1.0 bytes, every object 0.1.0 stored is valid in this release, the
// public objects keep their exact documents, the accounts and tokens become
// the identities of this release, the audit trail of 0.1.0 verifies and
// continues, and a second open changes nothing.
func TestOpenReleasedV01Golden(t *testing.T) {
	ctx := context.Background()
	path := releasedV01(t, "state.db.gz")
	raw := rawV01(t, path)
	public := rowsOf(t, raw, publicRows)
	identity := rowsOf(t, raw, `SELECT key, value FROM meta WHERE key IN ('cluster_id', 'cluster_epoch', 'created_at') ORDER BY key`)
	kinds := rowsOf(t, raw, `SELECT kind, count(*) FROM objects GROUP BY kind ORDER BY kind`)
	audit := rowsOf(t, raw, `SELECT id, ts, actor, action, kind, name, prev_hash, hash FROM audit ORDER BY id`)
	head := rowsOf(t, raw, `SELECT value FROM meta WHERE key = 'resource_version'`)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	// What the fixture must contain to be worth opening.
	for _, want := range []string{"APIToken 3", "BlockList 4", "ClientGroup 2", "DNSRecordSet 3", "DNSZone 4", "LocalUser 1", "Policy 1", "ServiceAccount 2"} {
		if !slices.Contains(kinds, want) {
			t.Fatalf("the golden file holds %v, want %q among them", kinds, want)
		}
	}
	if len(audit) < 30 {
		t.Fatalf("the golden file holds %d audit rows", len(audit))
	}

	s := openAt(t, path, productionOptions)
	if info := s.Info(); info.SchemaVersion != store.SchemaVersion {
		t.Fatalf("schema %d after the upgrade, want %d", info.SchemaVersion, store.SchemaVersion)
	}
	upgraded := func(query string) []string {
		t.Helper()
		db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		return rowsOf(t, db, query)
	}
	if got := upgraded(publicRows); !slices.Equal(got, public) {
		for i := range min(len(got), len(public)) {
			if got[i] != public[i] {
				t.Errorf("the upgrade changed a public object:\n was %s\n now %s", public[i], got[i])
			}
		}
		t.Fatalf("%d public objects after the upgrade, %d before", len(got), len(public))
	}
	if got := upgraded(`SELECT key, value FROM meta WHERE key IN ('cluster_id', 'cluster_epoch', 'created_at') ORDER BY key`); !slices.Equal(got, identity) {
		t.Fatalf("the upgrade changed the identity of the store: %v, was %v", got, identity)
	}
	if got := upgraded(`SELECT id, ts, actor, action, kind, name, prev_hash, hash FROM audit ORDER BY id`); !slices.Equal(got[:len(audit)], audit) {
		t.Fatal("the upgrade rewrote the audit rows of 0.1.0")
	}
	before, _ := strconv.ParseUint(head[0], 10, 64)
	if s.head.Load() < before {
		t.Fatalf("resourceVersion went back from %d to %d", before, s.head.Load())
	}

	// The objects through the typed store, as this release reads them.
	var apiZone v1alpha1.DNSZone
	if err := s.Get(ctx, store.Key{Kind: "DNSZone", Name: "api-test"}, &apiZone); err != nil || apiZone.Spec.ZoneName != "api.test." || apiZone.Status.Serial == 0 || store.ManagedBy(&apiZone) != store.ManagerAPI {
		t.Fatalf("DNSZone api-test: %+v %v", apiZone.Spec, err)
	}
	var fileZone v1alpha1.DNSZone
	if err := s.Get(ctx, store.Key{Kind: "DNSZone", Name: "file-test"}, &fileZone); err != nil || !strings.HasPrefix(store.ManagedBy(&fileZone), "file:") {
		t.Fatalf("DNSZone file-test: managed by %q, %v", store.ManagedBy(&fileZone), err)
	}
	// 0.1.0 defaulted and stored refreshInterval 24h; it stays an explicit value.
	var starter v1alpha1.BlockList
	if err := s.Get(ctx, store.Key{Kind: "BlockList", Namespace: "default", Name: "starter"}, &starter); err != nil || starter.Spec.RefreshInterval.Duration != 24*time.Hour || len(starter.Spec.Sources) == 0 {
		t.Fatalf("BlockList starter: refreshInterval %s, %d sources, %v", starter.Spec.RefreshInterval.Duration, len(starter.Spec.Sources), err)
	}
	// Accounts and tokens: the kinds of this release, the same names.
	after := upgraded(`SELECT kind, count(*) FROM objects GROUP BY kind ORDER BY kind`)
	for _, want := range []string{"APIToken 3", "LocalUser 1", "ServiceAccount 2", "PrincipalState 2"} {
		if !slices.Contains(after, want) {
			t.Errorf("after the upgrade the store holds %v, want %q among them", after, want)
		}
	}
	for _, sa := range []store.Key{{Kind: "ServiceAccount", Namespace: "kudurru-system", Name: "break-glass"}, {Kind: "ServiceAccount", Namespace: "kudurru-system", Name: "bootstrap"}} {
		var account v1alpha1.ServiceAccount
		if err := s.Get(ctx, sa, &account); err != nil || account.UID == "" {
			t.Errorf("%s: %v", sa, err)
		}
	}

	// Everything 0.1.0 stored is an object of this release: the complete
	// image of the upgraded store validates.
	if _, err := s.ValidateImage(ctx, *imageOf(t, s)); err != nil {
		t.Fatalf("the upgraded store holds an image that is not valid: %v", err)
	}
	// The audit trail of 0.1.0 (format 1) verifies and continues (format 2).
	if err := s.VerifyAudit(ctx); err != nil {
		t.Fatalf("the audit trail of 0.1.0 does not verify: %v", err)
	}
	mustApply(t, s, zone("after", "after.example."), store.ManagerAPI)
	rows, err := s.ReadAudit(ctx, AuditQuery{})
	if err != nil || len(rows) != len(audit)+1 {
		t.Fatalf("audit rows after one write: %d, want %d (%v)", len(rows), len(audit)+1, err)
	}
	slices.SortFunc(rows, func(a, b AuditEntry) int { return int(a.ID - b.ID) })
	if first, last := rows[0], rows[len(rows)-1]; first.ChainVersion != 1 || last.ChainVersion != 2 || last.PrevHash != rows[len(rows)-2].Hash {
		t.Fatalf("the audit chain does not continue across the upgrade: formats %d and %d", first.ChainVersion, last.ChainVersion)
	}
	if err := s.VerifyAudit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// A second open has nothing to migrate and changes nothing.
	objects := upgraded(`SELECT kind, namespace, name, resource_version, data FROM objects ORDER BY kind, namespace, name`)
	again := openAt(t, path, productionOptions)
	if err := again.VerifyAudit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
	if got := upgraded(`SELECT kind, namespace, name, resource_version, data FROM objects ORDER BY kind, namespace, name`); !slices.Equal(got, objects) {
		t.Fatal("the second open changed stored objects")
	}
	// And the schema of 0.1.0 is behind: that release refuses the file.
	db, err := sql.Open("sqlite", dsn(path, time.Second, true))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := migrate(ctx, db, migrations[:1], 1, time.Now); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("a schema-1 binary on the upgraded file: %v", err)
	}
}

// TestOpenReleasedV01LegacyGoldenIsRefused: the second golden file is the
// same installation with the objects 0.1.0 accepted and this release cannot
// serve. Opening it is refused, names exactly those objects, and leaves the
// file byte for byte as 0.1.0 wrote it.
func TestOpenReleasedV01LegacyGoldenIsRefused(t *testing.T) {
	path := releasedV01(t, "state-legacy.db.gz")
	raw := rawV01(t, path)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	sum := fileSHA256(t, path)

	_, err := Open(path, testOptions(productionOptions))
	var upgrade *UpgradeError
	if !errors.As(err, &upgrade) || !errors.Is(err, ErrUpgradeRefused) {
		t.Fatalf("Open = %v, want ErrUpgradeRefused", err)
	}
	var keys []string
	for _, issue := range upgrade.Issues {
		keys = append(keys, issue.Key.String())
	}
	want := []string{"DNSZone/api-test", "DNSZone/legacy-duplicate", "DNSZone/legacy-external", "DNSZone/legacy-secondary", "DNSZone/legacy-stub", "Policy/default/default"}
	if !slices.Equal(keys, want) {
		t.Fatalf("refused objects %v, want %v\n%v", keys, want, err)
	}
	if upgrade.From != 1 || upgrade.To != store.SchemaVersion {
		t.Fatalf("refusal of an upgrade from %d to %d", upgrade.From, upgrade.To)
	}
	if got := fileSHA256(t, path); !bytes.Equal(got[:], sum[:]) {
		t.Fatal("the refused upgrade changed state.db")
	}
	lock, err := acquireLock(path + ".lock")
	if err != nil {
		t.Fatalf("the refused upgrade kept the lock: %v", err)
	}
	_ = lock.release()
}
