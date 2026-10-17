package sqlitelog

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/querylog"
)

var update = flag.Bool("update", false, "rewrite testdata golden files")

// TestSchemaGolden pins the on-disk schema to dev-docs/design-record.md §11.2.
func TestSchemaGolden(t *testing.T) {
	w, _ := newWriter(t, t0)
	rows, err := w.w.Query(`SELECT type, name, sql FROM sqlite_master WHERE sql IS NOT NULL ORDER BY type DESC, name`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var b strings.Builder
	for rows.Next() {
		var typ, name, sqlText string
		if err := rows.Scan(&typ, &name, &sqlText); err != nil {
			t.Fatal(err)
		}
		b.WriteString("-- " + typ + " " + name + "\n" + sqlText + ";\n\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "schema.golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden) //nolint:gosec // fixed testdata path
	if err != nil {
		t.Fatalf("%v (run go test -update)", err)
	}
	if b.String() != string(want) {
		t.Errorf("schema drifted from %s (run go test -update after a deliberate change):\n%s", golden, b.String())
	}
	for _, table := range []string{"rollup_1h", "rollup_client_1h", "rollup_qtype_1h", "rollup_upstream_1h", "rollup_1d", "rollup_client_1d", "rollup_domain_1d"} {
		if !strings.Contains(b.String(), "-- table "+table+"\n") {
			t.Errorf("identical-shape table %s missing", table)
		}
	}
	if n := strings.Count(b.String(), "-- index query_"); n != 4 {
		t.Errorf("query has %d indexes, the spec allows exactly four", n)
	}
}

// TestPragmas checks the §11.2 PRAGMAs on the writer and reader connections.
func TestPragmas(t *testing.T) {
	w, _ := newWriter(t, t0)
	want := map[string]string{
		"journal_mode":       "wal",
		"synchronous":        "1", // NORMAL
		"auto_vacuum":        "2", // INCREMENTAL
		"wal_autocheckpoint": "2000",
		"mmap_size":          "268435456",
		"busy_timeout":       "1000",
		"temp_store":         "2", // MEMORY
	}
	for name, db := range map[string]*sql.DB{"writer": w.w, "reader": w.r} {
		for p, v := range want {
			var got string
			if err := db.QueryRow("PRAGMA " + p).Scan(&got); err != nil {
				t.Fatalf("%s PRAGMA %s: %v", name, p, err)
			}
			if got != v {
				t.Errorf("%s PRAGMA %s = %s, want %s", name, p, got, v)
			}
		}
	}
	var qo string
	if err := w.r.QueryRow("PRAGMA query_only").Scan(&qo); err != nil || qo != "1" {
		t.Errorf("reader query_only = %q, %v", qo, err)
	}
}

func TestMetaAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	opts := func(o *Options) { o.Path = path }
	w, _ := newWriter(t, t0, opts)
	mustWrite(t, w, event(t0, "10.0.0.1", "a.example"), event(t0.Add(time.Second), "10.0.0.2", "b.example"))
	if id := w.NextID(); id != 3 {
		t.Errorf("NextID = %d, want 3", id)
	}
	meta, err := readMeta(context.Background(), w.w)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{metaSchemaVersion: strconv.Itoa(SchemaVersion), metaNodeID: "node-a", metaPrivacy: "full", metaFirstTS: strconv.FormatInt(t0.UnixMicro(), 10)} {
		if meta[k] != v {
			t.Errorf("meta %s = %q, want %q", k, meta[k], v)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen under a new privacy mode: the rows survive, the mode is recorded.
	w2, _ := newWriter(t, t0.Add(time.Minute), opts, func(o *Options) { o.Privacy = querylog.PrivacyHideDomains })
	if n := w2.rows.Load(); n != 2 {
		t.Errorf("rows after reopen = %d", n)
	}
	if id := w2.NextID(); id != 3 {
		t.Errorf("NextID after reopen = %d", id)
	}
	meta, _ = readMeta(context.Background(), w2.w)
	if meta[metaPrivacy] != "hide_domains" {
		t.Errorf("privacy_mode_at_write = %q", meta[metaPrivacy])
	}
	if err := w2.Close(); err != nil {
		t.Fatal(err)
	}

	// A newer schema is refused.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE meta SET value=99 WHERE key='schema_version'`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if _, err := Open(context.Background(), Options{Path: path, Node: "node-a", Logger: quietLogger()}); !errors.Is(err, ErrSchemaTooNew) {
		t.Errorf("newer schema: got %v, want ErrSchemaTooNew", err)
	}
}
