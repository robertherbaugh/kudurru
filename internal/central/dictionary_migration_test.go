package central_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/robertherbaugh/kudurru/internal/central"
	"github.com/robertherbaugh/kudurru/internal/central/centraltest"
)

// Exercise the actual populated v3 schema, not an approximation of v4.
func TestDictionaryMigration(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(fmt.Sprint("dangling=", invalid), func(t *testing.T) {
			ctx := context.Background()
			p := pool(t, centraltest.DSN(t))
			err := pgx.BeginFunc(ctx, p, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `CREATE TABLE kudurru_meta (key text PRIMARY KEY, value text NOT NULL)`); err != nil {
					return err
				}
				for _, name := range []string{"migrations/0001_init.sql", "migrations/0002_audit.sql", "migrations/0003_querylog.sql"} {
					//nolint:gosec // fixed repository migration filenames above
					sql, err := os.ReadFile(name)
					if err != nil {
						return err
					}
					if _, err := tx.Exec(ctx, string(sql)); err != nil {
						return err
					}
				}
				_, err := tx.Exec(ctx, `INSERT INTO kudurru_meta VALUES ('schema_version','3'), ('compat_version','1');
     INSERT INTO dict_node(name) VALUES ('retained-node');
     INSERT INTO dict_domain(name) VALUES ('retained.example'), ('orphan.example');
     INSERT INTO dict_client(ip,name) VALUES ('192.0.2.1',NULL), ('192.0.2.1','');
     INSERT INTO rollup_domain_1d(ts,node,domain,total) VALUES (0,1,1,7)`)
				if err != nil {
					return err
				}
				if invalid {
					_, err = tx.Exec(ctx, `INSERT INTO rollup_domain_1d(ts,node,domain,total) VALUES (86400,1,999,3)`)
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := central.Migrate(ctx, p)
			if invalid {
				if err == nil {
					t.Fatal("migration accepted dangling existing rollup")
				}
				var version string
				if err := p.QueryRow(ctx, `SELECT value FROM kudurru_meta WHERE key='schema_version'`).Scan(&version); err != nil || version != "3" {
					t.Fatalf("version after rollback: %s: %v", version, err)
				}
				var refs, indexes, cursors int
				err := p.QueryRow(ctx, `SELECT
     (SELECT count(*) FROM pg_constraint WHERE connamespace=current_schema()::regnamespace AND contype='f' AND confrelid='dict_node'::regclass),
     (SELECT count(*) FROM pg_indexes WHERE schemaname=current_schema() AND indexname='query_node_ref'),
     (SELECT count(*) FROM kudurru_meta WHERE key LIKE 'prune_dict_%')`).Scan(&refs, &indexes, &cursors)
				if err != nil || refs != 0 || indexes != 0 || cursors != 0 {
					t.Fatalf("partial migration: refs=%d indexes=%d cursors=%d err=%v", refs, indexes, cursors, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.SchemaVersion != central.SchemaVersion || result.CompatVersion != central.CompatVersion || len(result.Applied) != 3 || result.Applied[0] != 4 || result.Applied[1] != 5 || result.Applied[2] != 6 {
				t.Fatalf("migration result: %+v", result)
			}
			var node, domain, clients, total int64
			err = p.QueryRow(ctx, `SELECT
    (SELECT id FROM dict_node WHERE name='retained-node'),
    (SELECT id FROM dict_domain WHERE name='retained.example'),
    (SELECT count(*) FROM dict_client WHERE ip='192.0.2.1'),
    (SELECT total FROM rollup_domain_1d WHERE ts=0)`).Scan(&node, &domain, &clients, &total)
			if err != nil || node != 1 || domain != 1 || clients != 2 || total != 7 {
				t.Fatalf("upgrade changed identities/data: %d %d %d %d: %v", node, domain, clients, total, err)
			}
			if _, err := p.Exec(ctx, `INSERT INTO dict_domain(name) VALUES ('after-upgrade.example')`); err != nil {
				t.Fatal(err)
			}
			var next int64
			if err := p.QueryRow(ctx, `SELECT id FROM dict_domain WHERE name='after-upgrade.example'`).Scan(&next); err != nil || next != 3 {
				t.Fatalf("sequence changed: %d: %v", next, err)
			}
			again, err := central.Migrate(ctx, p)
			if err != nil || len(again.Applied) != 0 {
				t.Fatalf("repeat: %+v: %v", again, err)
			}
			if _, err := p.Exec(ctx, `DELETE FROM dict_domain WHERE id=1`); err == nil {
				t.Fatal("retained reference not protected")
			}
		})
	}
}
