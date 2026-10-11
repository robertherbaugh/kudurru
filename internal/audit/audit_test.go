package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
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

func TestSQLiteRecorder(t *testing.T) {
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	r := SQLite{Store: st}
	ctx := context.Background()
	ev := Event{
		Time: time.Now(), Actor: Actor{Kind: "User", Name: "alice"}, Action: "blocking.toggle",
		Resource: Resource{Kind: "blocking"}, Result: ResultSuccess, Authz: "binding",
		Before: Diff(map[string]bool{"blocking": true}), After: Diff(map[string]bool{"blocking": false}),
		Request: Request{ID: "req-1", IP: "192.0.2.1"},
	}
	if err := r.Record(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if err := r.Record(ctx, Event{Time: time.Now(), Actor: Actor{Kind: "User", Name: "bob"}, Action: "zones.delete", Resource: Resource{Kind: "DNSZone", Name: "x"}, Result: ResultDenied}); err != nil {
		t.Fatal(err)
	}
	evs, err := r.Read(ctx, Query{})
	if err != nil || len(evs) != 2 {
		t.Fatalf("read %v %d", err, len(evs))
	}
	got := evs[0]
	if got.Actor.Kind != "User" || got.Actor.Name != "alice" || got.Manager != "api" || got.Hash == "" ||
		!strings.Contains(got.Reason, "authz=binding") || !strings.Contains(got.Reason, "request=req-1") || string(got.After) != `{"blocking":false}` {
		t.Fatalf("row %+v", got)
	}
	if evs[1].PrevHash != got.Hash {
		t.Fatal("rows not chained")
	}
	evs, _ = r.Read(ctx, Query{Actor: "bob"})
	if len(evs) != 1 || evs[0].Result != ResultDenied {
		t.Fatalf("actor filter %+v", evs)
	}
	if err := st.VerifyAudit(ctx); err != nil {
		t.Fatal(err)
	}
}
