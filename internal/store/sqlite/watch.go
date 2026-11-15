package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// Buffer sizes of a watcher: in holds published changes not yet forwarded;
// out is the result channel.
const (
	watchInBuffer  = 1024
	watchOutBuffer = 256
)

// change is one committed write as published to watchers. obj is shared
// and read-only; watchers forward deep copies.
type change struct {
	seq uint64
	typ store.EventType
	key store.Key
	obj store.Object
}

// broadcaster fans committed changes out to watchers without ever
// blocking the writer: a watcher whose buffer is full is unsubscribed and
// marked lagging, and catches up from the change feed on its own goroutine.
type broadcaster struct {
	mu     sync.Mutex
	subs   map[*watcher]struct{}
	all    map[*watcher]struct{} // includes temporarily unsubscribed slow watches
	closed bool
}

func newBroadcaster() *broadcaster {
	return &broadcaster{subs: map[*watcher]struct{}{}, all: map[*watcher]struct{}{}}
}

// subscribe adds w; it returns false once the broadcaster is closed.
func (b *broadcaster) subscribe(w *watcher) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || w.ctx.Err() != nil {
		return false
	}
	w.lagged.Store(false)
	b.subs[w] = struct{}{}
	b.all[w] = struct{}{}
	return true
}

func (b *broadcaster) unsubscribe(w *watcher) {
	b.mu.Lock()
	delete(b.subs, w)
	delete(b.all, w)
	b.mu.Unlock()
}

// publish offers c to every matching watcher. The caller holds the store's
// write lock, so changes arrive in resourceVersion order.
func (b *broadcaster) publish(c change) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for w := range b.subs {
		if w.ki.kind != c.key.Kind || (w.ns != "" && w.ns != c.key.Namespace) {
			continue
		}
		select {
		case w.in <- c:
		default:
			delete(b.subs, w)
			w.lagged.Store(true)
			select {
			case w.wake <- struct{}{}:
			default:
			}
		}
	}
}

// close stops every watcher and refuses new ones.
func (b *broadcaster) close() {
	b.mu.Lock()
	b.closed = true
	ws := make([]*watcher, 0, len(b.subs))
	for w := range b.all {
		ws = append(ws, w)
	}
	b.mu.Unlock()
	for _, w := range ws {
		w.Stop()
	}
}

// reset invalidates consumers of a previous source branch without closing
// the broadcaster itself. New watches list the new complete image.
func (b *broadcaster) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for w := range b.all {
		w.cancel()
		delete(b.subs, w)
		delete(b.all, w)
	}
}

// watcher is a store.Watcher. One goroutine (run) is the only sender on
// out and closes it when it exits.
type watcher struct {
	s      *Store
	ki     *kindInfo
	ns     string
	in     chan change
	out    chan store.Event
	wake   chan struct{}
	lagged atomic.Bool

	ctx    context.Context
	cancel context.CancelFunc
	exited chan struct{}
	once   sync.Once

	sent      uint64             // highest resourceVersion forwarded or covered
	known     map[store.Key]bool // objects the consumer has been told about
	lineage   string
	qualified bool
}

// ResultChan implements store.Watcher.
func (w *watcher) ResultChan() <-chan store.Event { return w.out }

// Stop implements store.Watcher. It is idempotent and returns after the
// result channel is closed.
func (w *watcher) Stop() {
	w.once.Do(func() {
		w.cancel()
		w.s.bus.unsubscribe(w)
	})
	<-w.exited
}

// Watch implements store.Store:
//
//   - With opts.ResourceVersion empty, every current object of the kind
//     (filtered by opts.Namespace) arrives as store.Added, then one
//     store.Bookmark whose Object is an empty object of the kind carrying
//     the resourceVersion the list was read at.
//   - With opts.ResourceVersion set, the changes after it are replayed
//     from the change feed instead (coalesced: one event per object state
//     still visible, Deleted with the final object), then the Bookmark. If
//     the feed has been compacted past that version, the watch relists as
//     in the first case.
//   - Then live Added/Modified/Deleted events follow in resourceVersion
//     order. A consumer slower than the writers never blocks them and
//     never loses events: its watcher falls back to the change feed and
//     catches up (after a compaction it relists, emitting Deleted for
//     objects that disappeared).
//
// The watch ends on Stop, when ctx is done, or on Close.
func (s *Store) Watch(ctx context.Context, kind store.Kind, opts store.WatchOptions) (_ store.Watcher, err error) {
	defer s.observe(opWatch, time.Now(), &err)
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.isClosed() {
		return nil, store.ErrClosed
	}
	ki, err := s.kinds.get(kind)
	if err != nil {
		return nil, err
	}
	var from uint64
	a := s.Authority()
	qualified := a.Mode != replication.Standalone
	resume := opts.ResourceVersion != ""
	if resume {
		rv := opts.ResourceVersion
		if lineage, counter, ok := strings.Cut(rv, ":"); ok {
			if lineage != a.WatchLineage {
				return nil, ErrWatchLineage
			}
			rv = counter
		} else if qualified {
			return nil, ErrWatchLineage
		}
		if from, err = strconv.ParseUint(rv, 10, 64); err != nil {
			return nil, fmt.Errorf("store/sqlite: invalid resourceVersion %q", opts.ResourceVersion)
		}
		// A version this store never issued (a resume against a restored
		// or different state.db) cannot be replayed; the caller relists.
		if head := s.head.Load(); from > head {
			return nil, fmt.Errorf("%w: %d, store is at %d", ErrResourceVersionTooNew, from, head)
		}
	}
	wctx, cancel := context.WithCancel(ctx)
	w := &watcher{
		s: s, ki: ki, ns: opts.Namespace,
		in:      make(chan change, watchInBuffer),
		out:     make(chan store.Event, watchOutBuffer),
		wake:    make(chan struct{}, 1),
		ctx:     wctx,
		cancel:  cancel,
		exited:  make(chan struct{}),
		known:   map[store.Key]bool{},
		lineage: a.WatchLineage, qualified: qualified,
	}
	// Subscribe before reading so no commit falls between the read and
	// the live stream; duplicates are dropped by resourceVersion.
	if !s.bus.subscribe(w) {
		cancel()
		return nil, store.ErrClosed
	}
	s.watchersAdd(1)
	go w.run(resume, from)
	return w, nil
}

func (w *watcher) run(resume bool, from uint64) {
	defer close(w.exited)
	defer close(w.out)
	defer w.s.watchersAdd(-1)
	defer w.s.bus.unsubscribe(w)

	if resume {
		if !w.catchUp(from, true) {
			return
		}
	} else if !w.relist() {
		return
	}
	for {
		select {
		case <-w.ctx.Done():
			return
		case c := <-w.in:
			if !w.forward(c) {
				return
			}
		case <-w.wake:
			if !w.drain() {
				return
			}
			if w.lagged.Load() {
				w.s.catchupObserved(w.ki.kind)
				if !w.s.bus.subscribe(w) {
					return
				}
				if !w.catchUp(w.sent, false) {
					return
				}
			}
		}
	}
}

// send delivers ev unless the watch ends first.
func (w *watcher) send(ev store.Event) bool {
	if w.ctx.Err() != nil {
		return false
	}
	if ev.Cursor == "" {
		ev.Cursor = w.cursor(w.sent)
	}
	select {
	case w.out <- ev:
		return true
	case <-w.ctx.Done():
		return false
	}
}

// emit sends a data event and tracks what the consumer knows.
func (w *watcher) emit(seq uint64, typ store.EventType, key store.Key, obj store.Object) bool {
	if seq <= w.sent {
		return true
	}
	if !w.send(store.Event{Type: typ, Object: obj, Cursor: w.cursor(seq)}) {
		return false
	}
	w.sent = seq
	if typ == store.Deleted {
		delete(w.known, key)
	} else {
		w.known[key] = true
	}
	return true
}

func (w *watcher) forward(c change) bool {
	if c.seq <= w.sent {
		return true
	}
	return w.emit(c.seq, c.typ, c.key, c.obj.DeepCopyObject().(store.Object))
}

// drain forwards whatever is buffered without blocking on in.
func (w *watcher) drain() bool {
	for {
		select {
		case c := <-w.in:
			if !w.forward(c) {
				return false
			}
		default:
			return true
		}
	}
}

func (w *watcher) bookmark() bool {
	bm := w.ki.newObject()
	bm.SetResourceVersion(w.cursor(w.sent))
	return w.send(store.Event{Type: store.Bookmark, Object: bm})
}

func (w *watcher) cursor(rv uint64) string {
	counter := strconv.FormatUint(rv, 10)
	if w.qualified {
		return w.lineage + ":" + counter
	}
	return counter
}

// relist sends the current objects as Added (and Deleted for objects the
// consumer knew that no longer exist), then a Bookmark.
func (w *watcher) relist() bool {
	objs, rv, err := w.s.readKind(w.ctx, w.ki, w.ns)
	if err != nil {
		w.fail(err)
		return false
	}
	present := make(map[store.Key]bool, len(objs))
	for _, o := range objs {
		present[store.KeyOf(o)] = true
	}
	for key := range w.known {
		if !present[key] {
			gone := w.ki.newObject()
			gone.SetNamespace(key.Namespace)
			gone.SetName(key.Name)
			gone.SetResourceVersion(strconv.FormatUint(rv, 10))
			if !w.send(store.Event{Type: store.Deleted, Object: gone}) {
				return false
			}
			delete(w.known, key)
		}
	}
	for _, o := range objs {
		if !w.send(store.Event{Type: store.Added, Object: o}) {
			return false
		}
		w.known[store.KeyOf(o)] = true
	}
	if rv > w.sent {
		w.sent = rv
	}
	return w.bookmark()
}

// catchUp replays the change feed after from, or relists when the feed no
// longer reaches back that far. A Bookmark follows when bookmark is set
// (a resumed watch) and always after a relist.
func (w *watcher) catchUp(from uint64, bookmark bool) bool {
	evs, head, ok, err := w.s.replay(w.ctx, w.ki, w.ns, from)
	if err != nil {
		w.fail(err)
		return false
	}
	if !ok {
		return w.relist()
	}
	for _, e := range evs {
		if !w.emit(e.seq, e.typ, e.key, e.obj) {
			return false
		}
	}
	if head > w.sent {
		w.sent = head
	}
	if bookmark {
		return w.bookmark()
	}
	return true
}

// fail ends the watch after an unexpected read error; the consumer sees the
// channel close and re-watches.
func (w *watcher) fail(err error) {
	if w.ctx.Err() == nil {
		w.s.log.Warn("watch ended after a read error", "kind", w.ki.kind, "err", err)
	}
}

// readKind returns every object of a kind and the resourceVersion the read
// is consistent with.
func (s *Store) readKind(ctx context.Context, ki *kindInfo, ns string) ([]store.Object, uint64, error) {
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	head, _, err := feedBounds(ctx, tx)
	if err != nil {
		return nil, 0, err
	}
	q := `SELECT data FROM objects WHERE kind = ?`
	args := []any{string(ki.kind)}
	if ns != "" {
		q += ` AND namespace = ?`
		args = append(args, ns)
	}
	rows, err := tx.QueryContext(ctx, q+` ORDER BY namespace, name`, args...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	var out []store.Object
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, 0, err
		}
		o := ki.newObject()
		if err := decodeInto(ki, data, o); err != nil {
			return nil, 0, err
		}
		out = append(out, o)
	}
	return out, head, rows.Err()
}

// feedBounds reads the resourceVersion counter and the compaction horizon.
func feedBounds(ctx context.Context, q queryer) (head, compacted uint64, err error) {
	var h, c string
	if err = q.QueryRowContext(ctx, `SELECT
		(SELECT value FROM meta WHERE key = ?),
		(SELECT value FROM meta WHERE key = ?)`, metaResourceVersion, metaFeedCompacted).Scan(&h, &c); err != nil {
		return 0, 0, fmt.Errorf("store/sqlite: read feed bounds: %w", err)
	}
	if head, err = strconv.ParseUint(h, 10, 64); err != nil {
		return 0, 0, err
	}
	compacted, err = strconv.ParseUint(c, 10, 64)
	return head, compacted, err
}

// feedEvent is one coalesced change replayed from the feed.
type feedEvent struct {
	seq uint64
	typ store.EventType
	key store.Key
	obj store.Object
}

// replay reads the change feed after from in one read transaction and
// coalesces it into the events a consumer at from needs: for each feed row
// whose object version is still the current one, that object; for each
// delete, the tombstone. ok is false when the feed was compacted past from.
func (s *Store) replay(ctx context.Context, ki *kindInfo, ns string, from uint64) (evs []feedEvent, head uint64, ok bool, err error) {
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, 0, false, err
	}
	defer func() { _ = tx.Rollback() }()
	head, compacted, err := feedBounds(ctx, tx)
	if err != nil {
		return nil, 0, false, err
	}
	if from < compacted {
		return nil, head, false, nil
	}
	q := `SELECT c.seq, c.type, c.namespace, c.name, c.tombstone, o.resource_version, o.data
		FROM changes AS c
		LEFT JOIN objects AS o ON o.kind = c.kind AND o.namespace = c.namespace AND o.name = c.name
		WHERE c.kind = ? AND c.seq > ?`
	args := []any{string(ki.kind), int64(from)} // #nosec G115 -- bounded by SQLite's int64 counter
	if ns != "" {
		q += ` AND c.namespace = ?`
		args = append(args, ns)
	}
	rows, err := tx.QueryContext(ctx, q+` ORDER BY c.seq`, args...)
	if err != nil {
		return nil, 0, false, err
	}
	defer func() { _ = rows.Close() }()
	pendingAdd := map[store.Key]bool{}
	for rows.Next() {
		var (
			seq        int64
			typ        string
			key        = store.Key{Kind: ki.kind}
			tombstone  sql.NullString
			currentRV  sql.NullInt64
			currentDoc sql.NullString
		)
		if err := rows.Scan(&seq, &typ, &key.Namespace, &key.Name, &tombstone, &currentRV, &currentDoc); err != nil {
			return nil, 0, false, err
		}
		useq := uint64(seq) // #nosec G115 -- resourceVersions are positive
		switch store.EventType(typ) {
		case store.Deleted:
			delete(pendingAdd, key)
			o := ki.newObject()
			if tombstone.Valid {
				if err := decodeInto(ki, tombstone.String, o); err != nil {
					return nil, 0, false, err
				}
			} else {
				o.SetNamespace(key.Namespace)
				o.SetName(key.Name)
				o.SetResourceVersion(strconv.FormatUint(useq, 10))
			}
			evs = append(evs, feedEvent{seq: useq, typ: store.Deleted, key: key, obj: o})
		default:
			if !currentRV.Valid || currentRV.Int64 != seq {
				// Superseded by a later row of this replay.
				if store.EventType(typ) == store.Added {
					pendingAdd[key] = true
				}
				continue
			}
			o := ki.newObject()
			if err := decodeInto(ki, currentDoc.String, o); err != nil {
				return nil, 0, false, err
			}
			t := store.EventType(typ)
			if pendingAdd[key] {
				t = store.Added
				delete(pendingAdd, key)
			}
			evs = append(evs, feedEvent{seq: useq, typ: t, key: key, obj: o})
		}
	}
	return evs, head, true, rows.Err()
}
