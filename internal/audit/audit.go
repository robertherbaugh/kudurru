// Package audit is the audit seam of docs/architecture.md §11.4: one
// immutable Event per mutating API action, denied attempt or runtime
// action that is not already audited by a store write, handed to a
// Recorder. Store writes are audited by the store driver itself (the
// sqlite driver chains them in state.db in the same transaction); the API
// records what no store write covers: authorization denials, the blocking
// toggle, cache flushes, query-log purges, NOTIFY and list refreshes.
//
// Recorders: SQLite appends to the sqlite store's hash-chained audit table
// (RecordAudit) and reads it back for GET /api/v1/audit; Log writes the
// JSON line on stdout ("log":"audit") that Kubernetes log pipelines
// capture; Memory keeps events in memory (tests, the kube driver without a
// persistent local.db); Multi fans out.
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"time"
)

// Results of an audited action (§11.4).
const (
	ResultSuccess = "success"
	ResultDenied  = "denied"
	ResultError   = "error"
)

// Actor is who acted.
type Actor struct {
	// Kind is the principal kind (User, ServiceAccount, ...).
	Kind string `json:"kind,omitempty"`
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	// Groups are the principal's groups at the time of the action.
	Groups []string `json:"groups,omitempty"`
	// Auth is how the principal authenticated: cookie, token, jwt, k8s,
	// unix, internal; "" for anonymous requests.
	Auth string `json:"auth,omitempty"`
}

// String is the actor as stored in the sqlite audit table's actor column:
// "<kind>:<name>" ("anonymous" when unauthenticated).
func (a Actor) String() string {
	name := a.Name
	if name == "" {
		name = a.ID
	}
	switch {
	case name == "":
		return "anonymous"
	case a.Kind == "":
		return name
	}
	return a.Kind + ":" + name
}

// Resource names the object or virtual resource acted upon.
type Resource struct {
	// Kind is a store kind ("DNSZone") or an RBAC resource ("blocking").
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
}

// Request carries the HTTP context of the action.
type Request struct {
	ID     string `json:"id,omitempty"`
	IP     string `json:"ip,omitempty"`
	UA     string `json:"ua,omitempty"`
	Method string `json:"method,omitempty"`
	Path   string `json:"path,omitempty"`
}

// Event is one audit record.
type Event struct {
	// ID is assigned by recorders that store events; 0 otherwise. It is
	// unique per store, or per node in a cluster-wide store (see UID).
	ID int64 `json:"id,omitempty"`
	// UID is unique across the nodes of a cluster-wide store
	// ("<source>-<id>"); empty for a node's own store, where ID is.
	UID string `json:"uid,omitempty"`
	// Cursor, when set, is the keyset position of the event for
	// Query.Cursor (stores whose order is not ID's); otherwise the
	// position is ID.
	Cursor string    `json:"-"`
	Time   time.Time `json:"ts"`
	// Node is the node that handled the action.
	Node  string `json:"node,omitempty"`
	Actor Actor  `json:"actor"`
	// Action is "<resource>.<verb>", e.g. "blocking.toggle".
	Action   string   `json:"action"`
	Resource Resource `json:"resource"`
	// Result is ResultSuccess, ResultDenied or ResultError.
	Result string `json:"result"`
	Reason string `json:"reason,omitempty"`
	// Authz is the authorizer path that decided: binding, sar, local or
	// none.
	Authz string `json:"authz,omitempty"`
	// Before and After are the redacted states around the action.
	Before json.RawMessage `json:"before,omitempty"`
	After  json.RawMessage `json:"after,omitempty"`
	// Manager is the store manager of a store write ("api", "file:...").
	Manager string `json:"manager,omitempty"`
	// ResourceVersion is the object's resourceVersion after a store write.
	ResourceVersion string  `json:"resource_version,omitempty"`
	Request         Request `json:"request"`
	// Hash and PrevHash chain stored events (recorders that store them;
	// ChainHash).
	Hash     string `json:"hash,omitempty"`
	PrevHash string `json:"prev_hash,omitempty"`
}

// Recorder stores or emits events. Record must not block for long: the
// API calls it on the request goroutine after the response is decided.
type Recorder interface {
	Record(ctx context.Context, ev Event) error
}

// Query filters Reader.Read. Zero fields do not filter.
type Query struct {
	Kind  string
	Name  string
	Actor string
	Since time.Time
	// AfterID is the keyset cursor: events with ID > AfterID.
	AfterID int64
	// BeforeID, when positive, is the keyset cursor of a Desc page:
	// events with ID < BeforeID.
	BeforeID int64
	// Cursor is the keyset cursor of stores that set Event.Cursor: the
	// events after it in the read order. Stores ordered by ID refuse it
	// (ErrBadCursor), and those that set Event.Cursor refuse AfterID and
	// BeforeID.
	Cursor string
	// Limit bounds the page; default 100, maximum 1000.
	Limit int
	// Desc reads newest first.
	Desc bool
}

// Reader reads stored events back, oldest first (newest first with
// Query.Desc).
type Reader interface {
	Read(ctx context.Context, q Query) ([]Event, error)
}

// ErrNotStored is returned by recorders that cannot read events back.
var ErrNotStored = errors.New("audit: events are not stored on this node")

// ErrBadCursor is returned by readers for a cursor they did not issue.
var ErrBadCursor = errors.New("audit: not a cursor of this audit store")

// Position is the keyset position of ev: Cursor when the store sets it,
// else ID in decimal.
func (ev Event) Position() string {
	if ev.Cursor != "" {
		return ev.Cursor
	}
	return strconv.FormatInt(ev.ID, 10)
}

// After moves q past ev, the last event of a page read with q.
func (q *Query) After(ev Event) {
	switch {
	case ev.Cursor != "":
		q.Cursor = ev.Cursor
	case q.Desc:
		q.BeforeID = ev.ID
	default:
		q.AfterID = ev.ID
	}
}

// MaxDiff is the largest Before/After document kept (4 KiB, §11.4).
const MaxDiff = 4096

// Diff marshals v for Event.Before/After, replacing documents larger than
// MaxDiff by a {"truncated":true,"bytes":N} marker. Callers redact secrets
// before passing v.
func Diff(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	if len(b) > MaxDiff {
		b, _ = json.Marshal(map[string]any{"truncated": true, "bytes": len(b)})
	}
	return b
}

// Multi records to every recorder and returns the joined errors.
type Multi []Recorder

// Record implements Recorder.
func (m Multi) Record(ctx context.Context, ev Event) error {
	var errs []error
	for _, r := range m {
		if r == nil {
			continue
		}
		if err := r.Record(ctx, ev); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Read implements Reader with the first member that stores events.
func (m Multi) Read(ctx context.Context, q Query) ([]Event, error) {
	if rd, ok := m.Reader(); ok {
		return rd.Read(ctx, q)
	}
	return nil, ErrNotStored
}

// Reader returns the first member that stores events; ok is false when
// none does (Read would return ErrNotStored).
func (m Multi) Reader() (rd Reader, ok bool) {
	for _, r := range m {
		if sub, isMulti := r.(Multi); isMulti {
			if rd, ok = sub.Reader(); ok {
				return rd, true
			}
			continue
		}
		if rd, ok = r.(Reader); ok {
			return rd, true
		}
	}
	return nil, false
}

// ReaderOf returns the Reader behind a Recorder: r itself, or for a Multi
// its first storing member; ok is false when r cannot read events back.
func ReaderOf(r Recorder) (Reader, bool) {
	if m, isMulti := r.(Multi); isMulti {
		return m.Reader()
	}
	rd, ok := r.(Reader)
	return rd, ok
}

// Gated records to Inner only while Allowed reports true, and reads from it
// only then too. The persisted audit trail is a licensed feature
// (license.FeatureAudit); the log lines of the Log recorder are not, so a
// node without the license still writes every event to its log.
type Gated struct {
	Inner   Recorder
	Allowed func() bool
}

// Record implements Recorder.
func (g Gated) Record(ctx context.Context, ev Event) error {
	if g.Allowed != nil && !g.Allowed() {
		return nil
	}
	return g.Inner.Record(ctx, ev)
}

// Read implements Reader.
func (g Gated) Read(ctx context.Context, q Query) ([]Event, error) {
	rd, ok := g.Inner.(Reader)
	if !ok || (g.Allowed != nil && !g.Allowed()) {
		return nil, ErrNotStored
	}
	return rd.Read(ctx, q)
}

// Log writes every event as one JSON log line with "log":"audit" (§11.4:
// the durable path in Kubernetes is the log pipeline).
type Log struct {
	Logger *slog.Logger
}

// Record implements Recorder.
func (l Log) Record(ctx context.Context, ev Event) error {
	lg := l.Logger
	if lg == nil {
		lg = slog.Default()
	}
	lg.LogAttrs(ctx, slog.LevelInfo, "audit",
		slog.String("log", "audit"),
		slog.String("action", ev.Action),
		slog.String("result", ev.Result),
		slog.String("actor", ev.Actor.String()),
		slog.String("authz", ev.Authz),
		slog.Any("resource", ev.Resource),
		slog.String("reason", ev.Reason),
		slog.Any("request", ev.Request),
		slog.String("node", ev.Node),
	)
	return nil
}

// Memory keeps up to Max events in memory (default 10 000) and reads them
// back; it implements Recorder and Reader.
type Memory struct {
	Max int

	mu     sync.Mutex
	events []Event
	next   int64
}

// Record implements Recorder.
func (m *Memory) Record(_ context.Context, ev Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	ev.ID = m.next
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	m.events = append(m.events, ev)
	limit := m.Max
	if limit <= 0 {
		limit = 10000
	}
	if over := len(m.events) - limit; over > 0 {
		m.events = slices.Delete(m.events, 0, over)
	}
	return nil
}

// Read implements Reader.
func (m *Memory) Read(_ context.Context, q Query) ([]Event, error) {
	if q.Cursor != "" {
		return nil, ErrBadCursor
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	limit := ClampLimit(q.Limit)
	out := []Event{}
	for i := range m.events {
		ev := m.events[i]
		if q.Desc {
			ev = m.events[len(m.events)-1-i]
		}
		if !q.match(ev) {
			continue
		}
		out = append(out, ev)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// Events returns a copy of every stored event.
func (m *Memory) Events() []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.events)
}

func (q Query) match(ev Event) bool {
	switch {
	case ev.ID <= q.AfterID:
		return false
	case q.BeforeID > 0 && ev.ID >= q.BeforeID:
		return false
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

// ClampLimit applies the default and the maximum page size of a Query.
func ClampLimit(n int) int {
	switch {
	case n <= 0:
		return 100
	case n > 1000:
		return 1000
	}
	return n
}
