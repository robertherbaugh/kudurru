package sqlitelog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// PolicyStats is the traffic one policy decided in a window: the rows whose
// query.policy names it (GET /policies/{namespace}/{name}/state). It reads
// the fact table, so it is bounded by the raw retention and empty under
// privacy modes that store no rows, and it only sees rows written since
// schema version 2.
type PolicyStats struct {
	Scope
	Window
	Policy     string         `json:"policy"`
	Queries    int64          `json:"queries"`
	Blocked    int64          `json:"blocked"`
	TopBlocked []PolicyDomain `json:"top_blocked"`
	TopClients []PolicyClient `json:"top_clients"`
}

// PolicyDomain is one blocked domain of a policy.
type PolicyDomain struct {
	Domain string `json:"domain"`
	Count  int64  `json:"count"`
}

// PolicyClient is one client of a policy: Key is its address, or its name
// when the privacy mode dropped the address.
type PolicyClient struct {
	Key   string `json:"key"`
	Name  string `json:"name,omitempty"`
	Count int64  `json:"count"`
}

// blockedIn is the SQL list of the blocked statuses.
var blockedIn = func() string {
	var b strings.Builder
	for i, s := range statusesOf(pipeline.ClassBlocked) {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprint(&b, int64(s))
	}
	return "(" + b.String() + ")"
}()

// PolicyStats counts the queries policy ("namespace/name") decided in w
// (default: the last 24 hours) and ranks its n most blocked domains and
// busiest clients (default 10, at most MaxTopN).
func (r *Reader) PolicyStats(ctx context.Context, policy string, w Window, n int) (*PolicyStats, error) {
	start := time.Now()
	w = r.window(w)
	if n <= 0 {
		n = 10
	}
	n = min(n, MaxTopN)
	out := &PolicyStats{Window: w, Policy: policy, TopBlocked: []PolicyDomain{}, TopClients: []PolicyClient{}}
	scope, err := r.readScoped(ctx, false, start, func(tx *rtx, _ *snapshot) error {
		var id int64
		args := []any{policy}
		err := tx.QueryRowContext(ctx, `SELECT id FROM dict_policy WHERE name=?`+tx.dictionaryWhere("dict_policy", "dict_policy.", &args), args...).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("sqlitelog: policy id: %w", err)
		}
		from, until := w.From.UnixMicro(), w.Until.UnixMicro()
		args = []any{from, until, id}
		if err := tx.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(CASE WHEN status IN `+blockedIn+` THEN 1 ELSE 0 END),0) FROM query WHERE ts>=? AND ts<? AND policy=?`+tx.nodeWhere("", &args), args...).Scan(&out.Queries, &out.Blocked); err != nil {
			return fmt.Errorf("sqlitelog: policy counts: %w", err)
		}
		args = []any{from, until, id}
		blockedWhere := tx.nodeWhere("q.", &args)
		args = append(args, n)
		rows, err := tx.QueryContext(ctx, `SELECT d.name,count(*) AS c FROM query q JOIN dict_domain d ON d.id=q.domain WHERE q.ts>=? AND q.ts<? AND q.policy=? AND q.status IN `+blockedIn+blockedWhere+` GROUP BY d.id,d.name ORDER BY c DESC,d.name LIMIT ?`, args...)
		if err != nil {
			return fmt.Errorf("sqlitelog: policy top blocked: %w", err)
		}
		for rows.Next() {
			var d PolicyDomain
			if err := rows.Scan(&d.Domain, &d.Count); err != nil {
				_ = rows.Close()
				return err
			}
			out.TopBlocked = append(out.TopBlocked, d)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		args = []any{from, until, id}
		clientWhere := tx.nodeWhere("q.", &args)
		args = append(args, n)
		rows, err = tx.QueryContext(ctx, `SELECT c.ip, coalesce(c.name,''), count(*) AS n FROM query q JOIN dict_client c ON c.id=q.client
			WHERE q.ts>=? AND q.ts<? AND q.policy=?`+clientWhere+`
			GROUP BY c.id, c.ip, c.name ORDER BY n DESC, c.ip, c.name LIMIT ?`, args...)
		if err != nil {
			return fmt.Errorf("sqlitelog: policy top clients: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var c PolicyClient
			if err := rows.Scan(&c.Key, &c.Name, &c.Count); err != nil {
				return err
			}
			if c.Key == "" {
				c.Key, c.Name = c.Name, ""
			}
			out.TopClients = append(out.TopClients, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	out.Scope = scope
	return out, nil
}

// ClientsSeenByGroup counts the distinct clients (by address, else name)
// seen since since, per client group ("namespace/name"). A client counts
// for the first group it was identified in (query log client_group), as
// recorded in its client dictionary entry; last_seen lags by up to an hour
// on disk, so the count is approximate at the window's start.
func (r *Reader) ClientsSeenByGroup(ctx context.Context, since time.Time) (map[string]int, error) {
	out := map[string]int{}
	err := r.read(ctx, false, func(tx *rtx, _ *snapshot) error {
		args := []any{}
		seen := tx.clientSeen("dict_client.", &args)
		args = append(args, since.Unix())
		rows, err := tx.QueryContext(ctx, `SELECT grp, count(DISTINCT CASE WHEN ip<>'' THEN ip ELSE name END) FROM dict_client
 WHERE grp IS NOT NULL AND grp<>'' AND `+seen+` >= ?`+tx.dictionaryWhere("dict_client", "dict_client.", &args)+` GROUP BY grp`, args...)
		if err != nil {
			return fmt.Errorf("sqlitelog: clients by group: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var g string
			var n int
			if err := rows.Scan(&g, &n); err != nil {
				return err
			}
			out[g] = n
		}
		return rows.Err()
	})
	return out, err
}
