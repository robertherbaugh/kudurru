// Package centraltest gives tests a central database: a fresh schema on
// the PostgreSQL server named by $KUDURRU_TEST_POSTGRES_DSN, dropped when
// the test ends. Without the variable the tests skip, unless
// $KUDURRU_REQUIRE_POSTGRES is set (CI), which makes the skip a failure.
// `make test-postgres` starts a server in Docker and sets both.
package centraltest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/robertherbaugh/kudurru/internal/central"
)

// EnvDSN names the test server; EnvRequire turns a missing server into a
// failure.
const (
	EnvDSN     = "KUDURRU_TEST_POSTGRES_DSN"
	EnvRequire = "KUDURRU_REQUIRE_POSTGRES"
)

// DSN returns a connection string to a fresh, empty schema of the test
// server (its search_path), dropped at the end of the test.
func DSN(t testing.TB) string {
	t.Helper()
	base := os.Getenv(EnvDSN)
	if base == "" {
		if os.Getenv(EnvRequire) != "" {
			t.Fatalf("%s is set but %s is empty", EnvRequire, EnvDSN)
		}
		t.Skipf("no PostgreSQL: set %s (make test-postgres)", EnvDSN)
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	schema := "t_" + hex.EncodeToString(b)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect to the test server: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if c, err := pgx.Connect(ctx, base); err == nil {
			_, _ = c.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
			_ = c.Close(ctx)
		}
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("%s must be a URL: %v", EnvDSN, err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

// New returns a migrated, ready DB on a fresh schema; its Run stops when
// the test ends.
func New(t testing.TB) *central.DB {
	t.Helper()
	return Open(t, DSN(t))
}

// Open opens dsn, runs it in the background and waits until it is ready.
func Open(t testing.TB, dsn string) *central.DB {
	t.Helper()
	db, err := central.Open(central.Options{DSN: dsn, Node: "test", MaxConns: 8})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = db.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done; db.Close() })
	deadline := time.Now().Add(20 * time.Second)
	for !db.State().Ready {
		if time.Now().After(deadline) {
			t.Fatalf("central database not ready: %s", db.State().LastError)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return db
}
