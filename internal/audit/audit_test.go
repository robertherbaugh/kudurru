package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestActorString(t *testing.T) {
	cases := []struct {
		a    Actor
		want string
	}{
		{Actor{}, "anonymous"},
		{Actor{Name: "alice"}, "alice"},
		{Actor{Kind: "User", Name: "alice"}, "User:alice"},
		{Actor{Kind: "ServiceAccount", ID: "sa-1"}, "ServiceAccount:sa-1"},
	}
	for _, tc := range cases {
		if got := tc.a.String(); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.a, got, tc.want)
		}
	}
}

func TestDiffCap(t *testing.T) {
	if Diff(nil) != nil {
		t.Fatal("nil diff")
	}
	small := Diff(map[string]any{"a": 1})
	if string(small) != `{"a":1}` {
		t.Fatalf("small %s", small)
	}
	big := Diff(map[string]string{"x": strings.Repeat("y", MaxDiff)})
	var m map[string]any
	if err := json.Unmarshal(big, &m); err != nil || m["truncated"] != true {
		t.Fatalf("big %s", big)
	}
}

func TestMemoryReadFilters(t *testing.T) {
	m := &Memory{Max: 3}
	ctx := context.Background()
	t0 := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	for i, ev := range []Event{
		{Time: t0, Actor: Actor{Kind: "User", Name: "alice"}, Action: "blocking.toggle", Resource: Resource{Kind: "blocking"}},
		{Time: t0.Add(time.Minute), Actor: Actor{Kind: "User", Name: "bob"}, Action: "zones.write", Resource: Resource{Kind: "DNSZone", Name: "home"}},
		{Time: t0.Add(2 * time.Minute), Actor: Actor{Kind: "User", Name: "alice"}, Action: "zones.delete", Resource: Resource{Kind: "DNSZone", Name: "home"}, Result: ResultDenied},
		{Time: t0.Add(3 * time.Minute), Actor: Actor{Kind: "User", Name: "carol"}, Action: "cache.flush", Resource: Resource{Kind: "cache"}},
	} {
		if err := m.Record(ctx, ev); err != nil {
			t.Fatal(i, err)
		}
	}
	all := m.Events()
	if len(all) != 3 || all[0].ID != 2 {
		t.Fatalf("ring kept %+v", all)
	}
	evs, _ := m.Read(ctx, Query{Kind: "DNSZone"})
	if len(evs) != 2 {
		t.Fatalf("kind filter %d", len(evs))
	}
	evs, _ = m.Read(ctx, Query{Actor: "alice"})
	if len(evs) != 1 || evs[0].Action != "zones.delete" {
		t.Fatalf("actor filter %+v", evs)
	}
	evs, _ = m.Read(ctx, Query{Actor: "User:bob"})
	if len(evs) != 1 {
		t.Fatalf("qualified actor filter %+v", evs)
	}
	evs, _ = m.Read(ctx, Query{AfterID: 3, Limit: 1})
	if len(evs) != 1 || evs[0].ID != 4 {
		t.Fatalf("cursor %+v", evs)
	}
	evs, _ = m.Read(ctx, Query{Since: t0.Add(150 * time.Second)})
	if len(evs) != 1 {
		t.Fatalf("since %+v", evs)
	}
	evs, _ = m.Read(ctx, Query{Desc: true, Limit: 2})
	if len(evs) != 2 || evs[0].ID != 4 || evs[1].ID != 3 {
		t.Fatalf("desc %+v", evs)
	}
	evs, _ = m.Read(ctx, Query{Desc: true, BeforeID: 3})
	if len(evs) != 1 || evs[0].ID != 2 {
		t.Fatalf("desc cursor %+v", evs)
	}
}

type failing struct{}

func (failing) Record(context.Context, Event) error { return errors.New("disk full") }

func TestMultiAndLog(t *testing.T) {
	var buf bytes.Buffer
	mem := &Memory{}
	m := Multi{Log{Logger: slog.New(slog.NewJSONHandler(&buf, nil))}, nil, failing{}, mem}
	err := m.Record(context.Background(), Event{Action: "cache.flush", Result: ResultSuccess, Actor: Actor{Kind: "User", Name: "alice"}})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("joined error %v", err)
	}
	if len(mem.Events()) != 1 {
		t.Fatal("later recorders skipped after an error")
	}
	if !strings.Contains(buf.String(), `"log":"audit"`) || !strings.Contains(buf.String(), `"actor":"User:alice"`) {
		t.Fatalf("log line %s", buf.String())
	}
	if evs, err := m.Read(context.Background(), Query{}); err != nil || len(evs) != 1 {
		t.Fatalf("multi read %v %d", err, len(evs))
	}
	if _, err := (Multi{Log{}}).Read(context.Background(), Query{}); !errors.Is(err, ErrNotStored) {
		t.Fatalf("read without a store: %v", err)
	}
}

func TestGated(t *testing.T) {
	mem := &Memory{}
	allowed := false
	g := Gated{Inner: mem, Allowed: func() bool { return allowed }}
	ctx := context.Background()
	if err := g.Record(ctx, Event{Action: "a"}); err != nil || len(mem.Events()) != 0 {
		t.Fatalf("recorded while not allowed: %v %d", err, len(mem.Events()))
	}
	if _, err := g.Read(ctx, Query{}); !errors.Is(err, ErrNotStored) {
		t.Fatalf("read while not allowed: %v", err)
	}
	allowed = true
	if err := g.Record(ctx, Event{Action: "b"}); err != nil || len(mem.Events()) != 1 {
		t.Fatalf("not recorded while allowed: %v %d", err, len(mem.Events()))
	}
	if evs, err := g.Read(ctx, Query{}); err != nil || len(evs) != 1 {
		t.Fatalf("read while allowed: %v %d", err, len(evs))
	}
	// A recorder that cannot read back never claims to.
	if _, err := (Gated{Inner: Log{}}).Read(ctx, Query{}); !errors.Is(err, ErrNotStored) {
		t.Fatalf("write-only inner: %v", err)
	}
}

func TestDocumentPreparationPreservesAbsenceAndRedactsBeforeCap(t *testing.T) {
	for _, kind := range []string{"APIToken", "LocalUser", "DNSRecordSet", "runtime"} {
		var absent json.RawMessage
		if Document(kind, absent) != nil {
			t.Fatal("typed nil document invented state")
		}
		ev := PrepareDocuments(Event{Resource: Resource{Kind: kind}})
		if ev.Before != nil || ev.After != nil {
			t.Fatal("new event preparation invented absent documents")
		}
	}
	raw := json.RawMessage(`{"password":"` + strings.Repeat("fixture-credential", MaxDiff) + `","safe":"record evidence"}`)
	safe := Document("DNSRecordSet", raw)
	if string(safe) != `{"safe":"record evidence"}` {
		t.Fatal("original secret size affected capped safe evidence")
	}
}
