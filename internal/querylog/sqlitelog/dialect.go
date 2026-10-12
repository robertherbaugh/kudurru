package sqlitelog

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
)

// dialect is the SQL flavour a Reader speaks: SQLite for a node's
// querylog.db, PostgreSQL for the central database whose tables mirror it
// (NewCentralReader, D161). The read queries are written once, portably;
// the dialect binds parameters and spells the few constructs that differ.
type dialect int

const (
	dialectSQLite dialect = iota
	dialectPostgres
)

// bind rewrites placeholders: SQLite takes the queries as written;
// PostgreSQL numbers them ($1, $2, ...), "?N" keeping its number. Question
// marks inside string literals are left alone.
func (d dialect) bind(q string) string {
	if d != dialectPostgres || !strings.Contains(q, "?") {
		return q
	}
	var b strings.Builder
	b.Grow(len(q) + 8)
	n, quoted := 0, false
	for i := 0; i < len(q); i++ {
		c := q[i]
		switch {
		case c == '\'':
			quoted = !quoted
			b.WriteByte(c)
		case c == '?' && !quoted:
			j := i + 1
			for j < len(q) && q[j] >= '0' && q[j] <= '9' {
				j++
			}
			if j > i+1 {
				b.WriteString("$" + q[i+1:j])
				i = j - 1
				continue
			}
			n++
			b.WriteString("$" + strconv.Itoa(n))
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// inIDs is "col is one of the ids of the bound parameter" (idsArg).
func (d dialect) inIDs(col string) string {
	if d == dialectPostgres {
		return col + " = ANY(?)"
	}
	return col + " IN (SELECT value FROM json_each(?))"
}

// idsArg is the parameter of inIDs.
func (d dialect) idsArg(ids []int64) any {
	if d == dialectPostgres {
		if ids == nil {
			return []int64{}
		}
		return ids
	}
	return idsJSON(ids)
}

// bytewise orders and compares a text column by its bytes (the BINARY
// collation of SQLite; "C" in PostgreSQL, whose default collation is
// linguistic).
func (d dialect) bytewise(col string) string {
	if d == dialectPostgres {
		return col + ` COLLATE "C"`
	}
	return col
}

// prefixEnd bounds every string that starts with prefix under bytewise
// order: "\xff" never occurs in UTF-8, but PostgreSQL only accepts valid
// UTF-8, where U+10FFFF encodes highest.
func (d dialect) prefixEnd(prefix string) string {
	if d == dialectPostgres {
		return prefix + "\U0010FFFF"
	}
	return prefix + "\xff"
}

// rtx is a read transaction that binds its queries for the dialect.
type rtx struct {
	tx *sql.Tx
	d  dialect
}

func (t *rtx) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return t.tx.QueryContext(ctx, t.d.bind(q), args...)
}

func (t *rtx) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return t.tx.QueryRowContext(ctx, t.d.bind(q), args...)
}
