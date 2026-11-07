package pgquerylog

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
)

const backfillTimeout = 5 * time.Second
const backfillRows = 10000
const backfillSources = 4
const backfillBuckets = 4

var errLegacyProvenance = errors.New("legacy_provenance_required")
var errInputIncomplete = errors.New("source_input_incomplete")
var errBucketOversize = errors.New("source_bucket_too_large")

// backfillStopped locks only selected sources. Durable keyset rotation gives
// other sources work even when one source has an unbounded or failing backlog.
func backfillStopped(ctx context.Context, tx pgx.Tx, now time.Time) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, backfillTimeout)
	defer cancel()
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout='100ms'; SET LOCAL statement_timeout='5s'`); err != nil {
		return 0, err
	}
	var raw string
	var sweep dictionarySweep
	if err := tx.QueryRow(ctx, `SELECT value FROM kudurru_meta WHERE key='rollup_backfill_after' FOR UPDATE SKIP LOCKED`).Scan(&raw); errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	if err := json.Unmarshal([]byte(raw), &sweep); err != nil {
		return 0, err
	}
	if sweep.After < 0 || sweep.Through < sweep.After {
		return 0, errors.New("invalid source sweep cursor")
	}
	if sweep.After == sweep.Through {
		sweep.After = 0
		if err := tx.QueryRow(ctx, `SELECT coalesce(max(id),0) FROM source WHERE kind='querylog'`).Scan(&sweep.Through); err != nil {
			return 0, err
		}
	}
	sources, err := tx.Query(ctx, `SELECT s.id FROM source s WHERE s.kind='querylog' AND s.id>$1 AND s.id<=$6 AND s.last_seen<=$2
 AND EXISTS(SELECT 1 FROM rollup_input_hour f LEFT JOIN rollup_bucket b ON b.source=f.source AND b.resolution=3600 AND b.ts=f.ts
 LEFT JOIN rollup_bucket d ON d.source=f.source AND d.resolution=86400 AND d.ts=(f.ts/86400)*86400
 WHERE f.source=s.id AND (f.ts<$3 AND (b.source IS NULL OR b.origin='pending' OR b.origin='synthetic' AND b.revision<>f.revision)
 OR f.ts<$5 AND (d.source IS NULL OR d.origin='pending' OR d.origin='synthetic' AND d.revision<>(SELECT sum(i.revision) FROM rollup_input_hour i WHERE i.source=f.source AND i.ts>=(f.ts/86400)*86400 AND i.ts<(f.ts/86400+1)*86400))))
 ORDER BY s.id LIMIT $4 FOR UPDATE OF s SKIP LOCKED`, sweep.After, now.Add(-2*time.Hour), now.Unix()/3600*3600, backfillSources, now.Unix()/86400*86400, sweep.Through)
	if err != nil {
		return 0, err
	}
	var ids []int32
	for sources.Next() {
		var id int32
		if err := sources.Scan(&id); err != nil {
			sources.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	sources.Close()
	if err := sources.Err(); err != nil {
		return 0, err
	}
	var completed int64
	for _, sid := range ids {
		sweep.After = int64(sid)
		rows, err := tx.Query(ctx, `SELECT f.ts FROM rollup_input_hour f LEFT JOIN rollup_bucket b ON b.source=f.source AND b.resolution=3600 AND b.ts=f.ts
   WHERE f.source=$1 AND f.ts<$2 AND f.ts>(SELECT hour_after FROM rollup_source_state WHERE source=$1) AND (b.source IS NULL OR b.origin<>'official' AND (b.revision<>f.revision OR b.origin='pending')) ORDER BY f.ts LIMIT $3`, sid, now.Unix()/3600*3600, backfillBuckets)
		if err != nil {
			return completed, err
		}
		var hours []int64
		for rows.Next() {
			var h int64
			if err := rows.Scan(&h); err != nil {
				rows.Close()
				return completed, err
			}
			hours = append(hours, h)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return completed, err
		}

		if len(hours) == 0 {
			if _, err := tx.Exec(ctx, `UPDATE rollup_source_state SET hour_after=0 WHERE source=$1`, sid); err != nil {
				return completed, err
			}
		}
		for _, h := range hours {
			if _, err := tx.Exec(ctx, `SAVEPOINT summarize_bucket`); err != nil {
				return completed, err
			}
			err := summarizeHour(ctx, tx, sid, h)
			if err != nil {
				if _, rollbackErr := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT summarize_bucket`); rollbackErr != nil {
					return completed, rollbackErr
				}
				key := "source_summary_failed"
				switch {
				case errors.Is(err, errLegacyProvenance):
					key = errLegacyProvenance.Error()
				case errors.Is(err, errInputIncomplete):
					key = errInputIncomplete.Error()
				case errors.Is(err, errBucketOversize):
					key = errBucketOversize.Error()
				}
				if _, err := tx.Exec(ctx, `INSERT INTO rollup_bucket(source,resolution,ts,origin,error_key) VALUES($1,3600,$2,'pending',$3)
     ON CONFLICT(source,resolution,ts) DO UPDATE SET error_key=excluded.error_key WHERE rollup_bucket.origin<>'official'`, sid, h, key); err != nil {
					return completed, err
				}
			} else {
				completed++
			}
			if _, err := tx.Exec(ctx, `RELEASE SAVEPOINT summarize_bucket`); err != nil {
				return completed, err
			}

			if _, err := tx.Exec(ctx, `UPDATE rollup_source_state SET hour_after=$2 WHERE source=$1`, sid, h); err != nil {
				return completed, err
			}
			if err := rebuildDays(ctx, tx, sid, h, h+3600, now); err != nil {
				return completed, err
			}
		}
		days, err := tx.Query(ctx, `SELECT DISTINCT (f.ts/86400)*86400 AS day FROM rollup_input_hour f LEFT JOIN rollup_bucket d ON d.source=f.source AND d.resolution=86400 AND d.ts=(f.ts/86400)*86400
  WHERE f.source=$1 AND f.ts<$2 AND (f.ts/86400)*86400>(SELECT day_after FROM rollup_source_state WHERE source=$1) AND (d.source IS NULL OR d.origin='pending' OR d.origin='synthetic' AND d.revision<>(SELECT sum(i.revision) FROM rollup_input_hour i WHERE i.source=f.source AND i.ts>=(f.ts/86400)*86400 AND i.ts<(f.ts/86400+1)*86400)) ORDER BY day LIMIT $3`, sid, now.Unix()/86400*86400, backfillBuckets)
		if err != nil {
			return completed, err
		}
		var pendingDays []int64
		for days.Next() {
			var day int64
			if err := days.Scan(&day); err != nil {
				days.Close()
				return completed, err
			}
			pendingDays = append(pendingDays, day)
		}
		days.Close()
		if err := days.Err(); err != nil {
			return completed, err
		}

		if len(pendingDays) == 0 {
			if _, err := tx.Exec(ctx, `UPDATE rollup_source_state SET day_after=0 WHERE source=$1`, sid); err != nil {
				return completed, err
			}
		}
		for _, day := range pendingDays {

			if _, err := tx.Exec(ctx, `UPDATE rollup_source_state SET day_after=$2 WHERE source=$1`, sid, day); err != nil {
				return completed, err
			}
			if err := rebuildDays(ctx, tx, sid, day, day+86400, now); err != nil {
				return completed, err
			}
		}
		if err := advanceSyntheticCoverage(ctx, tx, sid, now); err != nil {
			return completed, err
		}
	}
	if len(ids) == 0 {
		sweep.After = sweep.Through
	}
	next, err := json.Marshal(sweep)
	if err != nil {
		return completed, err
	}
	_, err = tx.Exec(ctx, `UPDATE kudurru_meta SET value=$1 WHERE key='rollup_backfill_after'`, string(next))
	return completed, err
}

func loadFacts(ctx context.Context, tx pgx.Tx, sid int32, h, after int64) ([]sqlitelog.FactMetrics, error) {
	rows, err := tx.Query(ctx, `SELECT node,client,domain,coalesce(upstream,0),coalesce(zone,0),coalesce(list,0),qtype,status,proto,rcode,latency_us
 FROM query WHERE source=$1 AND ts>=$2 AND ts<$3 AND source_id>$4 ORDER BY source_id LIMIT $5`, sid, h*1000000, (h+3600)*1000000, after, backfillRows+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var facts []sqlitelog.FactMetrics
	for rows.Next() {
		var f sqlitelog.FactMetrics
		var proto int
		if err := rows.Scan(&f.Node, &f.Client, &f.Domain, &f.Upstream, &f.Zone, &f.List, &f.QType, &f.Status, &proto, &f.Rcode, &f.LatencyUS); err != nil {
			return nil, err
		}
		if proto == 6 {
			proto = int(pipeline.ProtoDoH3)
		}
		f.Proto = pipeline.Proto(proto) //nolint:gosec // constrained source wire types
		facts = append(facts, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(facts) > backfillRows {
		return nil, errBucketOversize
	}
	return facts, nil
}

func summarizeHour(ctx context.Context, tx pgx.Tx, sid int32, h int64) error {
	var legacy int64
	var enabled, partial bool
	if err := tx.QueryRow(ctx, `SELECT legacy_before,facts_enabled,input_partial FROM rollup_source_state WHERE source=$1`, sid).Scan(&legacy, &enabled, &partial); err != nil {
		return err
	}
	if h < legacy {
		return errLegacyProvenance
	}
	var official bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM rollup_bucket WHERE source=$1 AND resolution=3600 AND ts=$2 AND origin='official') OR
 EXISTS(SELECT 1 FROM ship_cursor WHERE source=$1 AND stream=$3 AND last_id>$2) AND NOT EXISTS(SELECT 1 FROM rollup_bucket WHERE source=$1 AND resolution=3600 AND ts=$2)`, sid, h, streamHour).Scan(&official); err != nil {
		return err
	}
	if official {
		_, err := tx.Exec(ctx, `INSERT INTO rollup_bucket(source,resolution,ts,revision,head,origin) SELECT source,3600,ts,revision,head,'official' FROM rollup_input_hour WHERE source=$1 AND ts=$2 ON CONFLICT(source,resolution,ts) DO NOTHING`, sid, h)
		return err
	}

	var complete, minuteComplete bool
	if err := tx.QueryRow(ctx, `SELECT input_complete,minute_complete FROM rollup_input_hour WHERE source=$1 AND ts=$2`, sid, h).Scan(&complete, &minuteComplete); err != nil {
		return err
	}
	var after, priorTotal int64
	delta := enabled && !complete
	if delta {
		var safe bool
		if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT retained AND revision>0 AND error_key<>'facts_unavailable' FROM rollup_bucket WHERE source=$1 AND resolution=3600 AND ts=$2),false),coalesce((SELECT head FROM rollup_bucket WHERE source=$1 AND resolution=3600 AND ts=$2),0),coalesce((SELECT sum(total) FROM rollup_1h WHERE source=$1 AND ts=$2),0)`, sid, h).Scan(&safe, &after, &priorTotal); err != nil {
			return err
		}
		if !safe {
			return errInputIncomplete
		}
	}
	facts, err := loadFacts(ctx, tx, sid, h, after)
	if err != nil {
		return err
	}
	var minuteTotal int64
	if err := tx.QueryRow(ctx, `SELECT coalesce(sum(total),0) FROM rollup_1m WHERE source=$1 AND ts>=$2 AND ts<$3`, sid, h, h+3600).Scan(&minuteTotal); err != nil {
		return err
	}
	if enabled && (partial || minuteTotal > priorTotal+int64(len(facts))) || !enabled && !minuteComplete {
		return errInputIncomplete
	}
	if enabled && !delta && len(facts) == 0 && minuteTotal == 0 {
		return errInputIncomplete
	}
	if !delta {
		for _, t := range sqlitelog.RollupTables {
			if t.Res == 3600 && (enabled || t.Key != "domain" && t.Key != "list" && t.Key != "zone") {
				if _, err := tx.Exec(ctx, `DELETE FROM `+t.Name+` WHERE source=$1 AND ts=$2`, sid, h); err != nil {
					return err
				}
			}
		}
	}
	if enabled {
		grouped := map[string][]sqlitelog.MetricRollup{}
		for _, r := range sqlitelog.SummarizeFacts(facts) {
			grouped[r.Table.Name] = append(grouped[r.Table.Name], r)
		}
		for _, rs := range grouped {
			t := rs[0].Table
			cols := []string{"ts", "node", "source"}
			if t.Key != "" {
				cols = append(cols, t.Key)
			}
			cols = append(cols, t.Cols...)
			data := make([][]any, 0, len(rs))
			for _, r := range rs {
				row := []any{h, r.Node, sid}
				if t.Key != "" {
					row = append(row, r.Key)
				}
				for _, v := range r.Values {
					row = append(row, v)
				}
				data = append(data, row)
			}
			if err := writeRecoveredMetrics(ctx, tx, t, cols, data, delta); err != nil {
				return err
			}
		}
	} else {
		// Without facts only the four already-shipped minute dimensions exist.
		for _, t := range sqlitelog.RollupTables {
			if t.Res != 3600 || t.Key == "domain" || t.Key == "list" || t.Key == "zone" {
				continue
			}
			if err := aggregateOwned(ctx, tx, t, strings.TrimSuffix(t.Name, "1h")+"1m", sid, h, h+3600); err != nil {
				return err
			}
		}
	}
	code := ""
	if !enabled {
		code = "facts_unavailable"
	}
	_, err = tx.Exec(ctx, `INSERT INTO rollup_bucket(source,resolution,ts,revision,head,origin,error_key)
 SELECT source,3600,ts,revision,head,'synthetic',$3 FROM rollup_input_hour WHERE source=$1 AND ts=$2
 ON CONFLICT(source,resolution,ts) DO UPDATE SET revision=excluded.revision,head=excluded.head,origin='synthetic',error_key=excluded.error_key,retained=true`, sid, h, code)
	return err
}

func aggregateOwned(ctx context.Context, tx pgx.Tx, t sqlitelog.RollupTable, input string, sid int32, from, until int64) error {
	cols := "ts,node,source"
	group := "node"
	key := ""
	if t.Key != "" {
		key = "," + t.Key
		cols += key
		group += key
	}
	sums := make([]string, len(t.Cols))
	for i, c := range t.Cols {
		sums[i] = "sum(" + c + ")"
	}
	_, err := tx.Exec(ctx, `INSERT INTO `+t.Name+` (`+cols+`,`+strings.Join(t.Cols, ",")+`) SELECT $2,node,$1`+key+`,`+strings.Join(sums, ",")+` FROM `+input+` WHERE source=$1 AND ts>=$2 AND ts<$3 GROUP BY `+group, sid, from, until)
	return err
}

func rebuildDays(ctx context.Context, tx pgx.Tx, sid int32, from, until int64, now time.Time) error {
	rows, err := tx.Query(ctx, `SELECT DISTINCT (ts/86400)*86400 AS day FROM rollup_input_hour WHERE source=$1 AND ts>=$2 AND ts<$3 ORDER BY day LIMIT 9`, sid, from/86400*86400, ceilBucket(until, 86400))
	if err != nil {
		return err
	}
	var days []int64
	for rows.Next() {
		var d int64
		if err := rows.Scan(&d); err != nil {
			rows.Close()
			return err
		}
		days = append(days, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, d := range days {
		if d+86400 > now.Unix() {
			continue
		}
		var blocked, official bool
		var revision int64
		if err := tx.QueryRow(ctx, `SELECT
   EXISTS(SELECT 1 FROM rollup_source_state WHERE source=$1 AND legacy_before>$2) OR EXISTS(
    SELECT 1 FROM rollup_input_hour f LEFT JOIN rollup_bucket b ON b.source=f.source AND b.ts=f.ts AND b.resolution=3600
    WHERE f.source=$1 AND f.ts>=$2 AND f.ts<$3 AND (b.source IS NULL OR b.origin='pending' OR b.origin<>'official' AND b.revision<>f.revision)),
   EXISTS(SELECT 1 FROM rollup_bucket WHERE source=$1 AND resolution=86400 AND ts=$2 AND origin='official'),
   coalesce((SELECT sum(revision) FROM rollup_input_hour WHERE source=$1 AND ts>=$2 AND ts<$3),0)`, sid, d, d+86400).Scan(&blocked, &official, &revision); err != nil {
			return err
		}
		if blocked || official {
			continue
		}
		for _, t := range sqlitelog.RollupTables {
			if t.Res != 86400 {
				continue
			}
			if _, err := tx.Exec(ctx, `DELETE FROM `+t.Name+` WHERE source=$1 AND ts=$2`, sid, d); err != nil {
				return err
			}
			if err := aggregateOwned(ctx, tx, t, strings.TrimSuffix(t.Name, "1d")+"1h", sid, d, d+86400); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO rollup_bucket(source,resolution,ts,revision,origin) VALUES($1,86400,$2,$3,'synthetic')
   ON CONFLICT(source,resolution,ts) DO UPDATE SET revision=excluded.revision,origin='synthetic',error_key=''`, sid, d, revision); err != nil {
			return err
		}
	}
	return nil
}

func advanceSyntheticCoverage(ctx context.Context, tx pgx.Tx, sid int32, now time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE rollup_source_state s SET hour_done=CASE WHEN NOT EXISTS(
 SELECT 1 FROM rollup_input_hour f LEFT JOIN rollup_bucket b ON b.source=f.source AND b.resolution=3600 AND b.ts=f.ts
 WHERE f.source=s.source AND f.ts<$2 AND (b.source IS NULL OR b.origin='pending' OR b.origin<>'official' AND b.revision<>f.revision)) THEN $2 ELSE hour_done END,
 day_done=CASE WHEN NOT EXISTS(SELECT 1 FROM rollup_input_hour f LEFT JOIN rollup_bucket b ON b.source=f.source AND b.resolution=86400 AND b.ts=(f.ts/86400)*86400
 WHERE f.source=s.source AND f.ts<$3 AND (b.source IS NULL OR b.origin<>'official' AND b.revision<>(SELECT sum(i.revision) FROM rollup_input_hour i WHERE i.source=f.source AND i.ts>=(f.ts/86400)*86400 AND i.ts<(f.ts/86400+1)*86400))) THEN $3 ELSE day_done END
 WHERE source=$1 AND legacy_before=0`, sid, now.Unix()/3600*3600, now.Unix()/86400*86400)
	return err
}

// writeRecoveredMetrics adds only newly retained source IDs above the previous
// captured head when raw retention removed an already summarized prefix.
func writeRecoveredMetrics(ctx context.Context, tx pgx.Tx, t sqlitelog.RollupTable, cols []string, data [][]any, delta bool) error {
	table := t.Name
	if delta {
		table = "recover_" + t.Name
		if _, err := tx.Exec(ctx, `CREATE TEMP TABLE `+table+` (LIKE `+t.Name+`) ON COMMIT DROP`); err != nil {
			return err
		}
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{table}, cols, pgx.CopyFromRows(data)); err != nil {
		return err
	}
	if !delta {
		return nil
	}
	pk := "ts,node,source"
	if t.Key != "" {
		pk += "," + t.Key
	}
	updates := make([]string, len(t.Cols))
	for i, c := range t.Cols {
		updates[i] = c + "=" + t.Name + "." + c + "+excluded." + c
	}
	names := strings.Join(cols, ",")
	if _, err := tx.Exec(ctx, `INSERT INTO `+t.Name+` (`+names+`) SELECT `+names+` FROM `+table+` ON CONFLICT (`+pk+`) DO UPDATE SET `+strings.Join(updates, ",")); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `DROP TABLE `+table)
	return err
}
