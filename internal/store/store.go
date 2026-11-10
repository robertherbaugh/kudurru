// Package store is the persistence seam every driver implements
// (dev-docs/design-record.md §4.3). Serving code never reads the store on the
// query path; it reads snapshots materialized by zonecache.
//
// Drivers: kube (CRDs via the apiserver) and sqlite (state.db). Since v0.2
// a sqlite store can also be an enrolled primary or follower: the follower
// package supplies the pull loop that installs the primary's complete image
// into the local sqlite mirror. A raft driver is planned for v1.0. Every
// driver must pass the conformance suite in internal/store/conformance.
package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// SchemaVersion is compiled into the binary and recorded by every driver.
// A newer on-disk version refuses to start; an older one is migrated in one
// transaction (decision D11).
const SchemaVersion = 9

// Kind is the object kind name, e.g. "DNSZone".
type Kind string

// Object is any kudurru API object: Kubernetes metadata plus a runtime
// object so drivers can deep-copy and serialise it.
type Object interface {
	metav1.Object
	runtime.Object
}

// Key identifies one object.
type Key struct {
	Kind      Kind
	Namespace string
	Name      string
}

func (k Key) String() string {
	if k.Namespace == "" {
		return string(k.Kind) + "/" + k.Name
	}
	return string(k.Kind) + "/" + k.Namespace + "/" + k.Name
}

// KeyOf derives the key of an object from its Go type and metadata.
func KeyOf(obj Object) Key {
	return Key{Kind: KindOf(obj), Namespace: obj.GetNamespace(), Name: obj.GetName()}
}

// KindOf returns the kind of an object from its Go type name.
func KindOf(obj Object) Kind {
	t := reflect.TypeOf(obj)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return Kind(t.Name())
}

// Manager values for ApplyOptions.Manager; stored as metadata.managedBy.
const (
	ManagerAPI       = "api"
	ManagerBootstrap = "bootstrap"
	ManagerSystem    = "system"
	// ManagerDynamicUpdate owns the per-zone RFC 2136 record set on drivers
	// without committed zone heads (Kubernetes, D366).
	ManagerDynamicUpdate = "dns-update"
)

// ManagerFile builds the manager string for a file-managed object.
func ManagerFile(path string) string { return "file:" + path }

// ManagerIntegration builds the manager string for an integration.
func ManagerIntegration(name string) string { return "integration:" + name }

// ManagedBy returns the recorded single writer of obj, or "".
func ManagedBy(obj metav1.Object) string {
	return obj.GetAnnotations()[v1alpha1.ManagedByAnnotation]
}

// SetManagedBy records the single writer of obj.
func SetManagedBy(obj metav1.Object, manager string) {
	a := obj.GetAnnotations()
	if a == nil {
		a = map[string]string{}
	}
	a[v1alpha1.ManagedByAnnotation] = manager
	obj.SetAnnotations(a)
}

// ListOptions filters List.
type ListOptions struct {
	Namespace     string
	LabelSelector map[string]string
	Limit         int
	Continue      string
}

// ObjectList is a page of objects.
type ObjectList struct {
	Items    []Object
	Continue string
}

// ApplyOptions controls Apply (the Apply contract, §4.3).
type ApplyOptions struct {
	// Manager becomes metadata.managedBy on first write and must match on
	// later writes unless Force is set.
	Manager string
	// ExpectedResourceVersion enables optimistic concurrency; a mismatch is
	// ErrConflict.
	ExpectedResourceVersion string
	// CreateOnly requires an absent object. Existing objects conflict even
	// when identical or Force is true; the check holds at the commit boundary.
	CreateOnly bool
	// Force overrides managedBy (admin only, audited).
	Force bool
}

// DeleteOptions controls Delete.
type DeleteOptions struct {
	Manager string
	Force   bool
	// ExpectedUID and ExpectedResourceVersion bind deletion to the caller's
	// observed object. Either mismatch is ErrConflict, even with Force. A
	// guarded deletion never re-reads and retries against a newer object.
	ExpectedUID             string
	ExpectedResourceVersion string
}

// CheckDeletePreconditions checks the original identity before a driver plans
// a delete. The driver must retain these conditions through its commit boundary.
func CheckDeletePreconditions(obj metav1.Object, opts DeleteOptions) error {
	if (opts.ExpectedUID != "" && opts.ExpectedUID != string(obj.GetUID())) ||
		(opts.ExpectedResourceVersion != "" && opts.ExpectedResourceVersion != obj.GetResourceVersion()) {
		return ErrConflict
	}
	return nil
}

// IntegrationCleanupFinalizer is the reserved finalizer of an Integration
// whose spec.deletionPolicy is Delete (D367). While it is present, deleting the
// object only sets its deletionTimestamp: the object stays readable with its
// spec, credential references, UID and status until the elected cleanup owner
// has removed what the instance provably owns and clears the finalizer. No
// writer's document decides it; see ReconcileReservedFinalizers.
const IntegrationCleanupFinalizer = "kudurru.io/integration-cleanup"

// HasFinalizer reports whether obj carries the finalizer name.
func HasFinalizer(obj metav1.Object, name string) bool {
	return slices.Contains(obj.GetFinalizers(), name)
}

// Terminated reports whether obj is the removed form of an object: it was
// being deleted and no finalizer remains. Apply and UpdateFinalizer return
// that form when their write removed the object.
func Terminated(obj metav1.Object) bool {
	return obj.GetDeletionTimestamp() != nil && len(obj.GetFinalizers()) == 0
}

// IntegrationCleanupArmed is the pure rule for IntegrationCleanupFinalizer
// after an Apply whose spec carries policy (D367). stored is the object the
// apply replaces, nil on create.
//
//   - A policy other than Delete never carries it. On a driver that admits
//     writes itself (sqlite), changing a live object to Retain disarms it and
//     changing an object that is being deleted to Retain releases it, which
//     removes the object once no other finalizer remains. A driver without
//     admission (kube) stops asserting it: a Force takeover removes it, and
//     otherwise the host that armed it clears it.
//   - An object that is being deleted keeps it exactly while it still has it:
//     only the cleanup owner (UpdateFinalizer) or the change to Retain removes
//     it, and nothing adds it back.
//   - A live object with Delete keeps it when it has it. When it does not,
//     admits decides: sqlite arms it in the same write as the spec, so no
//     instance can own external state unarmed; kube only carries an
//     obligation the elected host armed with UpdateFinalizer, and never
//     invents one that nothing would clear.
func IntegrationCleanupArmed(policy string, stored metav1.Object, admits bool) bool {
	if policy != v1alpha1.IntegrationDeletionDelete {
		return false
	}
	if stored == nil {
		return admits
	}
	had := HasFinalizer(stored, IntegrationCleanupFinalizer)
	if stored.GetDeletionTimestamp() != nil {
		return had
	}
	return had || admits
}

// ReconcileReservedFinalizers rewrites the reserved finalizers of next, the
// object an Apply is about to write, from its defaulted spec and the stored
// object it replaces (nil on create). The caller's document cannot add or
// remove a reserved finalizer; its other finalizers are kept in order, and a
// reserved one that the rule requires is stored last. Kinds without a
// reserved finalizer are not touched.
func ReconcileReservedFinalizers(next Object, stored metav1.Object, admits bool) {
	i, ok := next.(*v1alpha1.Integration)
	if !ok || i == nil {
		return
	}
	finalizers := slices.DeleteFunc(slices.Clone(i.Finalizers), func(name string) bool { return name == IntegrationCleanupFinalizer })
	if IntegrationCleanupArmed(i.Spec.DeletionPolicy, stored, admits) {
		finalizers = append(finalizers, IntegrationCleanupFinalizer)
	}
	if len(finalizers) == 0 {
		finalizers = nil
	}
	i.Finalizers = finalizers
}

// FinalizerOptions selects one metadata-only finalizer change.
type FinalizerOptions struct {
	// Finalizer is the qualified name to add, or to remove when Remove is set.
	Finalizer string
	Remove    bool
	// ExpectedUID and ExpectedResourceVersion are both required and bind the
	// change to the exact object the caller observed. Either mismatch is
	// ErrConflict; the driver never re-reads and retries on a newer object.
	ExpectedUID             string
	ExpectedResourceVersion string
}

// Validate checks that the change names one finalizer and its exact target.
func (o FinalizerOptions) Validate() error {
	if o.ExpectedUID == "" || o.ExpectedResourceVersion == "" {
		return errors.New("store: FinalizerOptions requires ExpectedUID and ExpectedResourceVersion")
	}
	if !strings.Contains(o.Finalizer, "/") || len(validation.IsQualifiedName(o.Finalizer)) != 0 {
		return fmt.Errorf("store: %q is not a qualified finalizer name", o.Finalizer)
	}
	return nil
}

// PlanFinalizer applies opts to the finalizers of the stored object obj. It
// returns the next list and whether it differs. A mismatched UID or
// resourceVersion is ErrConflict, and adding to an object that is already
// being deleted is ErrTerminating (the apiserver forbids it too).
func PlanFinalizer(obj metav1.Object, opts FinalizerOptions) (next []string, changed bool, err error) {
	if err := opts.Validate(); err != nil {
		return nil, false, err
	}
	if opts.ExpectedUID != string(obj.GetUID()) || opts.ExpectedResourceVersion != obj.GetResourceVersion() {
		return nil, false, ErrConflict
	}
	current := obj.GetFinalizers()
	has := slices.Contains(current, opts.Finalizer)
	switch {
	case opts.Remove && !has, !opts.Remove && has:
		return slices.Clone(current), false, nil
	case opts.Remove:
		next = slices.DeleteFunc(slices.Clone(current), func(name string) bool { return name == opts.Finalizer })
		if len(next) == 0 {
			next = nil
		}
		return next, true, nil
	case obj.GetDeletionTimestamp() != nil:
		return nil, false, ErrTerminating
	default:
		return append(slices.Clone(current), opts.Finalizer), true, nil
	}
}

// FinalizerWriter is implemented by drivers that can change one entry of
// metadata.finalizers without replacing the object (sqlite, kube). The write
// keeps the manager, labels, annotations, spec, status and generation, and
// changes nothing when the finalizer is already in the requested state.
// Removing the last finalizer of an object that is being deleted removes the
// object. UpdateFinalizer returns the stored object, or the removed object
// (see Terminated) when its write removed it.
//
// It is the only way the cleanup owner clears IntegrationCleanupFinalizer,
// and how a host arms it on a driver without admission (D367).
type FinalizerWriter interface {
	UpdateFinalizer(ctx context.Context, key Key, opts FinalizerOptions) (Object, error)
}

// DeleteRequest is one delete of a cascade (DeleteAll, MultiDeleter).
type DeleteRequest struct {
	Key     Key
	Options DeleteOptions
}

// MultiDeleter is implemented by drivers that delete several objects in one
// transaction (sqlite, memory). DeleteMany applies the Delete contract to
// every request in order and commits all of them or none: when one fails
// (managed by another writer, closed, ...) nothing is deleted and the
// error names its key. Requests for objects that do not exist are skipped.
// The kube driver has no multi-object transaction and does not implement
// it.
type MultiDeleter interface {
	DeleteMany(ctx context.Context, reqs []DeleteRequest) error
}

// DeleteAll deletes reqs in order: in one transaction when st is a
// MultiDeleter (atomic is true; deleted is len(reqs) or 0), otherwise one
// Delete after the other, stopping at the first failure, so the requests
// before it are deleted and the rest are not (deleted counts them).
// Objects that no longer exist count as deleted.
func DeleteAll(ctx context.Context, st Store, reqs []DeleteRequest) (deleted int, atomic bool, err error) {
	if md, ok := st.(MultiDeleter); ok {
		if err := md.DeleteMany(ctx, reqs); err != nil {
			return 0, true, err
		}
		return len(reqs), true, nil
	}
	for _, r := range reqs {
		if err := st.Delete(ctx, r.Key, r.Options); err != nil && !errors.Is(err, ErrNotFound) {
			return deleted, false, err
		}
		deleted++
	}
	return deleted, false, nil
}

// EventType is the kind of watch event.
type EventType string

// Event types.
const (
	Added    EventType = "ADDED"
	Modified EventType = "MODIFIED"
	Deleted  EventType = "DELETED"
	Bookmark EventType = "BOOKMARK"
)

// Event is one watch event.
type Event struct {
	Type   EventType
	Object Object
	// Cursor is a local watch position. HA cursors include a lineage and must
	// not be confused with the source object's resourceVersion.
	Cursor string
}

// WatchOptions controls Watch.
type WatchOptions struct {
	Namespace string
	// ResourceVersion to start after; "" lists then watches.
	ResourceVersion string
}

// Watcher delivers events until Stop or its source stream ends. Drivers may
// additionally implement interface{ Err() error } to report terminal stream
// errors after ResultChan closes. Resume and compaction behavior is
// documented by each driver.
type Watcher interface {
	ResultChan() <-chan Event
	Stop()
}

// SnapshotOptions controls Snapshot.
type SnapshotOptions struct {
	// Safe strips secrets, sessions and tokens.
	Safe bool
}

// Elector is the leader-election primitive each driver provides: a Lease
// (kube), or the store's durable write authority (sqlite: a standalone node
// or enrolled primary leads; a follower, fenced or conflicted node does not).
//
// Run campaigns until ctx is cancelled and returns nil then; losing
// leadership never returns from Run (D44, D78). Each leadership period is a
// term: onStarted receives a term context that is cancelled when the term
// ends (leadership lost or ctx done) and may block until then, which is the
// shape of a leader-only runnable group. onStopped, if non-nil, runs after
// onStarted has returned and the term context is done, and must stop all
// leader-only work before returning, because a successor may start once it
// returns. Terms never overlap within one elector. Either callback may be
// nil.
type Elector interface {
	Run(ctx context.Context, onStarted func(term context.Context), onStopped func()) error
	// IsLeader reports whether a term is active.
	IsLeader() bool
	// Leader returns the identity of the last observed leader, or "".
	Leader() string
}

// Info describes the open store.
type Info struct {
	Driver        string
	SchemaVersion int
	ClusterID     string
	Epoch         uint64
	Leader        string
	MemberID      string
	PrimaryID     string
	PromotionID   string
	Role          string
	Fenced        bool
	Revision      uint64
	// LastContact is when the driver last completed a round trip with the
	// service behind it (the apiserver for kube). Zero means never, or a
	// local driver that is always in contact (sqlite, memory). zonecache
	// uses it so an outage does not keep the local.db snapshot looking
	// current.
	LastContact time.Time
}

// Store is the single persistence interface.
type Store interface {
	Get(ctx context.Context, key Key, into Object) error
	List(ctx context.Context, kind Kind, opts ListOptions) (ObjectList, error)
	Apply(ctx context.Context, obj Object, opts ApplyOptions) (Object, error)
	Delete(ctx context.Context, key Key, opts DeleteOptions) error
	UpdateStatus(ctx context.Context, obj Object) error
	Watch(ctx context.Context, kind Kind, opts WatchOptions) (Watcher, error)
	Elector(name string) Elector
	Snapshot(ctx context.Context, w io.Writer, opts SnapshotOptions) error
	Restore(ctx context.Context, r io.Reader) error
	Info() Info
	Close() error
}

// MaxObjectBytes is the largest encoded object a driver stores: the 1.5 MiB
// request cap of etcd, so an object that fits the sqlite driver also fits a
// Kubernetes apiserver. The API refuses larger request bodies with 413.
const MaxObjectBytes = 1536 << 10

// Sentinel errors every driver returns.
var (
	ErrNotFound = errors.New("store: not found")
	ErrConflict = errors.New("store: resource version conflict")
	ErrClosed   = errors.New("store: closed")
	// ErrTerminating: the object is being deleted, so the write cannot add an
	// obligation to it (a finalizer on an object with a deletionTimestamp).
	ErrTerminating = errors.New("store: object is being deleted")
	// ErrAuditIntegrity identifies changed audit contents or broken chain links,
	// as distinct from an unavailable backend or a canceled verification.
	ErrAuditIntegrity = errors.New("audit: integrity check failed")
	// ErrTooLarge: the encoded object exceeds MaxObjectBytes (sqlite) or
	// the apiserver refused it as too large (kube). HTTP maps it to 413.
	ErrTooLarge = errors.New("store: object too large")
)

// ErrManagedBy is returned when a writer other than the recorded manager
// tries to change an object. HTTP maps it to 409 with the owner in hint.
type ErrManagedBy struct {
	Key   Key
	Owner string
}

func (e *ErrManagedBy) Error() string {
	return fmt.Sprintf("store: %s is managed by %q", e.Key, e.Owner)
}

// IsManagedBy reports whether err is an ErrManagedBy.
func IsManagedBy(err error) bool {
	var e *ErrManagedBy
	return errors.As(err, &e)
}
