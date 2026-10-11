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
	// ID is assigned by recorders that store events; 0 otherwise.
	ID   int64     `json:"id,omitempty"`
	Time time.Time `json:"ts"`
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
	Manager string  `json:"manager,omitempty"`
	Request Request `json:"request"`
	// Hash and PrevHash chain stored events (sqlite recorder only).
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
	// Limit bounds the page; default 100, maximum 1000.
	Limit int
}

// Reader reads stored events back, oldest first.
type Reader interface {
	Read(ctx context.Context, q Query) ([]Event, error)
}

// ErrNotStored is returned by recorders that cannot read events back.
var ErrNotStored = errors.New("audit: events are not stored on this node")

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
	for _, r := range m {
		if rd, ok := r.(Reader); ok {
			return rd.Read(ctx, q)
		}
	}
	return nil, ErrNotStored
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
	m.mu.Lock()
	defer m.mu.Unlock()
	limit := clampLimit(q.Limit)
	out := []Event{}
	for _, ev := range m.events {
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

func clampLimit(n int) int {
	switch {
	case n <= 0:
		return 100
	case n > 1000:
		return 1000
	}
	return n
}
