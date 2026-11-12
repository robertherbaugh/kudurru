package upstream

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/tracing"
)

func traceOption(m *dns.Msg, code uint16) []byte {
	if m == nil || m.IsEdns0() == nil {
		return nil
	}
	for _, o := range m.IsEdns0().Option {
		if l, ok := o.(*dns.EDNS0_LOCAL); ok && l.Code == code {
			return l.Data
		}
	}
	return nil
}

// TestTracePropagationOnlyToFlaggedGroups pins D382: a traced query adds
// TRACEPARENT only to the servers of a group with TracePropagation, under
// the attempt span's id; every attempt is a client span of the caller's
// span; and a public group never sees the option.
func TestTracePropagationOnlyToFlaggedGroups(t *testing.T) {
	public, inner := startFake(t), startFake(t)
	s, err := New(Config{Groups: map[string]GroupConfig{
		DefaultGroup: {Servers: []string{"public=" + public.addr}, Strategy: Strict, Timeout: time.Second},
		"inner":      {Servers: []string{"inner=" + inner.addr}, Strategy: Strict, Timeout: time.Second, TracePropagation: true},
	}}, Options{TraceOptionCode: 65432})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	tracer, err := tracing.New(tracing.Config{Ratio: 1, Exporter: &tracing.Memory{}})
	if err != nil {
		t.Fatal(err)
	}
	client := new(dns.Msg)
	client.SetQuestion("www.example.org.", dns.TypeA)
	client.SetEdns0(1232, false)
	trace, _ := tracer.Begin(client, false, time.Now())
	stage := trace.Start(tracing.Root, "kudurru.upstream", tracing.KindInternal, time.Now())
	ctx := tracing.NewContext(context.Background(), trace, stage)

	for _, group := range []string{DefaultGroup, "inner"} {
		if _, err := s.Exchange(ctx, group, s.PrepareQuery(client, netip.Addr{})); err != nil {
			t.Fatal(group, err)
		}
	}
	if data := traceOption(public.lastQuery(), 65432); data != nil {
		t.Fatal("a group without tracePropagation received TRACEPARENT")
	}
	data := traceOption(inner.lastQuery(), 65432)
	remote, ok := tracing.ParseOption(data)
	if !ok || remote.TraceID != trace.ID() || !remote.HasFlags || remote.Flags != tracing.FlagSampled {
		t.Fatalf("propagated option %x parsed %+v %v", data, remote, ok)
	}
	// An untraced exchange sends nothing even to the flagged group.
	if _, err := s.Exchange(context.Background(), "inner", s.PrepareQuery(client, netip.Addr{})); err != nil {
		t.Fatal(err)
	}
	if traceOption(inner.lastQuery(), 65432) != nil {
		t.Fatal("an untraced query propagated TRACEPARENT")
	}
	// A failed attempt is a failed client span.
	public.set(servfail, 0)
	if _, err := s.Exchange(ctx, DefaultGroup, s.PrepareQuery(client, netip.Addr{})); err == nil {
		t.Fatal("servfail accepted")
	}
	trace.Decide(0, false)
	trace.Finish(time.Now())
	spans := trace.Spans()
	if len(spans) != 5 {
		t.Fatalf("%d spans", len(spans))
	}
	stageID := spans[1].ID
	for i, sp := range spans[2:] {
		if sp.Name != "kudurru.upstream.exchange" || sp.Kind != tracing.KindClient || sp.Parent != stageID {
			t.Fatalf("attempt %d: %+v", i, sp)
		}
	}
	if spans[3].ID != remote.SpanID {
		t.Fatal("the propagated parent id is not the attempt span")
	}
	if !spans[4].Error || spans[4].Message != "SERVFAIL" || spans[2].Error {
		t.Fatalf("attempt status: %+v / %+v", spans[2], spans[4])
	}
}

func TestRecursiveGroupRefusesPropagation(t *testing.T) {
	err := Config{Groups: map[string]GroupConfig{DefaultGroup: {Recursive: true, TracePropagation: true}}}.Validate()
	if err == nil {
		t.Fatal("a recursive group with tracePropagation was accepted")
	}
}
