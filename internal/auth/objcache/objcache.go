// Package objcache mirrors one kind of the store in memory, fed by
// Store.Watch. The identity layer reads users, tokens, service accounts,
// role bindings and identity providers from such caches on every request,
// so authentication never costs a store round trip (docs/architecture.md
// §8.2: "checked per request from the store snapshot").
//
// Every driver delivers the current objects as Added, then a Bookmark, then
// live events, and closes the channel of a watcher that falls behind (the
// slow-watcher contract). Cache re-watches with backoff and, at the
// Bookmark of each (re)watch, drops the objects the new initial list did
// not contain, so deletions missed while the channel was closed are not
// kept forever.
package objcache

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store"
)

// Options configures a Cache.
type Options struct {
	// Logger receives watch errors; default discards.
	Logger *slog.Logger
	// MinBackoff and MaxBackoff bound the re-watch delay; defaults 100ms
	// and 10s.
	MinBackoff, MaxBackoff time.Duration
}

// Cache is a watch-fed, read-mostly mirror of one kind. T is the pointer
// type of the kind (for example *v1alpha1.LocalUser). It is safe for
// concurrent use. Objects returned by Get and List are shared: callers
// must not modify them.
type Cache[T store.Object] struct {
	st   store.Store
	kind store.Kind
	opts Options

	mu       sync.RWMutex
	items    map[store.Key]T
	onChange []func()

	syncOnce sync.Once
	synced   chan struct{}
}

// New returns an empty cache of kind. Call Prime to load it and Run to keep
// it current.
func New[T store.Object](st store.Store, kind store.Kind, opts Options) *Cache[T] {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.MinBackoff <= 0 {
		opts.MinBackoff = 100 * time.Millisecond
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = 10 * time.Second
	}
	return &Cache[T]{st: st, kind: kind, opts: opts, items: map[store.Key]T{}, synced: make(chan struct{})}
}

// Kind returns the cached kind.
func (c *Cache[T]) Kind() store.Kind { return c.kind }

// OnChange registers fn to run (outside the cache lock) after every change
// the cache observes: a watch event, Prime, Upsert or Remove. Register
// before Prime and Run.
func (c *Cache[T]) OnChange(fn func()) {
	c.mu.Lock()
	c.onChange = append(c.onChange, fn)
	c.mu.Unlock()
}

// Synced is closed once the cache holds a complete list (after Prime or
// the first Bookmark).
func (c *Cache[T]) Synced() <-chan struct{} { return c.synced }

func (c *Cache[T]) markSynced() { c.syncOnce.Do(func() { close(c.synced) }) }

// Prime lists the kind from the store and replaces the cache content.
func (c *Cache[T]) Prime(ctx context.Context) error {
	items := map[store.Key]T{}
	cont := ""
	for {
		page, err := c.st.List(ctx, c.kind, store.ListOptions{Limit: 500, Continue: cont})
		if err != nil {
			return err
		}
		for _, o := range page.Items {
			if t, ok := o.(T); ok {
				items[store.KeyOf(o)] = t
			}
		}
		if cont = page.Continue; cont == "" {
			break
		}
	}
	c.mu.Lock()
	c.items = items
	fns := c.onChange
	c.mu.Unlock()
	c.markSynced()
	notify(fns)
	return nil
}

// Run watches the kind until ctx is done, re-watching with backoff when the
// watch ends or fails. It returns nil when ctx is cancelled.
func (c *Cache[T]) Run(ctx context.Context) error {
	backoff := c.opts.MinBackoff
	for {
		start := time.Now()
		err := c.watchOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, store.ErrClosed) {
			return nil
		}
		if err != nil {
			c.opts.Logger.Warn("objcache: watch failed; retrying", "kind", c.kind, "err", err, "backoff", backoff)
		}
		if time.Since(start) > time.Minute {
			backoff = c.opts.MinBackoff
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, c.opts.MaxBackoff)
	}
}

func (c *Cache[T]) watchOnce(ctx context.Context) error {
	w, err := c.st.Watch(ctx, c.kind, store.WatchOptions{})
	if err != nil {
		return err
	}
	defer w.Stop()
	seen := map[store.Key]bool{} // keys of the initial list, until the Bookmark
	initial := true
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-w.ResultChan():
			if !ok {
				return nil
			}
			switch ev.Type {
			case store.Bookmark:
				if initial {
					initial = false
					c.prune(seen)
					c.markSynced()
				}
			case store.Added, store.Modified:
				t, ok := ev.Object.(T)
				if !ok {
					continue
				}
				key := store.KeyOf(ev.Object)
				if initial {
					seen[key] = true
				}
				c.put(key, t)
			case store.Deleted:
				if ev.Object == nil {
					continue
				}
				c.remove(store.KeyOf(ev.Object))
			}
		}
	}
}

// prune drops every object whose key is not in keep.
func (c *Cache[T]) prune(keep map[store.Key]bool) {
	c.mu.Lock()
	changed := false
	for k := range c.items {
		if !keep[k] {
			delete(c.items, k)
			changed = true
		}
	}
	fns := c.onChange
	c.mu.Unlock()
	if changed {
		notify(fns)
	}
}

func (c *Cache[T]) put(key store.Key, t T) {
	c.mu.Lock()
	if old, ok := c.items[key]; ok && !newer(t.GetResourceVersion(), old.GetResourceVersion()) {
		c.mu.Unlock()
		return
	}
	c.items[key] = t
	fns := c.onChange
	c.mu.Unlock()
	notify(fns)
}

func (c *Cache[T]) remove(key store.Key) {
	c.mu.Lock()
	_, ok := c.items[key]
	delete(c.items, key)
	fns := c.onChange
	c.mu.Unlock()
	if ok {
		notify(fns)
	}
}

// Upsert records an object the caller has just written, so that the next
// request sees it before the watch event arrives (read-your-writes).
func (c *Cache[T]) Upsert(obj T) { c.put(store.KeyOf(obj), obj) }

// Remove records a deletion the caller has just made.
func (c *Cache[T]) Remove(key store.Key) { c.remove(key) }

// Get returns the object with key.
func (c *Cache[T]) Get(key store.Key) (T, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.items[key]
	return t, ok
}

// List returns every cached object, in no particular order.
func (c *Cache[T]) List() []T {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]T, 0, len(c.items))
	for _, t := range c.items {
		out = append(out, t)
	}
	return out
}

// Len returns the number of cached objects.
func (c *Cache[T]) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}

// newer reports whether resource version next should replace cur. Every
// driver issues increasing integer versions (sqlite and memory counters,
// etcd revisions); when both parse, an older version never replaces a
// newer one (a read-your-writes Upsert racing the watch event). Otherwise
// any different version replaces.
func newer(next, cur string) bool {
	if cur == "" || next == "" {
		return true
	}
	n, err1 := strconv.ParseUint(next, 10, 64)
	c, err2 := strconv.ParseUint(cur, 10, 64)
	if err1 == nil && err2 == nil {
		return n > c
	}
	return next != cur
}

func notify(fns []func()) {
	for _, fn := range fns {
		fn()
	}
}
