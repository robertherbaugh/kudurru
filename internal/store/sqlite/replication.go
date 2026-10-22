package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func (s *Store) replicationValidation(ctx context.Context) replication.Validation {
	kinds := make([]replication.Kind, 0, len(s.kinds.kinds))
	for _, k := range s.kinds.kinds {
		ki := s.kinds.byKind[k]
		kinds = append(kinds, replication.Kind{APIVersion: ki.gvk.GroupVersion().String(), Kind: string(k)})
	}
	return replication.Validation{SchemaVersion: store.SchemaVersion, Kinds: kinds, Limits: s.opts.ReplicationLimits, ValidateImage: s.opts.ValidateReplicaImage, ValidateEntry: s.opts.ValidateReplicaEntry, ValidateObject: func(raw json.RawMessage) error {
		obj, err := s.replicaObject(raw)
		if err != nil {
			return err
		}
		ct := obj.GetCreationTimestamp()
		if obj.GetUID() == "" || obj.GetGeneration() <= 0 || ct.IsZero() {
			return fmt.Errorf("%w: source object metadata", replication.ErrInvalid)
		}
		// Validation/defaulting runs on a copy, never the source document.
		check := obj.DeepCopyObject().(store.Object)
		if err := s.validate(ctx, check, nil); err != nil {
			return err
		}
		// Main validation covers the entire document for kinds without a
		// status subresource, including self-validating private runtime kinds.
		if s.opts.Validator.HasStatusSubresource(store.KindOf(obj)) {
			if err := s.validateStatus(ctx, obj.DeepCopyObject().(store.Object), obj.DeepCopyObject().(store.Object)); err != nil {
				return err
			}
		}
		if s.opts.ValidateReplicaObject != nil {
			return s.opts.ValidateReplicaObject(append(json.RawMessage(nil), raw...))
		}
		return nil
	}}
}

func (s *Store) replicaObject(raw json.RawMessage) (store.Object, error) {
	var h struct {
		Kind       string `json:"kind"`
		APIVersion string `json:"apiVersion"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return nil, replication.ErrInvalid
	}
	ki, err := s.kinds.get(store.Kind(h.Kind))
	if err != nil {
		return nil, replication.ErrIncompatible
	}
	obj := ki.newObject()
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(obj); err != nil {
		return nil, fmt.Errorf("%w: object document", replication.ErrInvalid)
	}
	return obj, nil
}

// ValidateImage checks a complete image against the local registry and owners.
func (s *Store) ValidateImage(ctx context.Context, image replication.Image) (*replication.Validated, error) {
	return replication.Validate(image, s.replicationValidation(ctx))
}

// ReadGeneration takes one coherent database cut across objects, private
// sections, source revision and authority. It is NOT a backup Snapshot.
func (s *Store) ReadGeneration(ctx context.Context) (*replication.Image, error) {
	if s.isClosed() {
		return nil, store.ErrClosed
	}
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	return s.readGeneration(ctx, tx)
}

func (s *Store) readGeneration(ctx context.Context, tx *sql.Tx) (*replication.Image, error) {
	l := s.opts.ReplicationLimits.Defaults()
	var objects, entries, size int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(length(CAST(data AS BLOB))),0) FROM objects`).Scan(&objects, &size); err != nil {
		return nil, err
	}
	if objects > int64(l.MaxObjects) || size > l.MaxBytes {
		return nil, replication.ErrTooLarge
	}
	var sectionBytes int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(length(CAST(data AS BLOB))+length(CAST(key AS BLOB))),0) FROM replication_sections`).Scan(&entries, &sectionBytes); err != nil {
		return nil, err
	}
	if entries > int64(l.MaxEntries) || sectionBytes > l.MaxBytes-size {
		return nil, replication.ErrTooLarge
	}
	a, err := readAuthority(ctx, tx)
	if err != nil {
		return nil, err
	}
	if a.Installed.Term.ClusterID == "" {
		return nil, fmt.Errorf("%w: initial image not installed", replication.ErrReadOnly)
	}
	var rv string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, metaResourceVersion).Scan(&rv); err != nil {
		return nil, err
	}
	head, err := strconv.ParseUint(rv, 10, 63)
	if err != nil {
		return nil, err
	}
	i := &replication.Image{StoreSchemaVersion: store.SchemaVersion, Term: a.Installed.Term, Revision: a.Installed.Revision, ObjectResourceVersion: head, Parent: a.Parent, CreatedAt: s.opts.Now().UTC()}
	for _, k := range s.kinds.kinds {
		ki := s.kinds.byKind[k]
		i.KindManifest = append(i.KindManifest, replication.Kind{APIVersion: ki.gvk.GroupVersion().String(), Kind: string(k)})
	}
	rows, err := tx.QueryContext(ctx, `SELECT data FROM objects ORDER BY kind,namespace,name`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			_ = rows.Close()
			return nil, err
		}
		i.Payload.Objects = append(i.Payload.Objects, json.RawMessage(raw))
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT section,key,data FROM replication_sections ORDER BY section,key`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var name, key, raw string
		if err := rows.Scan(&name, &key, &raw); err != nil {
			_ = rows.Close()
			return nil, err
		}
		section := replication.Section(&i.Payload, name)
		if section == nil {
			_ = rows.Close()
			return nil, replication.ErrIncompatible
		}
		*section = append(*section, replication.Entry{Key: key, Data: json.RawMessage(raw)})
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	if err := replication.Seal(i); err != nil {
		return nil, err
	}
	b, err := json.Marshal(i)
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > l.MaxBytes {
		return nil, replication.ErrTooLarge
	}
	return i, nil
}

// GenerationChanges is a coalesced notification: subscribe BEFORE reading an
// image, then re-read on closure. No per-kind stream is a transaction barrier.
func (s *Store) GenerationChanges() <-chan struct{} {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return s.generationChanged
}

func advanceRevision(ctx context.Context, tx *sql.Tx, a *replication.Authority) error {
	if a.Installed.Revision == math.MaxUint64 {
		return replication.ErrInvalid
	}
	a.Installed = replication.Generation{Term: a.Term, Revision: a.Installed.Revision + 1}
	return writeAuthority(ctx, tx, *a)
}

func installedID(ctx context.Context, s *Store, tx *sql.Tx, a replication.Authority) (replication.Generation, error) {
	if a.Installed.Term.ClusterID == "" {
		return a.Installed, nil
	}
	i, err := s.readGeneration(ctx, tx)
	if err != nil {
		return replication.Generation{}, err
	}
	return i.Generation(), nil
}

// InstallReplica replaces the canonical replicated set, including absence
// deletions. It never invokes Apply/finalizers or rewrites source status/RVs.
// Caller authenticates the source and observes its authority before install.
func (s *Store) InstallReplica(ctx context.Context, validated *replication.Validated, opts replication.InstallOptions) (replication.Generation, error) {
	if validated == nil {
		return replication.Generation{}, replication.ErrInvalid
	}
	image := validated.Image()
	// Revalidate against THIS store's complete scheme/section validators.
	if _, err := s.ValidateImage(ctx, image); err != nil {
		return replication.Generation{}, err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.isClosed() {
		return replication.Generation{}, store.ErrClosed
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return replication.Generation{}, err
	}
	defer func() { _ = tx.Rollback() }()
	a, err := readAuthority(ctx, tx)
	if err != nil {
		return replication.Generation{}, err
	}
	if image.Term.ClusterID != a.Term.ClusterID {
		return replication.Generation{}, replication.ErrWrongCluster
	}
	if a.Mode != replication.Follower && a.Mode != replication.Fenced {
		return replication.Generation{}, replication.ErrReadOnly
	}
	if s.writeBlocked.Load() || a.Conflict != nil || a.Divergence != nil {
		return replication.Generation{}, replication.ErrFenced
	}
	if !image.Term.Same(a.Term) || !opts.ExpectedTerm.Same(a.Term) {
		return replication.Generation{}, replication.ErrCAS
	}
	current, err := installedID(ctx, s, tx, a)
	if err != nil {
		return replication.Generation{}, err
	}
	if !current.Same(opts.ExpectedInstalled) {
		return replication.Generation{}, replication.ErrCAS
	}
	if current.Term.Same(image.Term) {
		if image.Revision < current.Revision || image.ObjectResourceVersion < s.head.Load() {
			return replication.Generation{}, replication.ErrStale
		}
		if !sameDivergence(a.Parent, image.Parent) {
			return replication.Generation{}, replication.ErrInvalid
		}
		if image.Revision == current.Revision {
			if current.Digest != image.PayloadSHA256 {
				s.writeBlocked.Store(true)
				a.Mode, a.Divergence = replication.Fenced, new(replication.Generation)
				*a.Divergence = image.Generation()
				if a.Incarnation < math.MaxUint64 {
					a.Incarnation++
				}
				err := writeAuthority(ctx, tx, a)
				if err == nil {
					err = tx.Commit()
				}
				s.publishAuthorityLocked(a)
				if err != nil {
					return replication.Generation{}, err
				}
				s.writeBlocked.Store(false)
				return replication.Generation{}, replication.ErrDivergent
			}
			return current, nil
		}
	}
	if a.InstallSequence == math.MaxUint64 || a.Incarnation == math.MaxUint64 {
		return replication.Generation{}, replication.ErrInvalid
	}
	if current.Term.ClusterID != "" && !current.Term.Same(image.Term) {
		if err := s.quarantine(ctx, tx); err != nil {
			return replication.Generation{}, err
		}
	}
	if err := s.replaceReplicaRows(ctx, tx, image); err != nil {
		return replication.Generation{}, err
	}
	a.Installed = image.Generation()
	a.Parent = image.Parent
	a.Mode = replication.Follower
	a.InstallSequence++
	a.Incarnation++
	a.WatchLineage = uuid.NewString()
	for _, key := range []string{metaResourceVersion, metaFeedCompacted} {
		if _, err := tx.ExecContext(ctx, `UPDATE meta SET value=? WHERE key=?`, strconv.FormatUint(image.ObjectResourceVersion, 10), key); err != nil {
			return replication.Generation{}, err
		}
	}
	if err := writeAuthority(ctx, tx, a); err != nil {
		return replication.Generation{}, err
	}
	if err := tx.Commit(); err != nil {
		return replication.Generation{}, err
	}
	s.head.Store(image.ObjectResourceVersion)
	s.compacted.Store(image.ObjectResourceVersion)
	s.sinceCompacted = 0
	s.bus.reset()
	s.publishAuthorityLocked(a)
	s.publishGenerationLocked()
	return a.Installed, nil
}

// replaceReplicaRows is the single ownership transaction lane used by install
// and explicit branch resolution. The caller validates and holds wmu.
func (s *Store) replaceReplicaRows(ctx context.Context, tx *sql.Tx, image replication.Image) error {
	for _, q := range []string{`DELETE FROM objects`, `DELETE FROM changes`, `DELETE FROM replication_sections`} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	for _, raw := range image.Payload.Objects {
		obj, err := s.replicaObject(raw)
		if err != nil {
			return err
		}
		labels := "{}"
		if len(obj.GetLabels()) > 0 {
			b, err := json.Marshal(obj.GetLabels())
			if err != nil {
				return err
			}
			labels = string(b)
		}
		rv, err := strconv.ParseInt(obj.GetResourceVersion(), 10, 64)
		if err != nil {
			return err
		}
		deleting := 0
		if obj.GetDeletionTimestamp() != nil {
			deleting = 1
		}
		ct := obj.GetCreationTimestamp()
		if _, err := tx.ExecContext(ctx, sqlUpsertObject, string(store.KindOf(obj)), obj.GetNamespace(), obj.GetName(), string(obj.GetUID()), obj.GetGeneration(), rv, store.ManagedBy(obj), ct.UTC().Format(time.RFC3339), s.opts.Now().UnixNano(), specHash(obj), labels, deleting, string(raw)); err != nil {
			return err
		}
	}
	for _, name := range replication.SectionNames() {
		for _, entry := range *replication.Section(&image.Payload, name) {
			if _, err := tx.ExecContext(ctx, `INSERT INTO replication_sections(section,key,data) VALUES (?,?,?)`, name, entry.Key, string(entry.Data)); err != nil {
				return err
			}
		}
	}
	return nil
}

// quarantine retains a complete local losing branch in the same transaction
// that discards it. It is node-local, private and absent from public backups
// and replicated images. Recovery/export needs operator authorization above.
func (s *Store) quarantine(ctx context.Context, tx *sql.Tx) error {
	i, err := s.readGeneration(ctx, tx)
	if err != nil {
		return err
	}
	b, err := json.Marshal(i)
	if err != nil {
		return err
	}
	if int64(len(b)) > s.opts.ReplicationLimits.Defaults().MaxBytes {
		return replication.ErrTooLarge
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO replication_quarantine(generation,image) VALUES (?,?) ON CONFLICT(generation) DO NOTHING`, i.Generation().ETag(), string(b))
	return err
}

// SectionChange belongs to an explicit registered section. Settings values are
// intentionally excluded: the future Kudurru singleton is their sole owner.
type SectionChange struct {
	Section string
	Key     string
	Data    json.RawMessage
	Delete  bool
}

// ApplyCluster atomically applies main objects and extension rows. New IAM
// domain commands can build on this transaction without adding duplicate
// settings authority. Secret/security validators remain their owners' concern.
func (s *Store) ApplyCluster(ctx context.Context, objects []store.Object, opts store.ApplyOptions, sections []SectionChange) ([]store.Object, error) {
	ctx, err := s.writeContext(ctx)
	if err != nil {
		return nil, err
	}
	l := s.opts.ReplicationLimits.Defaults()
	if len(objects) > l.MaxObjects || len(sections) > l.MaxEntries {
		return nil, replication.ErrTooLarge
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	a, err := s.checkWriter(ctx, tx)
	if err != nil {
		return nil, err
	}
	q := cachedTx{tx: tx, c: s.ws}
	var ms []*mutation
	var rvs []uint64
	var out []store.Object
	seenObjects := map[store.Key]bool{}
	for _, obj := range objects {
		if obj == nil || reflect.ValueOf(obj).IsNil() || opts.Manager == "" {
			return nil, replication.ErrInvalid
		}
		ki, err := s.kinds.get(store.KindOf(obj))
		if err != nil {
			return nil, err
		}
		key := store.KeyOf(obj)
		if key.Name == "" || seenObjects[key] {
			return nil, replication.ErrInvalid
		}
		seenObjects[key] = true
		prev, err := s.load(ctx, tx, ki, key)
		if err != nil {
			return nil, err
		}
		m, err := s.planApply(ctx, ki, key, obj, prev, opts)
		if err != nil {
			return nil, err
		}
		if m.noop {
			out = append(out, prev.obj.DeepCopyObject().(store.Object))
			continue
		}
		rv, err := s.writeMutation(ctx, q, m)
		if err != nil {
			return nil, err
		}
		rvs = append(rvs, rv)
		ms = append(ms, m)
		out = append(out, m.obj.DeepCopyObject().(store.Object))
	}
	changed := len(ms) > 0
	seenSections := map[string]map[string]bool{}
	for _, c := range sections {
		if replication.Section(&replication.Payload{}, c.Section) == nil || c.Key == "" || len(c.Key) > 512 {
			return nil, replication.ErrInvalid
		}
		if seenSections[c.Section] == nil {
			seenSections[c.Section] = map[string]bool{}
		}
		if seenSections[c.Section][c.Key] {
			return nil, replication.ErrInvalid
		}
		seenSections[c.Section][c.Key] = true
		if c.Delete {
			result, err := tx.ExecContext(ctx, `DELETE FROM replication_sections WHERE section=? AND key=?`, c.Section, c.Key)
			if err != nil {
				return nil, err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return nil, err
			}
			changed = changed || n > 0
			continue
		}
		entry, err := replication.ValidateEntry(c.Section, replication.Entry{Key: c.Key, Data: c.Data}, l)
		if err != nil {
			return nil, err
		}
		if s.opts.ValidateReplicaEntry != nil {
			if err := s.opts.ValidateReplicaEntry(c.Section, replication.Entry{Key: entry.Key, Data: append(json.RawMessage(nil), entry.Data...)}); err != nil {
				return nil, err
			}
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO replication_sections(section,key,data) VALUES (?,?,?) ON CONFLICT(section,key) DO UPDATE SET data=excluded.data WHERE replication_sections.data!=excluded.data`, c.Section, c.Key, string(entry.Data))
		if err != nil {
			return nil, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		changed = changed || n > 0
	}
	if !changed {
		return out, nil
	}
	if err := advanceRevision(ctx, tx, &a); err != nil {
		return nil, err
	}
	// Ensure the complete transaction is exportable before committing any
	// object/security/dedup rows. Validate only the changed extension input;
	// full owner callbacks run when a receiver prepares its image.
	if _, err := s.readGeneration(ctx, tx); err != nil {
		return nil, err
	}
	var compactTo uint64
	if len(rvs) > 0 && s.sinceCompacted+len(ms) >= s.compactEvery {
		rv := rvs[len(rvs)-1]
		retention := uint64(s.opts.FeedRetention) // #nosec G115 -- Open replaces nonpositive retention with its positive default.
		if rv > retention && rv-retention > s.compacted.Load() {
			compactTo = rv - retention
			if err := compactFeed(ctx, q, compactTo); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.sinceCompacted += len(ms)
	if compactTo > 0 {
		s.compacted.Store(compactTo)
		s.sinceCompacted = 0
	}
	if len(rvs) > 0 {
		s.head.Store(rvs[len(rvs)-1])
	}
	s.publishAuthorityLocked(a)
	s.publishGenerationLocked()
	for n, m := range ms {
		s.bus.publish(change{seq: rvs[n], typ: m.typ, key: m.key, obj: m.obj})
	}
	return out, nil
}
