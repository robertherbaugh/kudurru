package pgquerylog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
)

// Limit selected candidates, not just deleted rows: retained low IDs must not
// make an orphan sweep unbounded. The deadline also bounds scanning locked
// candidates. A pass holds locks for at most two seconds.
const dictionaryPruneBatch = 256
const dictionaryPruneTimeout = 2 * time.Second

type dictionaryReference struct{ table, column string }
type dictionarySpec struct {
	table      string
	references []dictionaryReference
}

// dictionarySpecs includes every dictionary-valued fact column and every
// rollup node/key, across all sources. Tests compare it with the database FKs.
func dictionarySpecs() []dictionarySpec {
	var out []dictionarySpec
	for _, name := range []string{"node", "client", "domain", "upstream", "zone", "list", "policy"} {
		spec := dictionarySpec{table: "dict_" + name, references: []dictionaryReference{{"query", name}}}
		if name == "domain" || name == "node" {
			spec.references = append(spec.references, dictionaryReference{"query_domain_hour", name})
		}
		if name == "domain" {
			spec.references = append(spec.references, dictionaryReference{"query", "cname"})
		}
		for _, table := range sqlitelog.RollupTables {
			if name == "node" {
				spec.references = append(spec.references, dictionaryReference{table.Name, "node"})
			}
			if table.Key == name {
				spec.references = append(spec.references, dictionaryReference{table.Name, table.Key})
			}
		}
		out = append(out, spec)
	}
	return out
}

type dictionarySweep struct {
	After   int64 `json:"after"`
	Through int64 `json:"through"`
}

// pruneDictionaries runs inside the Janitor's READ COMMITTED transaction.
// Row locks exclude concurrent interning/reference insertion. Each DELETE
// uses a fresh snapshot after candidate locks, and FKs protect even legacy
// writers that do not explicitly lock cache hits. No last_seen heuristic is
// involved: a retained daily rollup can protect a dictionary indefinitely.
func pruneDictionaries(ctx context.Context, tx pgx.Tx, _ time.Time) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, dictionaryPruneTimeout)
	defer cancel()
	// Janitor normally lifts statement_timeout. This bounded job restores its
	// own time/lock limits, scoped to this transaction.
	if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = '2s'; SET LOCAL lock_timeout = '100ms'`); err != nil {
		return 0, err
	}
	var removed int64
	for _, spec := range dictionarySpecs() {
		n, err := pruneDictionary(ctx, tx, spec)
		if err != nil {
			return 0, fmt.Errorf("pgquerylog: prune %s: %w", spec.table, err)
		}
		removed += n
	}
	return removed, nil
}

func pruneDictionary(ctx context.Context, tx pgx.Tx, spec dictionarySpec) (int64, error) {
	key := "prune_" + spec.table
	var state string
	err := tx.QueryRow(ctx, `SELECT value FROM kudurru_meta WHERE key=$1 FOR UPDATE SKIP LOCKED`, key).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	} // another sweep owns it
	if err != nil {
		return 0, err
	}
	var sweep dictionarySweep
	if err := json.Unmarshal([]byte(state), &sweep); err != nil {
		return 0, err
	}
	if sweep.After < 0 || sweep.Through < sweep.After {
		return 0, errors.New("invalid dictionary sweep cursor")
	}
	table := pgx.Identifier{spec.table}.Sanitize()
	if sweep.After == sweep.Through {
		sweep.After = 0
		if err := tx.QueryRow(ctx, `SELECT coalesce(max(id),0) FROM `+table).Scan(&sweep.Through); err != nil {
			return 0, err
		}
	}
	// The fixed upper ID guarantees wrap even while new IDs are being added.
	rows, err := tx.Query(ctx, `SELECT id FROM `+table+` WHERE id > $1 AND id <= $2 ORDER BY id LIMIT $3 FOR UPDATE SKIP LOCKED`, sweep.After, sweep.Through, dictionaryPruneBatch)
	if err != nil {
		return 0, err
	}
	var candidates []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		candidates = append(candidates, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var removed int64
	if len(candidates) == 0 {
		sweep.After = sweep.Through
	} else {
		sweep.After = candidates[len(candidates)-1]
		var predicates []string
		for _, ref := range spec.references {
			predicates = append(predicates, `NOT EXISTS (SELECT 1 FROM `+pgx.Identifier{ref.table}.Sanitize()+` r WHERE r.`+pgx.Identifier{ref.column}.Sanitize()+` = d.id)`)
		}
		// A separate statement is essential: a writer may have committed just
		// before we acquired these locks, after candidate selection's snapshot.
		result, err := tx.Exec(ctx, `DELETE FROM `+table+` d WHERE d.id = ANY($1::bigint[]) AND `+strings.Join(predicates, " AND "), candidates)
		if err != nil {
			return 0, err
		}
		removed = result.RowsAffected()
	}
	next, err := json.Marshal(sweep)
	if err != nil {
		return 0, err
	}
	_, err = tx.Exec(ctx, `UPDATE kudurru_meta SET value=$2 WHERE key=$1`, key, string(next))
	if err != nil {
		return 0, err
	}
	return removed, nil
}
