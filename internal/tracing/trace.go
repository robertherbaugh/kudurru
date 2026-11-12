// Package tracing records OpenTelemetry-shaped traces of DNS queries and
// hands the ones it keeps to an exporter (dev-docs/design-record.md §11.5,
// D382). It is a small in-house tracer, not the OpenTelemetry SDK: the
// query path records every span of a traced query into one preallocated
// Trace, and the keep decision is made when the query is answered, so a
// failed or slow query can be kept although the head sampler did not pick
// it (tail keep). Nothing here blocks: a kept trace is offered to a bounded
// queue and dropped, counted, when the queue is full.
//
// All Trace methods are safe on a nil *Trace and then do nothing, so call
// sites need no guard when tracing is off.
package tracing

import (
	"math/rand/v2"
	"sync"
	"time"
)

// Kind is the OTLP span kind.
type Kind uint8

// Span kinds, with the OTLP enum values.
const (
	KindInternal Kind = 1
	KindServer   Kind = 2
	KindClient   Kind = 3
)

// Attr is one span attribute: a string, or an integer when IsInt is set.
type Attr struct {
	Key   string
	Str   string
	Int   int64
	IsInt bool
}

// Span is one recorded span. It is read only after the trace finished.
type Span struct {
	ID     SpanID
	Parent SpanID
	Name   string
	Kind   Kind
	Start  time.Time
	End    time.Time
	// Error marks a failed operation; Message says why (never query data).
	Error   bool
	Message string
	Attrs   []Attr
}

// SpanRef names a span within its trace. The zero value is "no span": a
// child started from it becomes a child of the trace's root span.
type SpanRef int32

// Root is the reference of the server span every trace starts with.
const Root SpanRef = 1

// MaxSpans bounds the spans of one trace: a CNAME chain or a rewrite that
// re-enters the pipeline adds spans, and a bounded trace keeps the memory
// of one query bounded. Spans past it are counted, not recorded.
const MaxSpans = 64

// maxAttrs bounds the attributes of one span.
const maxAttrs = 12

// Trace is the trace of one query.
type Trace struct {
	tracer *Tracer

	id     TraceID
	remote SpanID
	// sampled is the head decision: the caller's sampled flag, or the
	// ratio sampler when the caller sent none.
	sampled bool
	// fromPeer is set when the trace continues a TRACEPARENT from a trusted
	// edge peer.
	fromPeer bool

	mu       sync.Mutex
	finished bool
	kept     bool
	dropped  int
	spans    []Span
	inline   [12]Span
}

// ID returns the trace id; zero for a nil trace.
func (t *Trace) ID() TraceID {
	if t == nil {
		return TraceID{}
	}
	return t.id
}

// Sampled reports the head decision.
func (t *Trace) Sampled() bool { return t != nil && t.sampled }

// FromPeer reports whether the trace continues a trusted peer's trace.
func (t *Trace) FromPeer() bool { return t != nil && t.fromPeer }

// Remote returns the caller's span id, zero when the trace started here.
func (t *Trace) Remote() SpanID {
	if t == nil {
		return SpanID{}
	}
	return t.remote
}

// Kept reports whether the finished trace was kept for export. It is
// false before Decide.
func (t *Trace) Kept() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.kept
}

// Spans returns the recorded spans of a finished trace; the slice must not
// be changed. Before Finish it returns nil.
func (t *Trace) Spans() []Span {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.finished {
		return nil
	}
	return t.spans
}

// DroppedSpans is the number of spans past MaxSpans.
func (t *Trace) DroppedSpans() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.dropped
}

func newSpanID() SpanID {
	for {
		v := rand.Uint64() //nolint:gosec // span ids need uniqueness, not secrecy (as the OpenTelemetry SDK)
		if v != 0 {
			var s SpanID
			for i := range 8 {
				s[i] = byte(v >> (56 - 8*i))
			}
			return s
		}
	}
}

func newTraceID() TraceID {
	var t TraceID
	for !t.IsValid() {
		hi, lo := rand.Uint64(), rand.Uint64() //nolint:gosec // as newSpanID
		for i := range 8 {
			t[i] = byte(hi >> (56 - 8*i))
			t[8+i] = byte(lo >> (56 - 8*i))
		}
	}
	return t
}

// Start records the start of a span under parent (the zero SpanRef means
// the root span) and returns its reference. A finished trace, or one that
// holds MaxSpans spans, records nothing and returns the zero SpanRef.
func (t *Trace) Start(parent SpanRef, name string, kind Kind, at time.Time) SpanRef {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return 0
	}
	if len(t.spans) >= MaxSpans {
		t.dropped++
		return 0
	}
	if parent <= 0 || int(parent) > len(t.spans) {
		parent = Root
	}
	var pid SpanID
	if len(t.spans) == 0 {
		pid = t.remote
	} else {
		pid = t.spans[parent-1].ID
	}
	t.spans = append(t.spans, Span{ID: newSpanID(), Parent: pid, Name: name, Kind: kind, Start: at})
	return SpanRef(len(t.spans))
}

// SpanID returns the id of a recorded span, zero for an unknown reference.
func (t *Trace) SpanID(ref SpanRef) SpanID {
	if t == nil {
		return SpanID{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if ref <= 0 || int(ref) > len(t.spans) {
		return SpanID{}
	}
	return t.spans[ref-1].ID
}

// SetStr sets a string attribute on a span.
func (t *Trace) SetStr(ref SpanRef, key, value string) {
	t.setAttr(ref, Attr{Key: key, Str: value})
}

// SetInt sets an integer attribute on a span.
func (t *Trace) SetInt(ref SpanRef, key string, value int64) {
	t.setAttr(ref, Attr{Key: key, Int: value, IsInt: true})
}

func (t *Trace) setAttr(ref SpanRef, a Attr) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished || ref <= 0 || int(ref) > len(t.spans) {
		return
	}
	s := &t.spans[ref-1]
	if len(s.Attrs) < maxAttrs {
		s.Attrs = append(s.Attrs, a)
	}
}

// End records the end of a span. A non-empty failure marks it failed.
// Ending a span of a finished trace (a raced upstream exchange that
// returned after the answer) does nothing.
func (t *Trace) End(ref SpanRef, at time.Time, failure string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished || ref <= 0 || int(ref) > len(t.spans) {
		return
	}
	s := &t.spans[ref-1]
	s.End = at
	if failure != "" {
		s.Error, s.Message = true, failure
	}
}

// Decide makes the keep decision for the answered query (D382): a trace
// is kept when the head sampler picked it, when the query failed, or when
// it took at least the slow threshold. It does not finish the trace, so the
// caller can still add the attributes a kept trace exports.
func (t *Trace) Decide(latency time.Duration, failed bool) bool {
	if t == nil {
		return false
	}
	keep := t.sampled || failed || (t.tracer.slow > 0 && latency >= t.tracer.slow)
	t.mu.Lock()
	t.kept = keep
	t.mu.Unlock()
	return keep
}

// Finish ends the root span and every span still open at end, closes the
// trace to further changes and, when Decide kept it, offers it to the
// export queue without blocking.
func (t *Trace) Finish(end time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.finished {
		t.mu.Unlock()
		return
	}
	for i := range t.spans {
		if t.spans[i].End.IsZero() {
			t.spans[i].End = end
		}
	}
	t.finished = true
	kept := t.kept
	t.mu.Unlock()
	if kept {
		t.tracer.offer(t)
	} else {
		t.tracer.metrics.unkept()
	}
}
