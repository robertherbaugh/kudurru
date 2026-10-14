package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	internalkinds "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// optimisticAttempts is how many times a write is prepared (read, checked
// and validated) outside the write lock before the final attempt, which
// holds the lock from the read to the commit and therefore cannot race.
const optimisticAttempts = 3

// defaultCompactEvery is how many writes pass between change-feed
// compactions.
const defaultCompactEvery = 1024

// Write-path statements, prepared once when the store opens.
const (
	sqlCurrentRV    = `SELECT resource_version FROM objects WHERE kind = ? AND namespace = ? AND name = ?`
	sqlNextRV       = `UPDATE meta SET value = CAST(value AS INTEGER) + 1 WHERE key = ? RETURNING value`
	sqlDeleteObject = `DELETE FROM objects WHERE kind = ? AND namespace = ? AND name = ?`
	sqlUpsertObject = `INSERT INTO objects
		(kind, namespace, name, uid, generation, resource_version, managed_by, created_at, updated_at, spec_hash, labels, deleting, data)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (kind, namespace, name) DO UPDATE SET
			uid = excluded.uid, generation = excluded.generation, resource_version = excluded.resource_version,
			managed_by = excluded.managed_by, created_at = excluded.created_at, updated_at = excluded.updated_at,
			spec_hash = excluded.spec_hash, labels = excluded.labels, deleting = excluded.deleting, data = excluded.data`
	sqlInsertChange = `INSERT INTO changes (seq, kind, namespace, name, type, resource_version, tombstone) VALUES (?, ?, ?, ?, ?, ?, ?)`
)

// writerQueries are prepared on the writer connection by Open.
var writerQueries = []string{sqlCurrentRV, sqlNextRV, sqlDeleteObject, sqlUpsertObject, sqlInsertChange, sqlLastAuditHash, sqlInsertAudit}

// errRaced means the stored row changed between preparing a write and
// committing it; the write is prepared again.
var errRaced = errors.New("store/sqlite: object changed during write")

// stored is one decoded objects row.
type stored struct {
	obj      store.Object
	data     string
	rv       uint64
	specHash string
}

// load reads one object, returning (nil, nil) when it does not exist.
func (s *Store) load(ctx context.Context, q queryer, ki *kindInfo, key store.Key) (*stored, error) {
	var st stored
	var rv int64
	err := q.QueryRowContext(ctx, `SELECT data, resource_version, spec_hash FROM objects WHERE kind = ? AND namespace = ? AND name = ?`,
		string(key.Kind), key.Namespace, key.Name).Scan(&st.data, &rv, &st.specHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store/sqlite: get %s: %w", key, err)
	}
	st.rv = uint64(rv) // #nosec G115 -- resourceVersions are positive
	st.obj = ki.newObject()
	if err := decodeInto(ki, st.data, st.obj); err != nil {
		return nil, err
	}
	return &st, nil
}

// decodeInto replaces *into with the object encoded in data.
func decodeInto(ki *kindInfo, data string, into store.Object) error {
	v := reflect.ValueOf(into)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return fmt.Errorf("store/sqlite: decode %s into a nil object", ki.kind)
	}
	v.Elem().Set(reflect.Zero(v.Elem().Type()))
	if err := json.Unmarshal([]byte(data), into); err != nil {
		return fmt.Errorf("store/sqlite: decode %s: %w", ki.kind, err)
	}
	into.GetObjectKind().SetGroupVersionKind(ki.gvk)
	return nil
}

// Get implements store.Store.
func (s *Store) Get(ctx context.Context, key store.Key, into store.Object) (err error) {
	defer s.observe(opGet, time.Now(), &err)
	if s.isClosed() {
		return store.ErrClosed
	}
	ki, err := s.kinds.get(key.Kind)
	if err != nil {
		return err
	}
	if into == nil {
		return fmt.Errorf("store/sqlite: Get %s into nil", key)
	}
	if got := store.KindOf(into); got != key.Kind {
		return fmt.Errorf("store/sqlite: Get %s into a %s", key, got)
	}
	var data string
	err = s.rq.QueryRowContext(ctx, `SELECT data FROM objects WHERE kind = ? AND namespace = ? AND name = ?`,
		string(key.Kind), key.Namespace, key.Name).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", store.ErrNotFound, key)
	}
	if err != nil {
		return fmt.Errorf("store/sqlite: get %s: %w", key, err)
	}
	return decodeInto(ki, data, into)
}

// continueToken is the decoded form of ObjectList.Continue: the last key of
// the previous page. Pages are ordered by (namespace, name).
type continueToken struct {
	Kind      string `json:"k"`
	Namespace string `json:"ns"`
	Name      string `json:"n"`
}

func encodeContinue(t continueToken) string {
	b, _ := json.Marshal(t)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeContinue(kind store.Kind, s string) (continueToken, error) {
	var t continueToken
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err == nil {
		err = json.Unmarshal(b, &t)
	}
	if err != nil || t.Kind != string(kind) {
		return continueToken{}, fmt.Errorf("store/sqlite: invalid continue token for %s", kind)
	}
	return t, nil
}

// List implements store.Store with keyset pagination: items are ordered by
// (namespace, name) and Continue is an opaque token naming the last key of
// the page, so a page costs O(Limit) regardless of how deep it is. Pages
// are not a consistent snapshot: objects written between pages appear or
// not depending on their key. Label selectors are equality matches on all
// given pairs.
func (s *Store) List(ctx context.Context, kind store.Kind, opts store.ListOptions) (_ store.ObjectList, err error) {
	defer s.observe(opList, time.Now(), &err)
	if s.isClosed() {
		return store.ObjectList{}, store.ErrClosed
	}
	ki, err := s.kinds.get(kind)
	if err != nil {
		return store.ObjectList{}, err
	}
	var b strings.Builder
	args := []any{string(kind)}
	b.WriteString(`SELECT namespace, name, data FROM objects WHERE kind = ?`)
	if opts.Namespace != "" {
		b.WriteString(` AND namespace = ?`)
		args = append(args, opts.Namespace)
	}
	if opts.Continue != "" {
		t, err := decodeContinue(kind, opts.Continue)
		if err != nil {
			return store.ObjectList{}, err
		}
		b.WriteString(` AND (namespace, name) > (?, ?)`)
		args = append(args, t.Namespace, t.Name)
	}
	keys := make([]string, 0, len(opts.LabelSelector))
	for k := range opts.LabelSelector {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(` AND EXISTS (SELECT 1 FROM json_each(objects.labels) AS l WHERE l.key = ? AND l.value = ?)`)
		args = append(args, k, opts.LabelSelector[k])
	}
	b.WriteString(` ORDER BY namespace, name`)
	if opts.Limit > 0 {
		b.WriteString(` LIMIT ?`)
		args = append(args, opts.Limit+1)
	}
	rows, err := s.rq.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return store.ObjectList{}, fmt.Errorf("store/sqlite: list %s: %w", kind, err)
	}
	defer func() { _ = rows.Close() }()
	out := store.ObjectList{Items: []store.Object{}}
	var lastNS, lastName string
	for rows.Next() {
		var ns, name, data string
		if err := rows.Scan(&ns, &name, &data); err != nil {
			return store.ObjectList{}, fmt.Errorf("store/sqlite: list %s: %w", kind, err)
		}
		if opts.Limit > 0 && len(out.Items) == opts.Limit {
			out.Continue = encodeContinue(continueToken{Kind: string(kind), Namespace: lastNS, Name: lastName})
			break
		}
		o := ki.newObject()
		if err := decodeInto(ki, data, o); err != nil {
			return store.ObjectList{}, err
		}
		out.Items = append(out.Items, o)
		lastNS, lastName = ns, name
	}
	if err := rows.Err(); err != nil {
		return store.ObjectList{}, fmt.Errorf("store/sqlite: list %s: %w", kind, err)
	}
	return out, nil
}

// mutation is a prepared write: what to store (or delete), what it
// replaces, and how to audit it.
type mutation struct {
	ki      *kindInfo
	key     store.Key
	typ     store.EventType // Added, Modified or Deleted
	obj     store.Object    // the new object, or the tombstone for Deleted
	prev    *stored         // nil on create
	manager string
	action  string // audit verb: create, update, status, delete, finalize
	reason  string
	noop    bool // nothing changed; result is prev
}

// mutate prepares and commits a write. plan reads the current row and
// returns the mutation; it runs outside the write lock first and is
// repeated when the row changed before the commit. With strict set (the
// caller passed a resourceVersion precondition) such a race is
// store.ErrConflict instead.
func (s *Store) mutate(ctx context.Context, strict bool, plan func(q queryer) (*mutation, error)) (store.Object, error) {
	var err error
	ctx, err = s.writeContext(ctx)
	if err != nil {
		return nil, err
	}
	for attempt := 1; ; attempt++ {
		locked := attempt > optimisticAttempts
		if locked {
			s.wmu.Lock()
		}
		m, err := plan(s.rq)
		if err != nil || m.noop {
			if err == nil {
				if !locked {
					s.wmu.Lock()
				}
				_, err = s.checkWriter(ctx, s.rq)
				if !locked {
					s.wmu.Unlock()
				}
			}
			if locked {
				s.wmu.Unlock()
			}
			if err != nil {
				return nil, err
			}
			return m.prev.obj, nil
		}
		if !locked {
			s.wmu.Lock()
		}
		out, err := s.commitLocked(ctx, m)
		s.wmu.Unlock()
		if errors.Is(err, errRaced) {
			if strict || locked {
				return nil, fmt.Errorf("%w: %s changed during write", store.ErrConflict, m.key)
			}
			continue
		}
		return out, err
	}
}

// commitLocked writes m in one transaction: precondition check, the new
// resourceVersion, the objects row, the change-feed row and the audit row.
// It then publishes the change. The caller holds wmu.
func (s *Store) commitLocked(ctx context.Context, m *mutation) (store.Object, error) {
	out, err := s.commitAllLocked(ctx, []*mutation{m})
	if err != nil {
		return nil, err
	}
	return out[0], nil
}

// commitAllLocked writes every mutation of ms in one transaction (each
// with its own resourceVersion, change-feed row and audit row, in order)
// and publishes the changes after the commit: all of them or none. The
// caller holds wmu.
func (s *Store) commitAllLocked(ctx context.Context, ms []*mutation) ([]store.Object, error) {
	if s.isClosed() {
		return nil, store.ErrClosed
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store/sqlite: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := cachedTx{tx: tx, c: s.ws}
	a, err := s.checkWriter(ctx, tx)
	if err != nil {
		return nil, err
	}

	rvs := make([]uint64, len(ms))
	for i, m := range ms {
		if rvs[i], err = s.writeMutation(ctx, q, m); err != nil {
			return nil, err
		}
	}
	rv := rvs[len(rvs)-1]
	if err := advanceRevision(ctx, tx, &a); err != nil {
		return nil, err
	}

	compactTo := uint64(0)
	s.sinceCompacted += len(ms)
	if s.sinceCompacted >= s.compactEvery {
		retention := uint64(s.opts.FeedRetention) // #nosec G115 -- positive by setDefaults
		if rv > retention && rv-retention > s.compacted.Load() {
			compactTo = rv - retention
			if err := compactFeed(ctx, q, compactTo); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store/sqlite: commit %s: %w", ms[0].key, err)
	}
	if compactTo > 0 {
		s.compacted.Store(compactTo)
		s.sinceCompacted = 0
	}
	s.head.Store(rv)
	s.publishAuthorityLocked(a)
	s.publishGenerationLocked()
	out := make([]store.Object, len(ms))
	for i, m := range ms {
		s.bus.publish(change{seq: rvs[i], typ: m.typ, key: m.key, obj: m.obj})
		out[i] = m.obj.DeepCopyObject().(store.Object)
	}
	return out, nil
}

// writeMutation writes one mutation inside the transaction q: the
// precondition check, the new resourceVersion (returned), the objects row,
// the change-feed row and the audit row.
func (s *Store) writeMutation(ctx context.Context, q cachedTx, m *mutation) (uint64, error) {
	var curRV int64
	err := q.QueryRowContext(ctx, sqlCurrentRV,
		string(m.key.Kind), m.key.Namespace, m.key.Name).Scan(&curRV)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if m.prev != nil {
			return 0, errRaced
		}
	case err != nil:
		return 0, fmt.Errorf("store/sqlite: read %s: %w", m.key, err)
	default:
		if m.prev == nil || uint64(curRV) != m.prev.rv { // #nosec G115 -- resourceVersions are positive
			return 0, errRaced
		}
	}

	var rvText string
	if err := q.QueryRowContext(ctx, sqlNextRV, metaResourceVersion).Scan(&rvText); err != nil {
		return 0, fmt.Errorf("store/sqlite: next resourceVersion: %w", err)
	}
	rv, err := strconv.ParseUint(rvText, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("store/sqlite: bad resourceVersion %q: %w", rvText, err)
	}
	m.obj.SetResourceVersion(rvText)
	data, err := json.Marshal(m.obj)
	if err != nil {
		return 0, fmt.Errorf("store/sqlite: encode %s: %w", m.key, err)
	}
	if m.typ != store.Deleted && len(data) > store.MaxObjectBytes {
		return 0, fmt.Errorf("%w: %s encodes to %d bytes; the limit is %d (the etcd request cap)",
			store.ErrTooLarge, m.key, len(data), store.MaxObjectBytes)
	}
	now := s.opts.Now()

	var tombstone any
	if m.typ == store.Deleted {
		if _, err := q.ExecContext(ctx, sqlDeleteObject,
			string(m.key.Kind), m.key.Namespace, m.key.Name); err != nil {
			return 0, fmt.Errorf("store/sqlite: delete %s: %w", m.key, err)
		}
		tombstone = string(data)
	} else {
		labels := "{}"
		if l := m.obj.GetLabels(); len(l) > 0 {
			b, err := json.Marshal(l)
			if err != nil {
				return 0, err
			}
			labels = string(b)
		}
		deleting := 0
		if m.obj.GetDeletionTimestamp() != nil {
			deleting = 1
		}
		ct := m.obj.GetCreationTimestamp()
		if _, err := q.ExecContext(ctx, sqlUpsertObject,
			string(m.key.Kind), m.key.Namespace, m.key.Name, string(m.obj.GetUID()), m.obj.GetGeneration(), int64(rv), // #nosec G115 -- bounded by SQLite's int64 counter
			store.ManagedBy(m.obj), ct.UTC().Format(time.RFC3339), now.UnixNano(), specHash(m.obj), labels, deleting, string(data)); err != nil {
			return 0, fmt.Errorf("store/sqlite: write %s: %w", m.key, err)
		}
	}
	if _, err := q.ExecContext(ctx, sqlInsertChange,
		int64(rv), string(m.key.Kind), m.key.Namespace, m.key.Name, string(m.typ), rvText, tombstone); err != nil { // #nosec G115 -- see above
		return 0, fmt.Errorf("store/sqlite: change feed %s: %w", m.key, err)
	}

	entry := AuditEntry{
		Time:            now,
		Actor:           s.opts.ActorFromContext(ctx),
		Manager:         m.manager,
		Action:          strings.ToLower(string(m.key.Kind)) + "." + m.action,
		Key:             m.key,
		ResourceVersion: rvText,
		Result:          AuditSuccess,
		Reason:          m.reason,
	}
	if m.prev != nil {
		entry.Before = json.RawMessage(m.prev.data)
	}
	if m.typ != store.Deleted {
		entry.After = json.RawMessage(data)
	}
	if m.key.Kind == internalkinds.KindAPIToken {
		entry.Before, entry.After = tokenAuditDocument(entry.Before), tokenAuditDocument(entry.After)
	} else if internalkinds.IsInternal(string(m.key.Kind)) {
		entry.Before, entry.After = nil, nil
	}
	if err := insertAudit(ctx, q, &entry); err != nil {
		return 0, err
	}
	return rv, nil
}

func tokenAuditDocument(data json.RawMessage) json.RawMessage {
	if data == nil {
		return nil
	}
	var tok internalkinds.APIToken
	if err := json.Unmarshal(data, &tok); err != nil {
		return nil
	}
	return tok.AuditDocument()
}

// compactFeed drops change-feed rows at or below seq.
func compactFeed(ctx context.Context, tx txQueryer, seq uint64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM changes WHERE seq <= ?`, int64(seq)); err != nil { // #nosec G115 -- bounded by SQLite's int64 counter
		return fmt.Errorf("store/sqlite: compact change feed: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE meta SET value = ? WHERE key = ?`, strconv.FormatUint(seq, 10), metaFeedCompacted); err != nil {
		return fmt.Errorf("store/sqlite: compact change feed: %w", err)
	}
	return nil
}

// Apply implements store.Store with the Apply contract of §4.3 (D9, D10,
// D70):
//
//   - Manager is required and recorded in the kudurru.io/managed-by
//     annotation; an object recorded for another manager is
//     *store.ErrManagedBy unless Force (a whole-object takeover, audited
//     with the previous owner).
//   - ExpectedResourceVersion that does not match the stored object, or
//     names an object that does not exist, is store.ErrConflict.
//   - The object is validated and defaulted by the validator before
//     anything is written; a rejection is a *validator.Error.
//   - status is taken from the stored object (empty on create); generation
//     is 1 on create and moves only when the spec changes; uid and
//     creationTimestamp are kept from the stored object, or taken from the
//     caller on create and generated only when absent; an identical
//     re-apply writes nothing and returns the stored object.
//   - An object being deleted (deletionTimestamp set) whose finalizers the
//     apply clears is removed.
//
// obj is not modified.
func (s *Store) Apply(ctx context.Context, obj store.Object, opts store.ApplyOptions) (_ store.Object, err error) {
	defer s.observe(opApply, time.Now(), &err)
	if opts.Manager == "" {
		return nil, errors.New("store: ApplyOptions.Manager is required")
	}
	if obj == nil || reflect.ValueOf(obj).IsNil() {
		return nil, errors.New("store/sqlite: Apply of a nil object")
	}
	if s.isClosed() {
		return nil, store.ErrClosed
	}
	ki, err := s.kinds.get(store.KindOf(obj))
	if err != nil {
		return nil, err
	}
	key := store.KeyOf(obj)
	if key.Name == "" {
		return nil, fmt.Errorf("store/sqlite: Apply %s: metadata.name is required", key.Kind)
	}
	return s.mutate(ctx, opts.ExpectedResourceVersion != "", func(q queryer) (*mutation, error) {
		prev, err := s.load(ctx, q, ki, key)
		if err != nil {
			return nil, err
		}
		return s.planApply(ctx, ki, key, obj, prev, opts)
	})
}

func (s *Store) planApply(ctx context.Context, ki *kindInfo, key store.Key, obj store.Object, prev *stored, opts store.ApplyOptions) (*mutation, error) {
	m := &mutation{ki: ki, key: key, prev: prev, manager: opts.Manager}
	next := obj.DeepCopyObject().(store.Object)
	next.GetObjectKind().SetGroupVersionKind(ki.gvk)
	next.SetManagedFields(nil)
	next.SetResourceVersion("")
	if prev == nil {
		if opts.ExpectedResourceVersion != "" {
			return nil, fmt.Errorf("%w: %s does not exist, expected resourceVersion %s", store.ErrConflict, key, opts.ExpectedResourceVersion)
		}
		if next.GetUID() == "" {
			next.SetUID(types.UID(uuid.NewString()))
		}
		if ct := next.GetCreationTimestamp(); ct.IsZero() {
			next.SetCreationTimestamp(storedTime(s.opts.Now()))
		} else {
			next.SetCreationTimestamp(storedTime(ct.Time))
		}
		next.SetGeneration(1)
		next.SetDeletionTimestamp(nil)
		next.SetDeletionGracePeriodSeconds(nil)
		m.typ, m.action = store.Added, "create"
	} else {
		p := prev.obj
		if opts.ExpectedResourceVersion != "" && opts.ExpectedResourceVersion != p.GetResourceVersion() {
			return nil, fmt.Errorf("%w: %s is at %s, expected %s", store.ErrConflict, key, p.GetResourceVersion(), opts.ExpectedResourceVersion)
		}
		owner := store.ManagedBy(p)
		if owner != "" && owner != opts.Manager {
			if !opts.Force {
				return nil, &store.ErrManagedBy{Key: key, Owner: owner}
			}
			m.reason = fmt.Sprintf("force: took over from %q", owner)
		}
		next.SetUID(p.GetUID())
		next.SetCreationTimestamp(p.GetCreationTimestamp())
		next.SetGeneration(p.GetGeneration())
		next.SetDeletionTimestamp(p.GetDeletionTimestamp())
		next.SetDeletionGracePeriodSeconds(p.GetDeletionGracePeriodSeconds())
		m.typ, m.action = store.Modified, "update"
	}
	store.SetManagedBy(next, opts.Manager)

	var old store.Object
	if prev != nil {
		old = prev.obj
	}
	if err := s.validate(ctx, next, old); err != nil {
		return nil, err
	}
	// Apply writes the main resource only (D70).
	if prev != nil {
		if err := copyStatus(next, prev.obj.DeepCopyObject().(store.Object)); err != nil {
			return nil, err
		}
		if specHash(next) != prev.specHash {
			next.SetGeneration(prev.obj.GetGeneration() + 1)
		}
		if same, err := identical(next, prev); err != nil {
			return nil, err
		} else if same {
			m.noop = true
			return m, nil
		}
		if next.GetDeletionTimestamp() != nil && len(next.GetFinalizers()) == 0 {
			m.typ, m.action = store.Deleted, "finalize"
		}
	} else if err := copyStatus(next, ki.newObject()); err != nil {
		return nil, err
	}
	m.obj = next
	return m, nil
}

// identical reports whether next, stamped with the stored resourceVersion,
// encodes to exactly the stored document.
func identical(next store.Object, prev *stored) (bool, error) {
	next.SetResourceVersion(prev.obj.GetResourceVersion())
	defer next.SetResourceVersion("")
	b, err := json.Marshal(next)
	if err != nil {
		return false, err
	}
	return string(b) == prev.data, nil
}

// Delete implements store.Store. A non-empty Manager must match the
// recorded managedBy unless Force (an empty Manager is unchecked, as in the
// other drivers). An object with finalizers is not removed: its
// deletionTimestamp is set (a Modified event) and it is removed by the
// Apply that clears the last finalizer. UpdateStatus cannot clear them: it
// writes status only, like the apiserver's status subresource (and
// validator.ValidateStatus takes metadata from the stored object). Deleting
// an object that is already being deleted is a no-op.
func (s *Store) Delete(ctx context.Context, key store.Key, opts store.DeleteOptions) (err error) {
	defer s.observe(opDelete, time.Now(), &err)
	if s.isClosed() {
		return store.ErrClosed
	}
	ki, err := s.kinds.get(key.Kind)
	if err != nil {
		return err
	}
	_, err = s.mutate(ctx, false, func(q queryer) (*mutation, error) {
		prev, err := s.load(ctx, q, ki, key)
		if err != nil {
			return nil, err
		}
		if prev == nil {
			return nil, fmt.Errorf("%w: %s", store.ErrNotFound, key)
		}
		return s.planDelete(ki, key, prev, opts)
	})
	return err
}

// planDelete prepares the delete of the stored object prev: the managedBy
// check, then a tombstone, or a deletionTimestamp while finalizers remain.
func (s *Store) planDelete(ki *kindInfo, key store.Key, prev *stored, opts store.DeleteOptions) (*mutation, error) {
	manager := opts.Manager
	m := &mutation{ki: ki, key: key, prev: prev, manager: manager, action: "delete"}
	if owner := store.ManagedBy(prev.obj); owner != "" && manager != "" && owner != manager {
		if !opts.Force {
			return nil, &store.ErrManagedBy{Key: key, Owner: owner}
		}
		m.reason = fmt.Sprintf("force: owner was %q", owner)
	}
	if m.manager == "" {
		m.manager = store.ManagedBy(prev.obj)
	}
	next := prev.obj.DeepCopyObject().(store.Object)
	if len(next.GetFinalizers()) > 0 {
		if next.GetDeletionTimestamp() != nil {
			m.noop = true
			return m, nil
		}
		// Second precision, as stored: the Modified event then carries
		// exactly what Get returns.
		now := storedTime(s.opts.Now())
		next.SetDeletionTimestamp(&now)
		m.typ, m.obj = store.Modified, next
		m.reason = strings.TrimPrefix(m.reason+"; waiting for finalizers", "; ")
		return m, nil
	}
	m.typ, m.obj = store.Deleted, next
	return m, nil
}

// DeleteMany implements store.MultiDeleter: every request is planned like
// Delete while the write lock is held and the deletes are committed in one
// transaction (one change-feed row and audit row each), so all of them
// happen or none. Objects that do not exist are skipped.
func (s *Store) DeleteMany(ctx context.Context, reqs []store.DeleteRequest) (err error) {
	defer s.observe(opDelete, time.Now(), &err)
	if s.isClosed() {
		return store.ErrClosed
	}
	ctx, err = s.writeContext(ctx)
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	var ms []*mutation
	seen := map[store.Key]bool{}
	for _, r := range reqs {
		if seen[r.Key] {
			continue
		}
		seen[r.Key] = true
		ki, err := s.kinds.get(r.Key.Kind)
		if err != nil {
			return err
		}
		prev, err := s.load(ctx, s.rq, ki, r.Key)
		if err != nil {
			return err
		}
		if prev == nil {
			continue
		}
		m, err := s.planDelete(ki, r.Key, prev, r.Options)
		if err != nil {
			return err
		}
		if !m.noop {
			ms = append(ms, m)
		}
	}
	if len(ms) == 0 {
		_, err := s.checkWriter(ctx, s.rq)
		return err
	}
	if _, err := s.commitAllLocked(ctx, ms); err != nil {
		if errors.Is(err, errRaced) {
			// Cannot happen while wmu is held; never retried silently.
			return fmt.Errorf("%w: an object changed during the delete", store.ErrConflict)
		}
		return err
	}
	return nil
}

// UpdateStatus implements store.Store like the status subresource: status
// is validated (validator.ValidateStatus) and written; metadata and spec
// are taken from the stored object, and generation does not move. If obj
// carries a resourceVersion it is a precondition (mismatch:
// store.ErrConflict). An unchanged status writes nothing.
func (s *Store) UpdateStatus(ctx context.Context, obj store.Object) (err error) {
	defer s.observe(opUpdateStatus, time.Now(), &err)
	if obj == nil || reflect.ValueOf(obj).IsNil() {
		return errors.New("store/sqlite: UpdateStatus of a nil object")
	}
	if s.isClosed() {
		return store.ErrClosed
	}
	ki, err := s.kinds.get(store.KindOf(obj))
	if err != nil {
		return err
	}
	key := store.KeyOf(obj)
	callerRV := obj.GetResourceVersion()
	_, err = s.mutate(ctx, callerRV != "", func(q queryer) (*mutation, error) {
		prev, err := s.load(ctx, q, ki, key)
		if err != nil {
			return nil, err
		}
		if prev == nil {
			return nil, fmt.Errorf("%w: %s", store.ErrNotFound, key)
		}
		if callerRV != "" && callerRV != prev.obj.GetResourceVersion() {
			return nil, fmt.Errorf("%w: %s is at %s, expected %s", store.ErrConflict, key, prev.obj.GetResourceVersion(), callerRV)
		}
		in := obj.DeepCopyObject().(store.Object)
		in.GetObjectKind().SetGroupVersionKind(ki.gvk)
		if err := s.validateStatus(ctx, in, prev.obj); err != nil {
			return nil, err
		}
		next := prev.obj.DeepCopyObject().(store.Object)
		if err := copyStatus(next, in); err != nil {
			return nil, err
		}
		m := &mutation{ki: ki, key: key, prev: prev, manager: store.ManagedBy(prev.obj), typ: store.Modified, obj: next, action: "status"}
		if same, err := identical(next, prev); err != nil {
			return nil, err
		} else if same {
			m.noop = true
		}
		return m, nil
	})
	return err
}

// storedTime is t as a stored document decodes it: metav1.Time encodes
// whole seconds and decodes in the local zone. Timestamps the driver stamps
// go through it so returned objects and watch events equal what Get reads.
func storedTime(t time.Time) metav1.Time {
	return metav1.NewTime(time.Unix(t.Unix(), 0))
}

// copyStatus sets the Status field of dst to that of src.
func copyStatus(dst, src store.Object) error {
	d := reflect.ValueOf(dst).Elem().FieldByName("Status")
	sv := reflect.ValueOf(src).Elem().FieldByName("Status")
	if !d.IsValid() || !sv.IsValid() || !d.CanSet() {
		return nil // kinds without status have nothing to carry
	}
	d.Set(sv)
	return nil
}

// specHash is the hex SHA-256 of the JSON encoding of the Spec field
// (encoding/json sorts map keys, so equal specs hash equally).
func specHash(obj store.Object) string {
	var v any
	if f := reflect.ValueOf(obj).Elem().FieldByName("Spec"); f.IsValid() {
		v = f.Interface()
	}
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
