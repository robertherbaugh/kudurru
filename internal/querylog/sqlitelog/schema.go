package sqlitelog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// FileName is the query log database under node.dataDir.
const FileName = "querylog.db"

// SchemaVersion is the schema this package writes. A database with a newer
// version is refused (ErrSchemaTooNew) rather than silently mis-read; an
// older one is upgraded in place by the steps in migrations.
//
//  1. v0.1 (dev-docs/design-record.md §11.2)
//  2. query.policy and dict_policy: the deciding policy per row; rows
//     written before the upgrade keep policy NULL
//  3. exact retained-fact domain/hour index (D269)
const SchemaVersion = 3

// ErrSchemaTooNew is returned by Open for a database written by a newer
// kudurru.
var ErrSchemaTooNew = errors.New("sqlitelog: querylog.db schema is newer than this binary")

// Pragmas are the connection settings of dev-docs/design-record.md §11.2, applied
// on every connection open. The writer connection opens before the schema
// is created, so auto_vacuum=INCREMENTAL takes effect on a fresh file (it
// cannot change once tables exist without a VACUUM).
var Pragmas = []string{
	"auto_vacuum(INCREMENTAL)",
	"journal_mode(WAL)",
	"synchronous(NORMAL)",
	"wal_autocheckpoint(2000)",
	"mmap_size(268435456)",
	"busy_timeout(1000)",
	"temp_store(MEMORY)",
}

// dsn builds the modernc.org/sqlite URI. The writer begins every
// transaction IMMEDIATE so the write lock is taken up front; readers are
// query_only.
func dsn(path string, writer bool) string {
	q := url.Values{}
	for _, p := range Pragmas {
		q.Add("_pragma", p)
	}
	if writer {
		q.Set("_txlock", "immediate")
	} else {
		q.Add("_pragma", "query_only(1)")
	}
	return "file:" + (&url.URL{Path: path}).EscapedPath() + "?" + q.Encode()
}

// Latency histogram buckets (upper bounds, ms): 0.5, 1, 2, 5, 10, 25, 50,
// 100, 250, 500, 1000, +Inf, identical to kudurru_dns_request_duration_seconds
// so the dashboard and Prometheus agree.
var latencyBoundsUS = [...]int64{500, 1000, 2000, 5000, 10000, 25000, 50000, 100000, 250000, 500000, 1000000}

const latencyBuckets = len(latencyBoundsUS) + 1

// latBucket returns the histogram bucket index for a latency in µs.
func latBucket(us int64) int {
	for i, b := range latencyBoundsUS {
		if us <= b {
			return i
		}
	}
	return latencyBuckets - 1
}

func latCols(indent string) string {
	var b strings.Builder
	for i := range latencyBuckets {
		if i%6 == 0 {
			b.WriteString("\n" + indent)
		} else {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "lat_b%d INTEGER,", i)
	}
	return b.String()
}

// baseCols are the rollup_1m value columns in accumulator order.
var baseCols = func() []string {
	c := []string{"total", "blocked", "cached", "forwarded", "authoritative", "dropped",
		"rc_noerror", "rc_nxdomain", "rc_servfail", "rc_refused", "rc_other",
		"udp", "tcp", "dot", "doh", "doq", "lat_sum_us", "lat_cnt"}
	for i := range latencyBuckets {
		c = append(c, "lat_b"+strconv.Itoa(i))
	}
	return c
}()

// Accumulator indexes into baseCols.
const (
	cTotal = iota
	cBlocked
	cCached
	cForwarded
	cAuthoritative
	cDropped
	cRcNoError
	cRcNXDomain
	cRcServfail
	cRcRefused
	cRcOther
	cUDP
	cTCP
	cDoT
	cDoH
	cDoQ
	cLatSum
	cLatCnt
	cLatB0
	nBase = cLatB0 + latencyBuckets
)

// upCols are the rollup_upstream_1m value columns in accumulator order.
var upCols = func() []string {
	c := []string{"total", "errors", "timeouts", "lat_sum_us", "lat_cnt"}
	for i := range latencyBuckets {
		c = append(c, "lat_b"+strconv.Itoa(i))
	}
	return c
}()

const (
	uTotal = iota
	uErrors
	uTimeouts
	uLatSum
	uLatCnt
	uLatB0
	nUp = uLatB0 + latencyBuckets
)

func rollupBase(name string) string {
	return `CREATE TABLE ` + name + ` (
  ts INTEGER NOT NULL, node INTEGER NOT NULL,
  total INTEGER NOT NULL, blocked INTEGER NOT NULL, cached INTEGER NOT NULL, forwarded INTEGER NOT NULL, authoritative INTEGER NOT NULL, dropped INTEGER NOT NULL,
  rc_noerror INTEGER, rc_nxdomain INTEGER, rc_servfail INTEGER, rc_refused INTEGER, rc_other INTEGER,
  udp INTEGER, tcp INTEGER, dot INTEGER, doh INTEGER, doq INTEGER,
  lat_sum_us INTEGER, lat_cnt INTEGER,` + latCols("  ") + `
  PRIMARY KEY (ts, node)) WITHOUT ROWID`
}

func rollupUpstream(name string) string {
	return `CREATE TABLE ` + name + ` (ts INTEGER, node INTEGER, upstream INTEGER, total INTEGER, errors INTEGER, timeouts INTEGER, lat_sum_us INTEGER, lat_cnt INTEGER,` +
		latCols("  ") + `
  PRIMARY KEY (ts,node,upstream)) WITHOUT ROWID`
}

func rollupClient(name string) string {
	return `CREATE TABLE ` + name + ` (ts INTEGER, node INTEGER, client INTEGER, total INTEGER, blocked INTEGER, PRIMARY KEY (ts,node,client)) WITHOUT ROWID`
}

func rollupQType(name string) string {
	return `CREATE TABLE ` + name + ` (ts INTEGER, node INTEGER, qtype INTEGER, total INTEGER, PRIMARY KEY (ts,node,qtype)) WITHOUT ROWID`
}

func rollupDomain(name string) string {
	return `CREATE TABLE ` + name + ` (ts INTEGER, node INTEGER, domain INTEGER, total INTEGER, blocked INTEGER, PRIMARY KEY (ts,node,domain)) WITHOUT ROWID`
}

// schema is dev-docs/design-record.md §11.2 verbatim, plus the _1h/_1d tables
// the spec declares "identical shapes". Rollup ts values are unix seconds
// of the UTC-aligned bucket start; query.ts is unix µs.
var schema = []string{
	`CREATE TABLE meta (key TEXT PRIMARY KEY, value) WITHOUT ROWID`,
	`CREATE TABLE dict_domain   (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, etld1 TEXT, first_seen INTEGER, last_seen INTEGER)`,
	`CREATE TABLE dict_client   (id INTEGER PRIMARY KEY, ip TEXT NOT NULL, name TEXT, grp TEXT, first_seen INTEGER, last_seen INTEGER, UNIQUE(ip,name))`,
	`CREATE TABLE dict_upstream (id INTEGER PRIMARY KEY, addr TEXT NOT NULL UNIQUE)`,
	`CREATE TABLE dict_zone     (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE)`,
	`CREATE TABLE dict_list     (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE)`,
	`CREATE TABLE dict_node     (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE)`,
	createDictPolicy,
	`CREATE TABLE query (
  id          INTEGER PRIMARY KEY,
  ts          INTEGER NOT NULL,
  latency_us  INTEGER,
  node        INTEGER NOT NULL REFERENCES dict_node,
  proto       INTEGER NOT NULL,
  client      INTEGER NOT NULL REFERENCES dict_client,
  domain      INTEGER NOT NULL REFERENCES dict_domain,
  qtype       INTEGER NOT NULL, qclass INTEGER NOT NULL DEFAULT 1,
  status      INTEGER NOT NULL,
  rcode       INTEGER NOT NULL,
  reply_type  INTEGER NOT NULL,
  upstream    INTEGER REFERENCES dict_upstream,
  zone        INTEGER REFERENCES dict_zone,
  list        INTEGER REFERENCES dict_list,
  rule        TEXT,
  cname       INTEGER REFERENCES dict_domain,
  dnssec      INTEGER NOT NULL DEFAULT 0,
  ede         INTEGER,
  flags       INTEGER NOT NULL DEFAULT 0,
  ecs         TEXT,
  trace_id    BLOB,
  answer      TEXT,
  policy      INTEGER REFERENCES dict_policy
)`,
	`CREATE INDEX query_ts        ON query(ts)`,
	`CREATE INDEX query_client_ts ON query(client, ts)`,
	`CREATE INDEX query_domain_ts ON query(domain, ts)`,
	`CREATE INDEX query_status_ts ON query(status, ts) WHERE status >= 16`,
	rollupBase("rollup_1m"),
	rollupClient("rollup_client_1m"),
	rollupQType("rollup_qtype_1m"),
	rollupUpstream("rollup_upstream_1m"),
	rollupDomain("rollup_domain_1h"),
	`CREATE TABLE rollup_list_1h     (ts INTEGER, node INTEGER, list INTEGER, hits INTEGER, PRIMARY KEY (ts,node,list)) WITHOUT ROWID`,
	`CREATE TABLE rollup_zone_1h     (ts INTEGER, node INTEGER, zone INTEGER, total INTEGER, PRIMARY KEY (ts,node,zone)) WITHOUT ROWID`,
	rollupBase("rollup_1h"),
	rollupClient("rollup_client_1h"),
	rollupQType("rollup_qtype_1h"),
	rollupUpstream("rollup_upstream_1h"),
	rollupBase("rollup_1d"),
	rollupClient("rollup_client_1d"),
	rollupDomain("rollup_domain_1d"),
	`CREATE INDEX rollup_domain_1h_ts ON rollup_domain_1h(ts)`,
}

// Rollup retention (D59): _1m 35 days, _1h 400 days, _1d forever.
var (
	minuteTables = []string{"rollup_1m", "rollup_client_1m", "rollup_qtype_1m", "rollup_upstream_1m"}
	hourTables   = []string{"rollup_1h", "rollup_client_1h", "rollup_qtype_1h", "rollup_upstream_1h", "rollup_domain_1h", "rollup_list_1h", "rollup_zone_1h"}
)

// Meta keys.
const (
	metaSchemaVersion = "schema_version"
	metaNodeID        = "node_id"
	metaFirstTS       = "first_ts"
	metaPrivacy       = "privacy_mode_at_write"
	metaHourDone      = "rollup_1h_until"
	metaDayDone       = "rollup_1d_until"
	metaLastAnalyze   = "last_analyze"
	metaLastRetention = "last_retention"
)

// createDictPolicy is the dictionary of query.policy (schema 2).
const createDictPolicy = `CREATE TABLE dict_policy   (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE)`

// migrations[v] upgrades a database at schema version v to v+1 inside one
// transaction. Every step is additive (new tables, nullable columns), so
// existing rows stay valid and no table is rewritten.
var migrations = map[int][]string{
	2: domainHourSchema,
	1: {createDictPolicy, `ALTER TABLE query ADD COLUMN policy INTEGER REFERENCES dict_policy`},
}

// migrate creates the schema on an empty database, upgrades an older one
// step by step and refuses newer ones.
func migrate(ctx context.Context, db *sql.DB) (created bool, err error) {
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='meta'`).Scan(&n); err != nil {
		return false, fmt.Errorf("sqlitelog: inspect schema: %w", err)
	}
	if n == 1 {
		var v sql.NullString
		err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, metaSchemaVersion).Scan(&v)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("sqlitelog: read schema version: %w", err)
		}
		ver, _ := strconv.Atoi(v.String)
		switch {
		case ver > SchemaVersion:
			return false, fmt.Errorf("%w (database %d, binary %d)", ErrSchemaTooNew, ver, SchemaVersion)
		case ver == SchemaVersion:
			return false, nil
		case ver < 1:
			return false, fmt.Errorf("sqlitelog: unsupported schema version %q", v.String)
		}
		for ; ver < SchemaVersion; ver++ {
			if err := upgrade(ctx, db, ver); err != nil {
				return false, err
			}
		}
		return false, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range append(append([]string{}, schema...), domainHourSchema...) {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return false, fmt.Errorf("sqlitelog: create schema: %w\n%s", err, stmt)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES(?,?)`, metaSchemaVersion, SchemaVersion); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// upgrade runs migrations[from] and records schema version from+1.
func upgrade(ctx context.Context, db *sql.DB, from int) error {
	steps, ok := migrations[from]
	if !ok {
		return fmt.Errorf("sqlitelog: no upgrade from schema version %d", from)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range steps {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("sqlitelog: upgrade schema %d to %d: %w\n%s", from, from+1, err, stmt)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE meta SET value=? WHERE key=?`, from+1, metaSchemaVersion); err != nil {
		return err
	}
	return tx.Commit()
}
