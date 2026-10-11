package sqlitelog

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	testclock "k8s.io/utils/clock/testing"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
)

// t0 is the fake "now" of most tests: a Sunday noon, UTC.
var t0 = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newWriter opens a writer on a temp file with a fake clock at now.
func newWriter(t testing.TB, now time.Time, mutate ...func(*Options)) (*Writer, *testclock.FakeClock) {
	t.Helper()
	fc := testclock.NewFakeClock(now)
	opts := Options{
		Path:         filepath.Join(t.TempDir(), FileName),
		Node:         "node-a",
		Privacy:      querylog.PrivacyFull,
		RawRetention: 30 * 24 * time.Hour,
		Clock:        fc,
		Logger:       quietLogger(),
	}
	for _, m := range mutate {
		m(&opts)
	}
	w, err := Open(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w, fc
}

// evOpt mutates a test event.
type evOpt func(*querylog.Event)

func withStatus(s pipeline.Status) evOpt { return func(e *querylog.Event) { e.Status = s } }
func withList(l, rule string) evOpt {
	return func(e *querylog.Event) { e.List, e.Rule = l, rule }
}
func withUpstream(u string) evOpt          { return func(e *querylog.Event) { e.Upstream = u } }
func withQType(q uint16) evOpt             { return func(e *querylog.Event) { e.QType = q } }
func withLatency(d time.Duration) evOpt    { return func(e *querylog.Event) { e.Latency = d } }
func withRcode(rc int) evOpt               { return func(e *querylog.Event) { e.Rcode = rc } }
func withProto(p pipeline.Proto) evOpt     { return func(e *querylog.Event) { e.Proto = p } }
func withZone(z string) evOpt              { return func(e *querylog.Event) { e.Zone = z } }
func withClientName(n string) evOpt        { return func(e *querylog.Event) { e.ClientName = n } }
func withAnswer(a string) evOpt            { return func(e *querylog.Event) { e.Answer = a } }
func withReply(r querylog.ReplyType) evOpt { return func(e *querylog.Event) { e.ReplyType = r } }

// event builds a forwarded A query unless opts say otherwise.
func event(ts time.Time, client, name string, opts ...evOpt) *querylog.Event {
	e := &querylog.Event{
		Time: ts, Latency: 3 * time.Millisecond, Node: "node-a", Proto: pipeline.ProtoUDP,
		Client: netip.MustParseAddr(client), Name: name + ".", QType: 1, QClass: 1,
		Status: pipeline.StatusForwarded, Rcode: 0, ReplyType: querylog.ReplyIP, Answers: 1,
		Upstream: "default",
	}
	for _, o := range opts {
		o(e)
	}
	if isBlocked(e.Status) {
		e.Upstream = ""
	}
	return e
}

func mustWrite(t testing.TB, w *Writer, evs ...*querylog.Event) {
	t.Helper()
	if err := w.Write(evs); err != nil {
		t.Fatal(err)
	}
}

func queryInt(t testing.TB, db *sql.DB, q string, args ...any) int64 {
	t.Helper()
	var n sql.NullInt64
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n.Int64
}

// waitFor polls cond for up to 5 s.
func waitFor(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
