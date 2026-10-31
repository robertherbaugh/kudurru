package pgquerylog

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
)

// dictCacheMax bounds each cache; a full cache starts over.
const dictCacheMax = 65536

// clientTouch is how often a client's last_seen is refreshed (seconds).
const clientTouch = 60

// dicts maps dictionary values onto the database's ids, caching them.
// IDs are never reassigned, but retention can delete an unreferenced entry.
// Every cache hit is revalidated and locked in the shipping transaction.
type dicts struct {
	mu      sync.Mutex
	byTable map[string]map[string]int64
	touched map[string]int64 // client key -> last_seen written
}

func newDicts() *dicts {
	return &dicts{byTable: map[string]map[string]int64{}, touched: map[string]int64{}}
}

// clientKey identifies a client entry: (ip, name), name NULL apart from "".
func clientKey(ip string, name *string) string {
	if name == nil {
		return ip + "\x00"
	}
	return ip + "\x01" + *name
}

// ids are the resolved ids of one batch.
type ids struct {
	node, client, domain, upstream, zone, list, policy map[string]int64
	// byKey holds the ids of a rollup table's key dictionary.
	byKey map[string]int64
}

// names resolves names and holds each dictionary row through the fact/rollup
// commit. A cache hit is only a hint: pruning or a rolled-back insert may have
// removed the ID since it was cached.
func (d *dicts) names(ctx context.Context, tx pgx.Tx, table, col string, values map[string]bool, etld map[string]*string, now int64) (map[string]int64, error) {
	out := make(map[string]int64, len(values))
	d.mu.Lock()
	cache := d.byTable[table]
	if cache == nil || len(cache) > dictCacheMax {
		cache = map[string]int64{}
		d.byTable[table] = cache
	}
	var cached []int64
	for v := range values {
		if id, ok := cache[v]; ok {
			cached = append(cached, id)
			out[v] = id
		}
	}
	d.mu.Unlock()
	found := map[string]int64{}
	if len(cached) > 0 {
		//nolint:gosec // table and col are package constants
		rows, err := tx.Query(ctx, `SELECT id, `+col+` FROM `+table+` WHERE id = ANY($1::bigint[]) ORDER BY id FOR KEY SHARE`, cached)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var v string
			if err := rows.Scan(&id, &v); err != nil {
				rows.Close()
				return nil, err
			}
			if out[v] == id {
				found[v] = id
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	var missing []string
	for v := range values {
		if _, ok := found[v]; !ok {
			missing = append(missing, v)
		}
	}
	sort.Strings(missing)
	if len(missing) == 0 {
		return found, nil
	}
	var err error
	if table == "dict_domain" {
		e := make([]*string, len(missing))
		for i, m := range missing {
			e[i] = etld[m]
		}
		_, err = tx.Exec(ctx, `INSERT INTO dict_domain (name, etld1, first_seen, last_seen) SELECT n, e, $3, $3 FROM unnest($1::text[], $2::text[]) AS t(n, e)
			ON CONFLICT (name) DO NOTHING`, missing, e, now)
	} else {
		//nolint:gosec // table and col are package constants
		_, err = tx.Exec(ctx, `INSERT INTO `+table+` (`+col+`) SELECT unnest($1::text[]) ON CONFLICT (`+col+`) DO NOTHING`, missing)
	}
	if err != nil {
		return nil, fmt.Errorf("pgquerylog: %s: %w", table, err)
	}
	// This is a new READ COMMITTED snapshot after INSERT's conflict check.
	// Lock the rows through commit, including those inserted by another shipper.
	//nolint:gosec // table and col are package constants
	rows, err := tx.Query(ctx, `SELECT id, `+col+` FROM `+table+` WHERE `+col+` = ANY($1) ORDER BY id FOR KEY SHARE`, missing)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var v string
		if err := rows.Scan(&id, &v); err != nil {
			rows.Close()
			return nil, err
		}
		found[v] = id
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(found) != len(values) {
		// A prune can win between the conflict check and the SELECT. Refuse the
		// entire batch: never use a missing map entry as a zero dictionary ID.
		return nil, fmt.Errorf("pgquerylog: %s: incomplete dictionary resolution", table)
	}
	d.mu.Lock()
	for v, id := range found {
		cache[v] = id
	}
	d.mu.Unlock()
	return found, nil
}

// client is one client entry of a batch.
type client struct {
	ip         string
	name, grp  *string
	lastSeenAt int64
}

// clients resolves client entries, refreshing group and last_seen at most
// once a minute per client (ClientsSeenByGroup and the client list read
// them).
func (d *dicts) clients(ctx context.Context, tx pgx.Tx, entries map[string]client, now int64) (map[string]int64, error) {
	out := make(map[string]int64, len(entries))
	d.mu.Lock()
	cache := d.byTable["dict_client"]
	if cache == nil || len(cache) > dictCacheMax {
		cache = map[string]int64{}
		d.byTable["dict_client"] = cache
		d.touched = map[string]int64{}
	}
	var cached []int64
	for k := range entries {
		if id, ok := cache[k]; ok && now-d.touched[k] < clientTouch {
			cached = append(cached, id)
			out[k] = id
		}
	}
	d.mu.Unlock()
	valid := map[string]int64{}
	if len(cached) > 0 {
		rows, err := tx.Query(ctx, `SELECT id, ip, name FROM dict_client WHERE id = ANY($1::bigint[]) ORDER BY id FOR KEY SHARE`, cached)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var ip string
			var name *string
			if err := rows.Scan(&id, &ip, &name); err != nil {
				rows.Close()
				return nil, err
			}
			k := clientKey(ip, name)
			if out[k] == id {
				valid[k] = id
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	out = valid
	var keys []string
	for k := range entries {
		if _, ok := out[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return out, nil
	}
	var ips []string
	var names, grps []*string
	var seen []int64
	for _, k := range keys {
		c := entries[k]
		ips, names, grps, seen = append(ips, c.ip), append(names, c.name), append(grps, c.grp), append(seen, c.lastSeenAt)
	}
	rows, err := tx.Query(ctx, `INSERT INTO dict_client (ip, name, grp, first_seen, last_seen)
		SELECT ip, name, grp, seen, seen FROM unnest($1::text[], $2::text[], $3::text[], $4::bigint[]) AS t(ip, name, grp, seen)
		ON CONFLICT (ip, name) DO UPDATE SET grp = coalesce(excluded.grp, dict_client.grp),
			last_seen = greatest(dict_client.last_seen, excluded.last_seen)
		RETURNING id, ip, name`, ips, names, grps, seen)
	if err != nil {
		return nil, fmt.Errorf("pgquerylog: dict_client: %w", err)
	}
	defer rows.Close()
	d.mu.Lock()
	defer d.mu.Unlock()
	for rows.Next() {
		var id int64
		var ip string
		var name *string
		if err := rows.Scan(&id, &ip, &name); err != nil {
			return nil, err
		}
		k := clientKey(ip, name)
		cache[k], d.touched[k], out[k] = id, now, id
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) != len(entries) {
		return nil, fmt.Errorf("pgquerylog: dict_client: incomplete dictionary resolution")
	}
	return out, nil
}

// resolveFacts resolves every dictionary value of a batch of fact rows.
func (d *dicts) resolveFacts(ctx context.Context, tx pgx.Tx, rows []sqlitelog.ExportRow, now int64) (*ids, error) {
	nodes, domains, ups, zones, lists, pols := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	etld := map[string]*string{}
	clients := map[string]client{}
	add := func(m map[string]bool, v *string) {
		if v != nil {
			m[*v] = true
		}
	}
	for _, r := range rows {
		nodes[r.Node] = true
		domains[r.Domain] = true
		etld[r.Domain] = r.DomainETLD
		add(domains, r.CNAME)
		add(ups, r.Upstream)
		add(zones, r.Zone)
		add(lists, r.List)
		add(pols, r.Policy)
		k := clientKey(r.ClientIP, r.ClientName)
		if c, ok := clients[k]; !ok || r.TS/1e6 > c.lastSeenAt {
			clients[k] = client{ip: r.ClientIP, name: r.ClientName, grp: r.ClientGrp, lastSeenAt: r.TS / 1e6}
		}
	}
	out := &ids{}
	var err error
	for _, x := range []struct {
		into         *map[string]int64
		table, col   string
		values       map[string]bool
		etldOfDomain map[string]*string
	}{
		{&out.node, "dict_node", "name", nodes, nil},
		{&out.domain, "dict_domain", "name", domains, etld},
		{&out.upstream, "dict_upstream", "addr", ups, nil},
		{&out.zone, "dict_zone", "name", zones, nil},
		{&out.list, "dict_list", "name", lists, nil},
		{&out.policy, "dict_policy", "name", pols, nil},
	} {
		if *x.into, err = d.names(ctx, tx, x.table, x.col, x.values, x.etldOfDomain, now); err != nil {
			return nil, err
		}
	}
	if out.client, err = d.clients(ctx, tx, clients, now); err != nil {
		return nil, err
	}
	return out, nil
}

// keyTables maps a rollup key onto its dictionary.
var keyTables = map[string]struct{ table, col string }{
	"upstream": {"dict_upstream", "addr"},
	"domain":   {"dict_domain", "name"},
	"list":     {"dict_list", "name"},
	"zone":     {"dict_zone", "name"},
}

// resolveRollup resolves the node and key values of rollup rows.
func (d *dicts) resolveRollup(ctx context.Context, tx pgx.Tx, key string, rows []sqlitelog.RollupRow, now int64) (*ids, error) {
	nodes := map[string]bool{}
	keys := map[string]bool{}
	clients := map[string]client{}
	for _, r := range rows {
		nodes[r.Node] = true
		switch key {
		case "client":
			k := clientKey(r.Key, r.ClientName)
			if c, ok := clients[k]; !ok || r.TS > c.lastSeenAt {
				clients[k] = client{ip: r.Key, name: r.ClientName, grp: r.ClientGrp, lastSeenAt: r.TS}
			}
		case "", "qtype":
		default:
			keys[r.Key] = true
		}
	}
	out := &ids{}
	var err error
	if out.node, err = d.names(ctx, tx, "dict_node", "name", nodes, nil, now); err != nil {
		return nil, err
	}
	switch key {
	case "client":
		out.client, err = d.clients(ctx, tx, clients, now)
	case "", "qtype":
	default:
		kt := keyTables[key]
		out.byKey, err = d.names(ctx, tx, kt.table, kt.col, keys, nil, now)
	}
	return out, err
}
