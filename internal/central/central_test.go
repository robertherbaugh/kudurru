package central_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/robertherbaugh/kudurru/internal/central"
	"github.com/robertherbaugh/kudurru/internal/central/centraltest"
)

func TestParseDSNNeverEchoes(t *testing.T) {
	for _, dsn := range []string{"", "postgres://u:s3cret@[bad", "host=x password=s3cret port=notaport"} {
		_, err := central.ParseDSN(dsn)
		if err == nil || strings.Contains(err.Error(), "s3cret") {
			t.Fatalf("%q: %v", dsn, err)
		}
	}
	if _, err := central.Open(central.Options{DSN: "postgres://u:s3cret@[bad"}); err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("open: %v", err)
	}
}

// TestUnreachableNeverBlocks: Open does not dial, Run keeps retrying, and
// Pool says the database is unavailable.
func TestUnreachableNeverBlocks(t *testing.T) {
	start := time.Now()
	db, err := central.Open(central.Options{DSN: "postgres://kudurru@127.0.0.1:1/kudurru?connect_timeout=1", MaxBackoff: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if time.Since(start) > time.Second {
		t.Fatal("Open dialled")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := db.Run(ctx); err != nil {
		t.Fatalf("Run ended with %v", err)
	}
	if _, err := db.Pool(); !errors.Is(err, central.ErrUnavailable) {
		t.Fatalf("pool: %v", err)
	}
	if st := db.State(); st.Ready || st.LastError == "" {
		t.Fatalf("state %+v", st)
	}
}

func pool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// TestMigrateConcurrent: replicas starting together migrate once.
func TestMigrateConcurrent(t *testing.T) {
	dsn := centraltest.DSN(t)
	var wg sync.WaitGroup
	results := make([]central.MigrateResult, 4)
	errs := make([]error, 4)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = central.Migrate(context.Background(), pool(t, dsn))
		}()
	}
	wg.Wait()
	applied := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if results[i].SchemaVersion != central.SchemaVersion {
			t.Fatalf("result %+v", results[i])
		}
		if len(results[i].Applied) > 0 {
			applied++
		}
	}
	if applied != 1 {
		t.Fatalf("%d replicas applied migrations", applied)
	}
}

// TestNewerSchema: a schema migrated by a newer binary is used while it
// stays compatible, and refused when it is not.
func TestNewerSchema(t *testing.T) {
	dsn := centraltest.DSN(t)
	p := pool(t, dsn)
	ctx := context.Background()
	if _, err := central.Migrate(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `UPDATE kudurru_meta SET value = '99' WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	if res, err := central.Migrate(ctx, p); err != nil || res.SchemaVersion != 99 {
		t.Fatalf("newer compatible: %+v %v", res, err)
	}
	if _, err := p.Exec(ctx, `UPDATE kudurru_meta SET value = '50' WHERE key = 'compat_version'`); err != nil {
		t.Fatal(err)
	}
	if _, err := central.Migrate(ctx, p); !errors.Is(err, central.ErrIncompatible) {
		t.Fatalf("incompatible: %v", err)
	}
	db, err := central.Open(central.Options{DSN: dsn, MaxBackoff: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	_ = db.Run(rctx)
	if _, err := db.Pool(); !errors.Is(err, central.ErrIncompatible) {
		t.Fatalf("pool on an incompatible schema: %v", err)
	}
	if st := db.State(); !st.Incompatible || st.CompatVersion != 50 {
		t.Fatalf("state %+v", st)
	}
}

func TestReady(t *testing.T) {
	db := centraltest.New(t)
	st := db.State()
	if !st.Ready || st.ServerVersion < central.MinServerVersion || st.SchemaVersion != central.SchemaVersion {
		t.Fatalf("state %+v", st)
	}
	p, err := db.Pool()
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := p.QueryRow(context.Background(), `SELECT count(*) FROM source`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("source table: %d %v", n, err)
	}
}
