package auditlog

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	auditv1 "github.com/robertherbaugh/kudurru/api/audit/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/audit/auditkeys"
)

// Reader defaults.
const (
	// DefaultListTTL is how long the list of batches is reused, so the
	// pages of one export do not list again each time.
	DefaultListTTL = time.Second
	// DefaultBodyCache is how many batch bodies are kept in memory
	// (batches are immutable, so a cached body never goes stale).
	DefaultBodyCache = 64
	// pendingMax bounds the serving node's unshipped events read.
	pendingMax = 1000
)

// Reader reads the events of every node's AuditLog batches; it implements
// audit.Reader. Events are ordered by (time, source, id); the cursor of a
// page is opaque (Event.Cursor), and Event.UID names an event across
// nodes. Query.AfterID and BeforeID are refused (audit.ErrBadCursor).
type Reader struct {
	// Client reads AuditLogs; an uncached reader (the manager's API
	// reader), since only the batches a query needs are fetched.
	Client client.Reader
	// Namespace is the release namespace.
	Namespace string
	// Keys captures independent trust for production verification.
	Keys *auditkeys.Source
	// SealKeys is the explicit legacy test seam.
	// SealKeys verify batch seals using the cluster encryption keys.
	SealKeys [][]byte
	// VerifyPending checks the local spool, including events not shipped yet.
	VerifyPending func(context.Context) error
	// Pending returns the serving node's events not shipped yet
	// (audit.Shipper.Pending), so a change shows on the node that made it
	// at once; PendingSource is their spool id. nil adds none.
	Pending       func(ctx context.Context, n int) ([]audit.Event, error)
	PendingSource string
	// ListTTL and BodyCache default to DefaultListTTL and
	// DefaultBodyCache.
	ListTTL   time.Duration
	BodyCache int

	mu      sync.Mutex
	metas   []batchMeta
	metasAt time.Time
	bodies  map[types.UID]*auditv1.AuditLog
	lru     []types.UID
}

var _ audit.Reader = (*Reader)(nil)

// Verify reads fresh batches from the API server, bypassing the read caches.
func (r *Reader) Verify(ctx context.Context) error {
	snapshot, err := r.SnapshotVerification(ctx)
	if err != nil {
		return err
	}
	if err = snapshot.Verifier.Verify(ctx); err != nil {
		return err
	}
	return snapshot.Current(ctx)
}

// SnapshotVerification never substitutes live trust for the captured ring.
func (r *Reader) SnapshotVerification(ctx context.Context) (audit.VerificationSnapshot, error) {
	keys, reader, namespace, legacy, pending := r.Keys, r.Client, r.Namespace, r.SealKeys, r.VerifyPending
	var ring *auditkeys.Ring
	identity := "legacy"
	current := func(context.Context) error { return nil }
	if keys != nil {
		loaded, id, err := keys.Load(ctx)
		if err != nil {
			return audit.VerificationSnapshot{}, err
		}
		ring, identity = loaded.VerificationOnly(), id
		current = func(ctx context.Context) error { return keys.Current(ctx, id) }
	}
	verify := audit.VerifierFunc(func(ctx context.Context) error {
		if err := verifyWithRing(ctx, reader, namespace, legacy, ring); err != nil {
			return err
		}
		if pending != nil {
			return pending(ctx)
		}
		return nil
	})
	return audit.VerificationSnapshot{Verifier: verify, Identity: identity, Current: current}, nil
}

// batchMeta is what the reader knows of a batch without its events.
type batchMeta struct {
	name   string
	uid    types.UID
	from   time.Time
	until  time.Time
	kinds  map[string]bool // nil: unknown
	actors map[string]bool // nil: unknown
}

// candidate is a batch to read, or the serving node's pending events.
type candidate struct {
	meta    *batchMeta
	pending []audit.Event
	from    time.Time
	until   time.Time
}

// position orders events across batches.
type position struct {
	ts     int64 // unix µs
	source string
	id     int64
}

func (a position) less(b position) bool {
	if a.ts != b.ts {
		return a.ts < b.ts
	}
	if a.source != b.source {
		return a.source < b.source
	}
	return a.id < b.id
}

func (a position) String() string {
	return "c1." + strconv.FormatInt(a.ts, 10) + "." + a.source + "." + strconv.FormatInt(a.id, 10)
}

func parsePosition(s string) (position, bool) {
	parts := strings.Split(s, ".")
	if len(parts) != 4 || parts[0] != "c1" || parts[2] == "" {
		return position{}, false
	}
	ts, err1 := strconv.ParseInt(parts[1], 10, 64)
	id, err2 := strconv.ParseInt(parts[3], 10, 64)
	if err1 != nil || err2 != nil {
		return position{}, false
	}
	return position{ts: ts, source: parts[2], id: id}, true
}

// Read implements audit.Reader.
func (r *Reader) Read(ctx context.Context, q audit.Query) ([]audit.Event, error) {
	if q.AfterID != 0 || q.BeforeID != 0 {
		return nil, audit.ErrBadCursor
	}
	var cur *position
	if q.Cursor != "" {
		p, ok := parsePosition(q.Cursor)
		if !ok {
			return nil, audit.ErrBadCursor
		}
		cur = &p
	}
	limit := clampLimit(q.Limit)
	metas, err := r.list(ctx)
	if err != nil {
		return nil, err
	}
	var cands []candidate
	for i := range metas {
		m := &metas[i]
		if m.skip(q, cur) {
			continue
		}
		cands = append(cands, candidate{meta: m, from: m.from, until: m.until})
	}
	if r.Pending != nil {
		evs, err := r.Pending(ctx, pendingMax)
		if err != nil {
			return nil, err
		}
		if len(evs) > 0 {
			c := candidate{pending: evs, from: evs[0].Time, until: evs[0].Time}
			for _, ev := range evs {
				if ev.Time.Before(c.from) {
					c.from = ev.Time
				}
				if ev.Time.After(c.until) {
					c.until = ev.Time
				}
			}
			cands = append(cands, c)
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if q.Desc {
			return cands[i].until.After(cands[j].until)
		}
		return cands[i].from.Before(cands[j].from)
	})

	type keyed struct {
		pos position
		ev  audit.Event
	}
	var out []keyed
	seen := map[position]bool{}
	order := func() {
		sort.Slice(out, func(i, j int) bool {
			if q.Desc {
				return out[j].pos.less(out[i].pos)
			}
			return out[i].pos.less(out[j].pos)
		})
	}
	for _, c := range cands {
		if len(out) >= limit {
			order()
			out = out[:limit]
			// Every event of c is at or after c.from (before c.until for
			// newest first); once the page ends before that, c and the
			// candidates after it cannot change it.
			edge := out[limit-1].pos.ts
			if !q.Desc && edge < c.from.UnixMicro() || q.Desc && edge > c.until.UnixMicro() {
				break
			}
		}
		var evs []audit.Event
		source := r.PendingSource
		if c.meta != nil {
			body, err := r.body(ctx, c.meta)
			if err != nil {
				return nil, err
			}
			if body == nil {
				continue // pruned since the list
			}
			source = body.Spec.Source
			evs = make([]audit.Event, 0, len(body.Spec.Events))
			for i := range body.Spec.Events {
				evs = append(evs, toEvent(&body.Spec, &body.Spec.Events[i]))
			}
		} else {
			evs = c.pending
		}
		for _, ev := range evs {
			pos := position{ts: ev.Time.UnixMicro(), source: source, id: ev.ID}
			if seen[pos] || !match(q, ev) {
				continue
			}
			if cur != nil && (!q.Desc && !cur.less(pos) || q.Desc && !pos.less(*cur)) {
				continue
			}
			seen[pos] = true
			ev.UID = short(source) + "-" + strconv.FormatInt(ev.ID, 10)
			ev.Cursor = pos.String()
			out = append(out, keyed{pos: pos, ev: ev})
		}
	}
	order()
	if len(out) > limit {
		out = out[:limit]
	}
	evs := make([]audit.Event, len(out))
	for i := range out {
		evs[i] = out[i].ev
	}
	return evs, nil
}

// skip reports whether no event of the batch can match q after cur.
func (m *batchMeta) skip(q audit.Query, cur *position) bool {
	switch {
	case !q.Since.IsZero() && m.until.Before(q.Since):
		return true
	case cur != nil && !q.Desc && m.until.UnixMicro() < cur.ts:
		return true
	case cur != nil && q.Desc && m.from.UnixMicro() > cur.ts:
		return true
	case q.Kind != "" && m.kinds != nil && !m.kinds[q.Kind]:
		return true
	}
	if q.Actor != "" && m.actors != nil {
		for a := range m.actors {
			if a == q.Actor || strings.HasSuffix(a, ":"+q.Actor) {
				return false
			}
		}
		return true
	}
	return false
}

// match is the filter of audit.Memory.
func match(q audit.Query, ev audit.Event) bool {
	switch {
	case q.Kind != "" && ev.Resource.Kind != q.Kind:
		return false
	case q.Name != "" && ev.Resource.Name != q.Name:
		return false
	case q.Actor != "" && ev.Actor.String() != q.Actor && ev.Actor.Name != q.Actor:
		return false
	case !q.Since.IsZero() && ev.Time.Before(q.Since):
		return false
	}
	return true
}

// list returns the batches, reusing the last list for ListTTL.
func (r *Reader) list(ctx context.Context) ([]batchMeta, error) {
	ttl := r.ListTTL
	if ttl <= 0 {
		ttl = DefaultListTTL
	}
	r.mu.Lock()
	if r.metas != nil && time.Since(r.metasAt) < ttl {
		metas := r.metas
		r.mu.Unlock()
		return metas, nil
	}
	r.mu.Unlock()
	var list metav1.PartialObjectMetadataList
	list.SetGroupVersionKind(auditv1.GroupVersion.WithKind("AuditLogList"))
	if err := r.Client.List(ctx, &list, client.InNamespace(r.Namespace)); err != nil {
		return nil, fmt.Errorf("auditlog: list batches: %w", err)
	}
	metas := make([]batchMeta, 0, len(list.Items))
	for i := range list.Items {
		it := &list.Items[i]
		from, err1 := time.Parse(time.RFC3339Nano, it.Annotations[auditv1.AnnotationFrom])
		until, err2 := time.Parse(time.RFC3339Nano, it.Annotations[auditv1.AnnotationUntil])
		if err1 != nil || err2 != nil {
			// Not written by kudurru: read it, wherever it falls.
			from, until = time.Time{}, time.Unix(1<<40, 0)
		}
		metas = append(metas, batchMeta{name: it.Name, uid: it.UID, from: from, until: until,
			kinds: setOf(it.Annotations, auditv1.AnnotationKinds), actors: setOf(it.Annotations, auditv1.AnnotationActors)})
	}
	r.mu.Lock()
	r.metas, r.metasAt = metas, time.Now()
	r.mu.Unlock()
	return metas, nil
}

func setOf(annotations map[string]string, key string) map[string]bool {
	v, ok := annotations[key]
	if !ok {
		return nil
	}
	out := map[string]bool{}
	for _, s := range strings.Split(v, ",") {
		if s != "" {
			out[s] = true
		}
	}
	return out
}

// body returns the batch, from the cache when it holds it; nil when the
// batch is gone.
func (r *Reader) body(ctx context.Context, m *batchMeta) (*auditv1.AuditLog, error) {
	r.mu.Lock()
	if b, ok := r.bodies[m.uid]; ok {
		r.mu.Unlock()
		return b, nil
	}
	r.mu.Unlock()
	var b auditv1.AuditLog
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: m.name}, &b)
	switch {
	case apierrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("auditlog: read batch %s: %w", m.name, err)
	case b.UID != m.uid:
		return nil, nil // replaced since the list
	}
	size := r.BodyCache
	if size <= 0 {
		size = DefaultBodyCache
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.bodies == nil {
		r.bodies = map[types.UID]*auditv1.AuditLog{}
	}
	if _, ok := r.bodies[m.uid]; !ok {
		r.bodies[m.uid] = &b
		r.lru = append(r.lru, m.uid)
		for len(r.lru) > size {
			delete(r.bodies, r.lru[0])
			r.lru = r.lru[1:]
		}
	}
	return &b, nil
}

// forget drops the cached list (after a prune).
func (r *Reader) forget() {
	r.mu.Lock()
	r.metas = nil
	r.mu.Unlock()
}

// Prune deletes the batches whose last event is older than before; it is
// the leader's retention job (audit.Pruner).
func (r *Reader) Prune(ctx context.Context, before time.Time, c client.Client) (int64, error) {
	r.forget()
	metas, err := r.list(ctx)
	if err != nil {
		return 0, err
	}
	var n int64
	var errs []error
	for _, m := range metas {
		if !m.until.Before(before) {
			continue
		}
		obj := &auditv1.AuditLog{ObjectMeta: metav1.ObjectMeta{Name: m.name, Namespace: r.Namespace}}
		err := c.Delete(ctx, obj, client.Preconditions{UID: &m.uid})
		switch {
		case err == nil:
			n++
		case !apierrors.IsNotFound(err) && !apierrors.IsConflict(err):
			errs = append(errs, err)
		}
	}
	r.forget()
	return n, errors.Join(errs...)
}

func clampLimit(n int) int {
	switch {
	case n <= 0:
		return 100
	case n > 1000:
		return 1000
	}
	return n
}
