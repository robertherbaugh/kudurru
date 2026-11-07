package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func anchorLegacyDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), FileName)
	db, err := sql.Open("sqlite", dsn(path, time.Second, true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := migrate(t.Context(), db, migrations[:6], 6, time.Now); err != nil {
		t.Fatal(err)
	}
	return db, path
}

// Keep whole persisted values in private assertions. Diagnostics identify only
// the table/stage; a failure must not disclose opaque section or audit contents.
func anchorDBRows(t *testing.T, db *sql.DB, omitSentinel bool) map[string][]string {
	t.Helper()
	queries := map[string]string{
		"meta":       `SELECT json_array(key,value) FROM meta WHERE key <> 'schema_version' ORDER BY key`,
		"objects":    `SELECT json_array(kind,namespace,name,uid,generation,resource_version,managed_by,created_at,updated_at,spec_hash,labels,deleting,data) FROM objects ORDER BY kind,namespace,name`,
		"changes":    `SELECT json_array(seq,kind,namespace,name,type,resource_version,tombstone) FROM changes ORDER BY seq`,
		"audit":      `SELECT json_array(id,ts,actor,manager,action,kind,namespace,name,resource_version,result,reason,before,after,prev_hash,hash,chain_version,authz,request_id,client_ip,user_agent,request_method,request_path) FROM audit ORDER BY id`,
		"sections":   `SELECT json_array(section,key,data) FROM replication_sections ORDER BY section,key`,
		"quarantine": `SELECT json_array(generation,image) FROM replication_quarantine ORDER BY generation`,
	}
	if omitSentinel {
		queries["sections"] = `SELECT json_array(section,key,data) FROM replication_sections WHERE NOT(section='runtime' AND key='trustanchors/v1/root') ORDER BY section,key`
	}
	out := map[string][]string{}
	for table, query := range queries {
		rows, err := db.QueryContext(t.Context(), query)
		if err != nil {
			t.Fatal("read preserved table", table, err)
		}
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				t.Fatal("read preserved row", table, err)
			}
			out[table] = append(out[table], raw)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			t.Fatal("finish preserved table", table, err)
		}
	}
	return out
}

func requireAnchorSchema(t *testing.T, db *sql.DB, want string) {
	t.Helper()
	var got string
	if err := db.QueryRowContext(t.Context(), `SELECT value FROM meta WHERE key=?`, metaSchemaVersion).Scan(&got); err != nil || got != want {
		t.Fatal("unexpected physical schema marker", err)
	}
}

func requireAnchorSentinel(t *testing.T, db *sql.DB) {
	t.Helper()
	var data string
	if err := db.QueryRowContext(t.Context(), `SELECT data FROM replication_sections WHERE section=? AND key=?`, accepted.Section, accepted.RootKey).Scan(&data); err != nil || data != accepted.AwaitingOwnerJSON {
		t.Fatal("missing exact physical sentinel", err)
	}
}

func TestTrustAnchorMigrationFreshAndLegacyPreserveBytes(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "schema6"}[legacy], func(t *testing.T) {
			db, path := anchorLegacyDB(t)
			if !legacy {
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(t.TempDir(), FileName)
				var err error
				db, err = sql.Open("sqlite", dsn(path, time.Second, true))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = db.Close() })
				if err := migrate(t.Context(), db, migrations[:7], 7, time.Now); err != nil {
					t.Fatal(err)
				}
			} else {
				z := zone("preserved", "preserved.example.")
				z.SetCreationTimestamp(metav1.NewTime(time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)))
				z.Status.Serial = 2026101501
				insertLegacyCredential(t, db, z, 41)
				// Durable writer metadata and an old change-feed entry are also
				// byte-preserved by this physical step; neither is a new grant.
				tx, err := db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				term := replication.Term{ClusterID: "legacy-cluster", Epoch: 2, PrimaryID: "legacy-primary", PromotionID: "legacy-promotion"}
				authority := replication.Authority{MemberID: term.PrimaryID, Term: term, Mode: replication.Primary, Incarnation: 4, MaxObservedEpoch: 2, Installed: replication.Generation{Term: term, Revision: 9}, WatchLineage: "legacy-lineage"}
				if err := writeAuthority(t.Context(), tx, authority); err != nil {
					_ = tx.Rollback()
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(t.Context(), `INSERT INTO changes(seq,kind,namespace,name,type,resource_version,tombstone) VALUES (41,'DNSZone','','preserved','ADDED','41',NULL)`); err != nil {
					_ = tx.Rollback()
					t.Fatal(err)
				}
				audit := AuditEntry{Time: time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC), Actor: "operator", Manager: store.ManagerAPI, Action: "dnszone.update", Key: store.Key{Kind: "DNSZone", Name: "preserved"}, ResourceVersion: "41", Result: AuditSuccess, Reason: "original", RequestID: "original-request", ClientIP: "192.0.2.1", RequestMethod: "PUT", RequestPath: "/fixture"}
				if err := insertAudit(t.Context(), tx, &audit); err != nil {
					_ = tx.Rollback()
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				for _, section := range []string{"runtime", "security", "membership", "dedup"} {
					key := "foreign/original"
					if section == "security" {
						key = accepted.RootKey
					}
					if _, err := db.ExecContext(t.Context(), `INSERT INTO replication_sections(section,key,data) VALUES (?,?,?)`, section, key, ` { "original" : true } `); err != nil {
						t.Fatal(err)
					}
				}
				before := anchorDBRows(t, db, false)
				if err := migrate(t.Context(), db, migrations[:7], 7, time.Now); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before, anchorDBRows(t, db, true)) {
					t.Fatal("physical upgrade changed old persisted values")
				}
			}
			requireAnchorSchema(t, db, "7")
			requireAnchorSentinel(t, db)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			s := openAt(t, path)
			if !accepted.IsAwaitingOwner(imageOf(t, s).Payload.Runtime) {
				t.Fatal("raw reopen reset or adopted physical sentinel")
			}
		})
	}
}

func TestTrustAnchorMigrationReservedHistoryRefusesWithoutMutation(t *testing.T) {
	for n, row := range []replication.Entry{
		{Key: accepted.RootKey, Data: json.RawMessage(accepted.AwaitingOwnerJSON)},
		{Key: accepted.RootKey, Data: json.RawMessage(`{"formatVersion":1,"phase":"managed"}`)},
		{Key: accepted.ChunkPrefix + "orphan", Data: json.RawMessage(` {"original":true} `)},
		{Key: "trustanchors/v99/root", Data: json.RawMessage(`{}`)},
		{Key: accepted.Prefix, Data: json.RawMessage(`{}`)},
	} {
		t.Run([]string{"sentinel", "managed-root", "orphan", "unknown-version", "prefix-only"}[n], func(t *testing.T) {
			db, _ := anchorLegacyDB(t)
			if _, err := db.ExecContext(t.Context(), `INSERT INTO replication_sections(section,key,data) VALUES (?,?,?)`, accepted.Section, row.Key, string(row.Data)); err != nil {
				t.Fatal(err)
			}
			before := anchorDBRows(t, db, false)
			if err := migrate(t.Context(), db, migrations, 7, time.Now); !errors.Is(err, accepted.ErrInvalid) {
				t.Fatal("legacy reserved history adopted", err)
			}
			requireAnchorSchema(t, db, "6")
			if !reflect.DeepEqual(before, anchorDBRows(t, db, false)) {
				t.Fatal("refused migration changed original rows")
			}
		})
	}
}

func TestTrustAnchorMigrationRollbackCancellationAndDowngrade(t *testing.T) {
	for _, stage := range []string{"after-insert", "cancel", "downgrade"} {
		t.Run(stage, func(t *testing.T) {
			db, _ := anchorLegacyDB(t)
			if stage == "downgrade" {
				if err := migrate(t.Context(), db, migrations, 7, time.Now); err != nil {
					t.Fatal(err)
				}
			}
			before := anchorDBRows(t, db, false)
			var err error
			switch stage {
			case "after-insert":
				refused := errors.New("fixture: physical upgrade refused")
				steps := append(append([]migration(nil), migrations[:7]...), migration{version: 8, name: "fixture refusal", up: func(context.Context, *sql.Tx) error { return refused }})
				err = migrate(t.Context(), db, steps, 8, time.Now)
				if !errors.Is(err, refused) {
					t.Fatal("post-sentinel failure not retained", err)
				}
			case "cancel":
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				err = migrate(ctx, db, migrations, 7, time.Now)
				if !errors.Is(err, context.Canceled) {
					t.Fatal("canceled migration proceeded", err)
				}
			case "downgrade":
				// Exercise the actual schema6 migration target, not a new-binary
				// target+1 approximation or a rewritten metadata marker.
				err = migrate(t.Context(), db, migrations[:6], 6, time.Now)
				if !errors.Is(err, ErrSchemaTooNew) {
					t.Fatal("schema6 reader admitted schema7", err)
				}
			}
			want := "6"
			if stage == "downgrade" {
				want = "7"
			}
			requireAnchorSchema(t, db, want)
			if !reflect.DeepEqual(before, anchorDBRows(t, db, false)) {
				t.Fatal("refused transition modified persisted values")
			}
		})
	}
}

func TestTrustAnchorMigrationOldQuarantineIsPreservedButIncompatible(t *testing.T) {
	db, path := anchorLegacyDB(t)
	old := replication.Image{Format: replication.Format, FormatVersion: 1, StoreSchemaVersion: 6, Complete: true, CreatedAt: time.Now().UTC(), Term: replication.Term{ClusterID: "legacy-cluster", Epoch: 1, PrimaryID: "legacy-primary", PromotionID: "legacy-term"}, Revision: 5, KindManifest: []replication.Kind{}, RequiredFeatures: []string{"complete-set-v1", "singleton-settings-v1", "opaque-sections-v1", "provider-registry-v1", "inventory-state-v1", "auth-browser-attempts-v1"}}
	old.Payload = replication.Payload{ClusterSettings: replication.Settings{Owner: replication.SettingsOwner}, Objects: []json.RawMessage{}, Runtime: []replication.Entry{}, Security: []replication.Entry{}, Membership: []replication.Entry{}, Dedup: []replication.Entry{}}
	var err error
	old.PayloadSHA256, err = replication.Digest(old) // Local frozen schema6 fixture; never Seal a received image.
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO replication_quarantine(generation,image) VALUES (?,?)`, old.Generation().ETag(), string(raw)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s := openAt(t, path)
	if _, err := s.ReadQuarantined(t.Context(), old.Generation()); !errors.Is(err, replication.ErrIncompatible) {
		t.Fatal("historical quarantine silently converted", err)
	}
	var stored string
	if err := s.r.QueryRowContext(t.Context(), `SELECT image FROM replication_quarantine WHERE generation=?`, old.Generation().ETag()).Scan(&stored); err != nil || stored != string(raw) {
		t.Fatal("upgrade changed private historical evidence", err)
	}
}

// SeedSchema6AnchorDatabase is a test-only bridge for external composition tests
// to create REAL physical schema6 storage with its original installed receipt.
// It never opens a schema7 Store and edits its version marker backward. The
// supplied image is a local fixture cut, not a received/conversion API.
func SeedSchema6AnchorDatabase(t *testing.T, path string, image replication.Image, authority replication.Authority) {
	t.Helper()
	if image.StoreSchemaVersion != 6 || len(image.RequiredFeatures) != 6 || len(image.Payload.Objects) != 0 || image.ObjectResourceVersion != 0 || !authority.Installed.Same(image.Generation()) {
		t.Fatal("invalid frozen schema6 fixture")
	}
	db, err := sql.Open("sqlite", dsn(path, time.Second, true))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := migrate(t.Context(), db, migrations[:6], 6, time.Now); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, name := range replication.SectionNames() {
		for _, row := range *replication.Section(&image.Payload, name) {
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO replication_sections(section,key,data) VALUES (?,?,?)`, name, row.Key, string(row.Data)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writeAuthority(t.Context(), tx, authority); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
