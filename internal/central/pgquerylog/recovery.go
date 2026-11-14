package pgquerylog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/robertherbaugh/kudurru/internal/central"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
)

// Recovery implements bounded, independently recomputable input receipts. It
// repairs only known source ownership; it cannot adopt an opaque legacy cell.
type Recovery struct {
	DB  *central.DB
	Now func() time.Time
}

func (r Recovery) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func recoverySource(ctx context.Context, tx pgx.Tx, uuid, node string, hour int64) (int32, error) {
	var sid int32
	err := tx.QueryRow(ctx, `SELECT s.id FROM source s WHERE s.uuid=$1 AND s.kind='querylog' AND ($2='' OR s.node=$2
 OR EXISTS(SELECT 1 FROM query q JOIN dict_node n ON n.id=q.node WHERE q.source=s.id AND n.name=$2)
 OR EXISTS(SELECT 1 FROM rollup_1h h JOIN dict_node n ON n.id=h.node WHERE h.source=s.id AND n.name=$2)
 OR EXISTS(SELECT 1 FROM rollup_1d d JOIN dict_node n ON n.id=d.node WHERE d.source=s.id AND n.name=$2))`, uuid, node).Scan(&sid)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, sqlitelog.ErrUnknownNode
	}
	if err != nil {
		return 0, err
	}
	if node != "" {
		var other bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM query q JOIN dict_node n ON n.id=q.node WHERE q.source=$1 AND q.ts>=$2 AND q.ts<$3 AND n.name<>$4)
   OR EXISTS(SELECT 1 FROM rollup_1m m JOIN dict_node n ON n.id=m.node WHERE m.source=$1 AND m.ts>=$5 AND m.ts<$6 AND n.name<>$4)
   OR EXISTS(SELECT 1 FROM rollup_1h h JOIN dict_node n ON n.id=h.node WHERE h.source=$1 AND h.ts=$5 AND n.name<>$4)`, sid, hour*1000000, (hour+3600)*1000000, node, hour, hour+3600).Scan(&other); err != nil {
			return 0, err
		}
		if other {
			return 0, sqlitelog.ErrRollupStale
		} // cannot extend an exact node receipt to another node
	}
	return sid, nil
}

// Evidence captures a bounded retained-input receipt in a read-only snapshot.
func (r Recovery) Evidence(ctx context.Context, source, node string, hour int64) (*sqlitelog.RollupEvidence, error) {
	return r.readEvidence(ctx, source, node, hour, time.Time{})
}

func (r Recovery) readEvidence(ctx context.Context, source, node string, hour int64, expiresAt time.Time) (*sqlitelog.RollupEvidence, error) {
	ctx, cancel := context.WithTimeout(ctx, backfillTimeout)
	defer cancel()
	if len(source) == 0 || len(source) > 128 || len(node) > 253 || hour <= 0 || hour%3600 != 0 || hour > r.now().Unix()-3600 {
		return nil, sqlitelog.ErrRollupBound
	}
	pool, err := r.DB.Pool()
	if err != nil {
		return nil, err
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	sid, err := recoverySource(ctx, tx, source, node, hour)
	if err != nil {
		return nil, err
	}
	return r.evidence(ctx, tx, sid, source, node, hour, expiresAt)
}

func (r Recovery) evidence(ctx context.Context, tx pgx.Tx, sid int32, source, node string, hour int64, expiresAt time.Time) (*sqlitelog.RollupEvidence, error) {
	var legacy int64
	var enabled, partial, complete, minuteComplete bool
	if expiresAt.IsZero() {
		expiresAt = r.now().Add(5 * time.Minute)
	}
	e := &sqlitelog.RollupEvidence{SourceID: source, Node: node, Hour: hour, ExpiresAt: expiresAt}
	if err := tx.QueryRow(ctx, `SELECT s.legacy_before,s.facts_enabled,s.input_partial,f.revision,f.head,f.input_complete,f.minute_complete FROM rollup_source_state s JOIN rollup_input_hour f ON f.source=s.source AND f.ts=$2 WHERE s.source=$1`, sid, hour).Scan(&legacy, &enabled, &partial, &e.Revision, &e.Head, &complete, &minuteComplete); errors.Is(err, pgx.ErrNoRows) {
		return nil, sqlitelog.ErrRollupBound
	} else if err != nil {
		return nil, err
	}
	if hour < legacy {
		e.BlockedReason = sqlitelog.ErrRollupLegacy.Error()
	}
	digest := sha256.New()
	header, err := json.Marshal(struct {
		Version                      int
		Source, Node                 string
		Hour, Revision, Head, Legacy int64
		Facts, Partial               bool
		ExpiresAt                    time.Time
		Complete, MinuteComplete     bool
	}{1, source, node, hour, e.Revision, e.Head, legacy, enabled, partial, expiresAt, complete, minuteComplete})
	if err != nil {
		return nil, err
	}
	if _, err := digest.Write(header); err != nil {
		return nil, err
	}
	bytes := len(header)
	if err := hashEvidenceRows(ctx, tx, digest, &bytes, `SELECT to_jsonb(q)::text FROM query q WHERE source=$1 AND ts>=$2 AND ts<$3 ORDER BY source_id,id,ts LIMIT $4`, sid, hour*1000000, (hour+3600)*1000000); err != nil {
		return nil, err
	}
	for _, t := range sqlitelog.RollupTables {
		from := hour / t.Res * t.Res
		until := hour + 3600
		if t.Res == 86400 {
			until = from + 86400
		}
		order := "ts,node"
		if t.Key != "" {
			order += "," + t.Key
		}
		if _, err := io.WriteString(digest, t.Name+"\x00"); err != nil {
			return nil, err
		}
		if err := hashEvidenceRows(ctx, tx, digest, &bytes, `SELECT to_jsonb(r)::text FROM `+t.Name+` r WHERE source=$1 AND ts>=$2 AND ts<$3 ORDER BY `+order+` LIMIT $4`, sid, from, until); err != nil {
			return nil, err
		}
	}
	e.Digest = hex.EncodeToString(digest.Sum(nil))
	e.Scope = sqlitelog.Scope{Scope: sqlitelog.ScopeCluster, Source: sqlitelog.SourceCentral, Nodes: []sqlitelog.NodeResult{}, Partial: e.BlockedReason != ""}
	if node != "" {
		e.Scope.Scope = sqlitelog.ScopeNode
		e.Nodes = append(e.Nodes, sqlitelog.NodeResult{Name: node, OK: e.BlockedReason == "", Error: e.BlockedReason})
	}
	return e, nil
}

func hashEvidenceRows(ctx context.Context, tx pgx.Tx, digest hash.Hash, bytes *int, q string, sid int32, from, until int64) error {
	rows, err := tx.Query(ctx, q, sid, from, until, backfillRows+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			return err
		}
		count++
		*bytes += len(row)
		if count > backfillRows || *bytes > 4<<20 {
			return sqlitelog.ErrRollupBound
		}
		if _, err := io.WriteString(digest, row+"\x00"); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Repair replaces known source-owned input exactly once after current checks.
func (r Recovery) Repair(ctx context.Context, e sqlitelog.RollupEvidence, check func(context.Context) error) (*sqlitelog.RollupRepairResult, error) {
	ctx, cancel := context.WithTimeout(ctx, backfillTimeout)
	defer cancel()
	if check == nil {
		return nil, sqlitelog.ErrCentralRead
	}
	if err := check(ctx); err != nil {
		return nil, err
	}
	if !validEvidenceScope(e) || len(e.SourceID) == 0 || len(e.SourceID) > 128 || len(e.Node) > 253 || len(e.Digest) != 64 || e.Hour <= 0 || e.Hour%3600 != 0 || e.Hour > r.now().Unix()-3600 || !r.now().Before(e.ExpiresAt) || e.ExpiresAt.After(r.now().Add(5*time.Minute)) {
		return nil, sqlitelog.ErrRollupStale
	}
	result := &sqlitelog.RollupRepairResult{}
	err := r.DB.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout='100ms'; SET LOCAL statement_timeout='5s'`); err != nil {
			return err
		}
		// Take the source lock before checking input or a deduplicated receipt.
		var id int32
		if err := tx.QueryRow(ctx, `SELECT id FROM source WHERE uuid=$1 AND kind='querylog' FOR UPDATE`, e.SourceID).Scan(&id); err != nil {
			return err
		}
		sid, err := recoverySource(ctx, tx, e.SourceID, e.Node, e.Hour)
		if err != nil {
			return err
		}
		var legacy, revision int64
		if err := tx.QueryRow(ctx, `SELECT s.legacy_before,f.revision FROM rollup_source_state s JOIN rollup_input_hour f ON f.source=s.source AND f.ts=$2 WHERE s.source=$1`, sid, e.Hour).Scan(&legacy, &revision); err != nil {
			return err
		}
		if e.Hour < legacy {
			return sqlitelog.ErrRollupLegacy
		}
		if revision != e.Revision {
			return sqlitelog.ErrRollupStale
		}
		var duplicate bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM rollup_repair_receipt WHERE digest=$1 AND source=$2 AND node_scope=$3 AND ts=$4 AND revision=$5)`, e.Digest, sid, e.Node, e.Hour, e.Revision).Scan(&duplicate); err != nil {
			return err
		}
		if duplicate {
			if !r.now().Before(e.ExpiresAt) {
				return sqlitelog.ErrRollupStale
			}
			result.AlreadyApplied = true
			return check(ctx)
		}
		current, err := r.evidence(ctx, tx, sid, e.SourceID, e.Node, e.Hour, e.ExpiresAt)
		if err != nil {
			return err
		}
		if current.Digest != e.Digest || current.Head != e.Head {
			return sqlitelog.ErrRollupStale
		}
		if err := summarizeHour(ctx, tx, sid, e.Hour); err != nil {
			return err
		}
		if err := rebuildDays(ctx, tx, sid, e.Hour, e.Hour+3600, r.now()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO rollup_repair_receipt(digest,source,node_scope,ts,revision) VALUES($1,$2,$3,$4,$5)`, e.Digest, sid, e.Node, e.Hour, e.Revision); err != nil {
			return err
		}
		if !r.now().Before(e.ExpiresAt) {
			return sqlitelog.ErrRollupStale
		}
		if err := check(ctx); err != nil {
			return err
		}
		result.Applied = true
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("source rollup repair: %w", err)
	}
	return result, nil
}

// Verify rechecks the bounded receipt in a fresh read-only snapshot. A success
// describes that snapshot; it does not promise that a later apply will succeed.
func (r Recovery) Verify(ctx context.Context, e sqlitelog.RollupEvidence) error {
	if !validEvidenceScope(e) || len(e.Digest) != 64 || !r.now().Before(e.ExpiresAt) || e.ExpiresAt.After(r.now().Add(5*time.Minute)) {
		return sqlitelog.ErrRollupStale
	}
	current, err := r.readEvidence(ctx, e.SourceID, e.Node, e.Hour, e.ExpiresAt)
	if err != nil {
		return err
	}
	if current.BlockedReason != "" {
		return sqlitelog.ErrRollupLegacy
	}
	if current.Digest != e.Digest || current.Revision != e.Revision || current.Head != e.Head {
		return sqlitelog.ErrRollupStale
	}
	return nil
}

func validEvidenceScope(e sqlitelog.RollupEvidence) bool {
	if e.Source != sqlitelog.SourceCentral {
		return false
	}
	if e.Node == "" {
		return e.Scope.Scope == sqlitelog.ScopeCluster
	}
	return e.Scope.Scope == sqlitelog.ScopeNode && len(e.Nodes) == 1 && e.Nodes[0].Name == e.Node
}
