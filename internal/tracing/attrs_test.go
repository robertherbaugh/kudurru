package tracing

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// TestAttributesPastTheInlineArray: attributes recorded interleaved across
// spans, more than the inline array holds, come back per span in the order
// they were set, each span capped at maxAttrs.
func TestAttributesPastTheInlineArray(t *testing.T) {
	tr, _, _ := newTestTracer(t, Config{Ratio: 1})
	now := time.Now()
	trace, _ := tr.Begin(query(), false, now)
	refs := []SpanRef{Root}
	for i := range inlineSpans + 3 {
		refs = append(refs, trace.Start(Root, "s"+strconv.Itoa(i), KindInternal, now))
	}
	for round := range maxAttrs + 2 {
		for _, ref := range refs {
			trace.SetInt(ref, "k"+strconv.Itoa(round), int64(round))
		}
	}
	trace.Decide(0, false)
	trace.Finish(now)
	spans := trace.Spans()
	if len(spans) != len(refs) {
		t.Fatalf("%d spans", len(spans))
	}
	for i, s := range spans {
		if len(s.Attrs) != maxAttrs {
			t.Fatalf("span %d has %d attributes", i, len(s.Attrs))
		}
		for j, a := range s.Attrs {
			if a.Key != "k"+strconv.Itoa(j) || a.Int != int64(j) || !a.IsInt {
				t.Fatalf("span %d attribute %d = %+v", i, j, a)
			}
		}
	}
	// A second read returns the same, already built spans.
	if again := trace.Spans(); &again[0] != &spans[0] || len(again[1].Attrs) != maxAttrs {
		t.Fatal("Spans rebuilt the attributes")
	}
}

// TestContextCarriesTraceAndParent: the one-allocation trace context
// passes other values and cancellation through.
func TestContextCarriesTraceAndParent(t *testing.T) {
	tr, _, _ := newTestTracer(t, Config{Ratio: 1})
	trace, _ := tr.Begin(query(), false, time.Now())
	type key struct{}
	parent, cancel := contextWithValueAndCancel(key{}, "v")
	ctx := NewContext(parent, trace, 3)
	if got, span := FromContext(ctx); got != trace || span != 3 {
		t.Fatalf("FromContext = %p %d", got, span)
	}
	if ctx.Value(key{}) != "v" {
		t.Fatal("the parent's value is lost")
	}
	cancel()
	<-ctx.Done()
	if got, _ := FromContext(parent); got != nil {
		t.Fatal("the parent carries the trace")
	}
}

func contextWithValueAndCancel(key, value any) (context.Context, context.CancelFunc) {
	return context.WithCancel(context.WithValue(context.Background(), key, value))
}
