package sqlitelog

import (
	"container/list"
	"context"
	"database/sql"
	"fmt"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// touchInterval bounds how stale a dictionary's last_seen may get on disk:
// a cache hit rewrites last_seen only when the stored value is this much
// older than the event (one UPDATE per key per hour instead of per query).
const touchInterval = 3600

// dictEntry is one cached dictionary row.
type dictEntry struct {
	key  string
	id   int64
	seen int64 // last_seen on disk (unix s)
	mem  int64 // newest event time seen (unix s)
}

// lru is a fixed-size least-recently-used map from dictionary key to id.
type lru struct {
	cap int
	ll  *list.List
	m   map[string]*list.Element
}

func newLRU(capacity int) *lru {
	return &lru{cap: capacity, ll: list.New(), m: make(map[string]*list.Element, capacity)}
}

func (l *lru) get(k string) (*dictEntry, bool) {
	if el, ok := l.m[k]; ok {
		l.ll.MoveToFront(el)
		return el.Value.(*dictEntry), true
	}
	return nil, false
}

func (l *lru) put(e *dictEntry) {
	if el, ok := l.m[e.key]; ok {
		el.Value = e
		l.ll.MoveToFront(el)
		return
	}
	l.m[e.key] = l.ll.PushFront(e)
	for l.ll.Len() > l.cap {
		old := l.ll.Back()
		l.ll.Remove(old)
		delete(l.m, old.Value.(*dictEntry).key)
	}
}

func (l *lru) purge() {
	l.ll.Init()
	clear(l.m)
}

func (l *lru) each(fn func(*dictEntry)) {
	for el := l.ll.Front(); el != nil; el = el.Next() {
		fn(el.Value.(*dictEntry))
	}
}

// dict resolves names to dictionary ids through an LRU, inserting on miss.
type dict struct {
	table  string
	cache  *lru
	upsert *sql.Stmt // returns id
	touch  *sql.Stmt // nil for dictionaries without last_seen
}

// dictionaries are the writer's id caches, used only under Writer.mu.
type dictionaries struct {
	domain, client, upstream, zone, list *dict
}

func prepareDicts(ctx context.Context, db *sql.DB, size int) (*dictionaries, error) {
	small := max(size/16, 256)
	mk := func(table string, capacity int, upsert, touch string) (*dict, error) {
		d := &dict{table: table, cache: newLRU(capacity)}
		var err error
		if d.upsert, err = db.PrepareContext(ctx, upsert); err != nil {
			return nil, fmt.Errorf("sqlitelog: prepare %s upsert: %w", table, err)
		}
		if touch != "" {
			if d.touch, err = db.PrepareContext(ctx, touch); err != nil {
				return nil, fmt.Errorf("sqlitelog: prepare %s touch: %w", table, err)
			}
		}
		return d, nil
	}
	var (
		ds  dictionaries
		err error
	)
	if ds.domain, err = mk("dict_domain", size,
		`INSERT INTO dict_domain(name, etld1, first_seen, last_seen) VALUES(?1, ?2, ?3, ?3)
		 ON CONFLICT(name) DO UPDATE SET last_seen=max(coalesce(last_seen,0), excluded.last_seen) RETURNING id, last_seen`,
		`UPDATE dict_domain SET last_seen=?2 WHERE id=?1 AND coalesce(last_seen,0) < ?2`); err != nil {
		return nil, err
	}
	if ds.client, err = mk("dict_client", small*4,
		`INSERT INTO dict_client(ip, name, grp, first_seen, last_seen) VALUES(?1, ?2, ?3, ?4, ?4)
		 ON CONFLICT(ip, name) DO UPDATE SET last_seen=max(coalesce(last_seen,0), excluded.last_seen), grp=excluded.grp RETURNING id, last_seen`,
		`UPDATE dict_client SET last_seen=?2 WHERE id=?1 AND coalesce(last_seen,0) < ?2`); err != nil {
		return nil, err
	}
	name := func(table, col string) string {
		return `INSERT INTO ` + table + `(` + col + `) VALUES(?1) ON CONFLICT(` + col + `) DO UPDATE SET ` + col + `=excluded.` + col + ` RETURNING id, 0`
	}
	if ds.upstream, err = mk("dict_upstream", small, name("dict_upstream", "addr"), ""); err != nil {
		return nil, err
	}
	if ds.zone, err = mk("dict_zone", small, name("dict_zone", "name"), ""); err != nil {
		return nil, err
	}
	if ds.list, err = mk("dict_list", small, name("dict_list", "name"), ""); err != nil {
		return nil, err
	}
	return &ds, nil
}

func (ds *dictionaries) all() []*dict {
	return []*dict{ds.domain, ds.client, ds.upstream, ds.zone, ds.list}
}

// purge drops every cached id; called after a rolled-back transaction whose
// inserts may have been cached.
func (ds *dictionaries) purge() {
	for _, d := range ds.all() {
		d.cache.purge()
	}
}

func (ds *dictionaries) close() {
	for _, d := range ds.all() {
		if d == nil {
			continue
		}
		_ = d.upsert.Close()
		if d.touch != nil {
			_ = d.touch.Close()
		}
	}
}

// id returns the dictionary id for key inside tx. args are the upsert
// arguments after the key's own columns (see prepareDicts); seen is the
// event time in unix seconds.
func (d *dict) id(ctx context.Context, tx *sql.Tx, key string, seen int64, args ...any) (int64, error) {
	if e, ok := d.cache.get(key); ok {
		if seen > e.mem {
			e.mem = seen
		}
		if d.touch != nil && seen-e.seen >= touchInterval {
			if _, err := tx.StmtContext(ctx, d.touch).ExecContext(ctx, e.id, seen); err != nil {
				return 0, fmt.Errorf("sqlitelog: touch %s: %w", d.table, err)
			}
			e.seen = seen
		}
		return e.id, nil
	}
	var id, last int64
	var lastNull sql.NullInt64
	if err := tx.StmtContext(ctx, d.upsert).QueryRowContext(ctx, args...).Scan(&id, &lastNull); err != nil {
		return 0, fmt.Errorf("sqlitelog: upsert %s: %w", d.table, err)
	}
	last = lastNull.Int64
	d.cache.put(&dictEntry{key: key, id: id, seen: last, mem: max(seen, last)})
	return id, nil
}

// domainID returns the dict_domain id of name.
func (ds *dictionaries) domainID(ctx context.Context, tx *sql.Tx, name string, seen int64) (int64, error) {
	return ds.domain.id(ctx, tx, name, seen, name, etld1(name), seen)
}

// clientID returns the dict_client id of (ip, name).
func (ds *dictionaries) clientID(ctx context.Context, tx *sql.Tx, ip, name, group string, seen int64) (int64, error) {
	var grp any
	if group != "" {
		grp = group
	}
	return ds.client.id(ctx, tx, ip+"\x00"+name, seen, ip, name, grp, seen)
}

// nameID returns the id of a name-only dictionary entry.
func (d *dict) nameID(ctx context.Context, tx *sql.Tx, name string) (int64, error) {
	return d.id(ctx, tx, name, 0, name)
}

// flushSeen writes back last_seen values newer than the stored ones (on
// close, so pruning by last_seen stays accurate).
func (ds *dictionaries) flushSeen(ctx context.Context, tx *sql.Tx) error {
	for _, d := range []*dict{ds.domain, ds.client} {
		var err error
		d.cache.each(func(e *dictEntry) {
			if err == nil && e.mem > e.seen {
				_, err = tx.StmtContext(ctx, d.touch).ExecContext(ctx, e.id, e.mem)
				e.seen = e.mem
			}
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// etld1 returns the registrable domain (eTLD+1) or nil.
func etld1(name string) any {
	if name == "" || !strings.Contains(name, ".") {
		return nil
	}
	e, err := publicsuffix.EffectiveTLDPlusOne(name)
	if err != nil {
		return nil
	}
	return e
}
