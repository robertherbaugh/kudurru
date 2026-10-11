// Package store is the persistence seam every driver implements
// (docs/architecture.md §4.3). Serving code never reads the store on the
// query path; it reads snapshots materialised by zonecache.
//
// Drivers: kube (CRDs via the apiserver) and sqlite (state.db) in v0.1,
// follower in v0.2, raft in v1.0. Every driver must pass the conformance
// suite in internal/store/conformance.
package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// SchemaVersion is compiled into the binary and recorded by every driver.
// A newer on-disk version refuses to start; an older one is migrated in one
// transaction (decision D11).
const SchemaVersion = 1

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
	// Force overrides managedBy (admin only, audited).
	Force bool
}

// DeleteOptions controls Delete.
type DeleteOptions struct {
	Manager string
	Force   bool
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
}

// WatchOptions controls Watch.
type WatchOptions struct {
	Namespace string
	// ResourceVersion to start after; "" lists then watches.
	ResourceVersion string
}

// Watcher delivers events until Stop.
type Watcher interface {
	ResultChan() <-chan Event
	Stop()
}

// SnapshotOptions controls Snapshot.
type SnapshotOptions struct {
	// Safe strips secrets, sessions and tokens.
	Safe bool
}

// Elector is the leader-election primitive each driver provides:
// Lease (kube), always-leader (sqlite), never-leader (follower), raft.
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

// Sentinel errors every driver returns.
var (
	ErrNotFound = errors.New("store: not found")
	ErrConflict = errors.New("store: resource version conflict")
	ErrClosed   = errors.New("store: closed")
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
