package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"sync"
)

// stmts caches prepared statements of one database handle by query text.
// Parsing SQL is a large share of a point read in SQLite, so every fixed
// query is prepared once; database/sql re-prepares per pooled connection
// on demand. The query texts are compile-time constants or assembled from
// a bounded set of fragments (List), so the cache stays small.
type stmts struct {
	db *sql.DB
	mu sync.Mutex
	m  map[string]*sql.Stmt
}

func newStmts(db *sql.DB) *stmts { return &stmts{db: db, m: map[string]*sql.Stmt{}} }

// prep returns the prepared statement for q.
func (c *stmts) prep(ctx context.Context, q string) (*sql.Stmt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if st, ok := c.m[q]; ok {
		return st, nil
	}
	st, err := c.db.PrepareContext(ctx, q)
	if err != nil {
		return nil, err
	}
	c.m[q] = st
	return st, nil
}

// inTx returns q bound to tx: the cached statement when q was prepared
// before (on the writer this is done by Open, see writerQueries), else a
// statement prepared on the transaction's own connection. It never asks
// the pool for another connection, which would deadlock the single-
// connection writer.
func (c *stmts) inTx(ctx context.Context, tx *sql.Tx, q string) (*sql.Stmt, error) {
	c.mu.Lock()
	st, ok := c.m[q]
	c.mu.Unlock()
	if !ok {
		return tx.PrepareContext(ctx, q)
	}
	return tx.StmtContext(ctx, st), nil
}

// prepareAll prepares qs up front.
func (c *stmts) prepareAll(ctx context.Context, qs []string) error {
	for _, q := range qs {
		if _, err := c.prep(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// txQueryer is what the write path needs from a transaction.
type txQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// cachedTx runs queries of a transaction through prepared statements.
type cachedTx struct {
	tx *sql.Tx
	c  *stmts
}

// QueryRowContext implements txQueryer. If preparing fails, the query runs
// unprepared so the error surfaces from Scan.
func (t cachedTx) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	st, err := t.c.inTx(ctx, t.tx, q)
	if err != nil {
		return t.tx.QueryRowContext(ctx, q, args...)
	}
	return st.QueryRowContext(ctx, args...)
}

// ExecContext implements txQueryer.
func (t cachedTx) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	st, err := t.c.inTx(ctx, t.tx, q)
	if err != nil {
		return nil, err
	}
	return st.ExecContext(ctx, args...)
}

// cachedDB runs reader-pool queries through prepared statements.
type cachedDB struct{ c *stmts }

// QueryRowContext implements queryer.
func (d cachedDB) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	st, err := d.c.prep(ctx, q)
	if err != nil {
		return d.c.db.QueryRowContext(ctx, q, args...)
	}
	return st.QueryRowContext(ctx, args...)
}

// QueryContext implements queryer.
func (d cachedDB) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	st, err := d.c.prep(ctx, q)
	if err != nil {
		return nil, err
	}
	return st.QueryContext(ctx, args...)
}

func (c *stmts) close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var errs []error
	for q, st := range c.m {
		errs = append(errs, st.Close())
		delete(c.m, q)
	}
	return errors.Join(errs...)
}
