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
	tx     *sql.Tx
	d      dialect
	scoped bool
	node   int64
}

func (t *rtx) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return t.tx.QueryContext(ctx, t.d.bind(q), args...)
}

func (t *rtx) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return t.tx.QueryRowContext(ctx, t.d.bind(q), args...)
}

// nodeWhere is appended explicitly at each fact/rollup relation, before its
// grouping, count, ordering or limit. Values always remain bound parameters.
func (t *rtx) nodeWhere(alias string, args *[]any) string {
	if !t.scoped {
		return ""
	}
	*args = append(*args, t.node)
	return " AND " + alias + "node=?"
}

// dictionaryWhere requires a retained selected-node reference. Global interner
// timestamps alone never expose another node's identities after raw expiry.
func (t *rtx) dictionaryWhere(table, alias string, args *[]any) string {
	if !t.scoped {
		return ""
	}
	key := strings.TrimPrefix(table, "dict_")
	refs := []struct{ table, column string }{{"query", key}}
	if key == "domain" {
		refs = append(refs, struct{ table, column string }{"query", "cname"})
	}
	for _, r := range RollupTables {
		if r.Key == key || key == "node" {
			refs = append(refs, struct{ table, column string }{r.Name, key})
		}
	}
	parts := make([]string, 0, len(refs))
	for _, r := range refs {
		parts = append(parts, "EXISTS (SELECT 1 FROM "+r.table+" ref WHERE ref."+r.column+"="+alias+"id"+t.nodeWhere("ref.", args)+")")
	}
	return " AND (" + strings.Join(parts, " OR ") + ")"
}

// clientSeen uses only retained selected-node facts and rollups. A dictionary
// timestamp is global; using it here would leak another node's more recent use.
func (t *rtx) clientSeen(alias string, args *[]any) string {
	if !t.scoped {
		return "coalesce(" + alias + "last_seen,0)"
	}
	parts := []string{"SELECT max(ts)/1000000 AS ts FROM query WHERE client=" + alias + "id" + t.nodeWhere("", args)}
	for _, r := range RollupTables {
		if r.Key == "client" {
			parts = append(parts, "SELECT max(ts) AS ts FROM "+r.Name+" WHERE client="+alias+"id"+t.nodeWhere("", args))
		}
	}
	return "coalesce((SELECT max(ts) FROM (" + strings.Join(parts, " UNION ALL ") + ") seen),0)"
}
