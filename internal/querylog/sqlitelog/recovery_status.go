package sqlitelog

import (
	"context"
	"strings"
)

// sourceWhere binds source visibility to actual retained references as well as
// its current registration. A renamed source's old node is not reassigned.
func (t *rtx) sourceWhere(alias string, args *[]any) string {
	if !t.scoped {
		return ""
	}
	*args = append(*args, t.node)
	parts := []string{alias + "node=(SELECT name FROM dict_node WHERE id=?)"}
	for _, table := range append([]string{"query"}, rollupTableNames()...) {
		parts = append(parts, "EXISTS(SELECT 1 FROM "+table+" ref WHERE ref.source="+alias+"id"+t.nodeWhere("ref.", args)+")")
	}
	return " AND (" + strings.Join(parts, " OR ") + ")"
}

func (t *rtx) backfillStatus(ctx context.Context) (*RollupBackfillStatus, error) {
	out := &RollupBackfillStatus{Details: []RollupBackfillDetail{}}
	for _, table := range RollupTables {
		args := []any{}
		q := `SELECT count(*) FROM (SELECT 1 FROM ` + table.Name + ` WHERE source IS NULL` + t.nodeWhere("", &args) + ` LIMIT ?) legacy`
		args = append(args, centralCap+1)
		var n int64
		if err := t.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			return nil, err
		}
		out.LegacyCells += n
		out.Capped = out.Capped || n > centralCap
	}
	args := []any{}
	where := t.sourceWhere("s.", &args)
	args = append(args, centralCap+1)
	if err := t.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM rollup_input_hour f JOIN source s ON s.id=f.source LEFT JOIN rollup_bucket b ON b.source=f.source AND b.resolution=3600 AND b.ts=f.ts
 WHERE (b.source IS NULL OR b.origin='pending' OR b.origin='synthetic' AND b.revision<>f.revision)`+where+` LIMIT ?) pending`, args...).Scan(&out.PendingHours); err != nil {
		return nil, err
	}
	out.Capped = out.Capped || out.PendingHours > centralCap

	args = []any{}
	where = t.sourceWhere("s.", &args)
	args = append(args, centralCap+1)
	if err := t.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT DISTINCT f.source,(f.ts/86400)*86400 FROM rollup_input_hour f JOIN source s ON s.id=f.source LEFT JOIN rollup_bucket d ON d.source=f.source AND d.resolution=86400 AND d.ts=(f.ts/86400)*86400 WHERE (d.source IS NULL OR d.origin='pending' OR d.origin='synthetic' AND d.revision<>(SELECT sum(i.revision) FROM rollup_input_hour i WHERE i.source=f.source AND i.ts>=(f.ts/86400)*86400 AND i.ts<(f.ts/86400+1)*86400))`+where+` LIMIT ?) pending`, args...).Scan(&out.PendingDays); err != nil {
		return nil, err
	}
	out.Capped = out.Capped || out.PendingDays > centralCap
	args = []any{}
	where = t.sourceWhere("s.", &args)
	args = append(args, 26)
	rows, err := t.QueryContext(ctx, `SELECT s.uuid,r.legacy_before FROM source s JOIN rollup_source_state r ON r.source=s.id WHERE r.legacy_before>0`+where+` ORDER BY s.id LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var d RollupBackfillDetail
		if err := rows.Scan(&d.Source, &d.LegacyBefore); err != nil {
			_ = rows.Close()
			return nil, err
		}
		d.State = ErrRollupLegacy.Error()
		out.Details = append(out.Details, d)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	args = []any{}
	where = t.sourceWhere("s.", &args)
	args = append(args, 26)
	rows, err = t.QueryContext(ctx, `SELECT s.uuid,b.ts,b.resolution,b.error_key FROM source s JOIN (SELECT source,ts,resolution,error_key FROM rollup_bucket WHERE error_key<>'' UNION ALL SELECT f.source,f.ts,3600 AS resolution,'pending_source_rollup' AS error_key FROM rollup_input_hour f WHERE NOT EXISTS(SELECT 1 FROM rollup_bucket h WHERE h.source=f.source AND h.ts=f.ts AND h.resolution=3600)) b ON b.source=s.id WHERE true`+where+` ORDER BY s.id,b.ts LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var d RollupBackfillDetail
		if err := rows.Scan(&d.Source, &d.Hour, &d.Resolution, &d.State); err != nil {
			return nil, err
		}
		out.Details = append(out.Details, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out.Details) > 25 {
		out.Truncated = true
		out.Details = out.Details[:25]
	}
	return out, nil
}

func rollupTableNames() []string {
	out := make([]string, 0, len(RollupTables))
	for _, t := range RollupTables {
		out = append(out, t.Name)
	}
	return out
}
