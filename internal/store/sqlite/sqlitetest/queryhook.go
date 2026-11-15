// Package sqlitetest provides database helpers for SQLite integration tests.
package sqlitetest

import (
	"context"
	"database/sql"
	"database/sql/driver"
)

// WithQueryHook opens a pool over the SQLite driver with a hook immediately
// before each query. Transactions and rows still use the real driver, allowing
// tests to schedule concurrent writes between scan pages without timing sleeps.
func WithQueryHook(d driver.Driver, dsn string, before func(string)) *sql.DB {
	return sql.OpenDB(&connector{driver: d, dsn: dsn, before: before})
}

// WithExecHook is WithQueryHook for executed statements: before runs
// immediately before each ExecContext, so a test can hold or fail one write of
// a transaction while it observes the store from outside.
func WithExecHook(d driver.Driver, dsn string, before func(string)) *sql.DB {
	return sql.OpenDB(&connector{driver: d, dsn: dsn, beforeExec: before})
}

type connector struct {
	driver     driver.Driver
	dsn        string
	before     func(string)
	beforeExec func(string)
}

func (c *connector) Driver() driver.Driver { return c.driver }

func (c *connector) Connect(ctx context.Context) (driver.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conn, err := c.driver.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &connection{Conn: conn, before: c.before, beforeExec: c.beforeExec}, nil
}

type connection struct {
	driver.Conn
	before     func(string)
	beforeExec func(string)
}

func (c *connection) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

func (c *connection) QueryContext(ctx context.Context, stmt string, args []driver.NamedValue) (driver.Rows, error) {
	if c.before != nil {
		c.before(stmt)
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, stmt, args)
}

func (c *connection) ExecContext(ctx context.Context, stmt string, args []driver.NamedValue) (driver.Result, error) {
	if c.beforeExec != nil {
		c.beforeExec(stmt)
	}
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, stmt, args)
}
