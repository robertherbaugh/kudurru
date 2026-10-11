package fuzz

import (
	"context"
	"net/netip"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
)

// queryLogFixture opens a querylog.db with a handful of rows.
func queryLogFixture(f *testing.F) *sqlitelog.Reader {
	f.Helper()
	ctx := context.Background()
	w, err := sqlitelog.Open(ctx, sqlitelog.Options{Path: filepath.Join(f.TempDir(), sqlitelog.FileName), Node: "fuzz", Privacy: querylog.PrivacyFull})
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { _ = w.Close() })
	now := time.Now().Add(-time.Minute)
	rows := []struct {
		client, name, upstream, zone, list string
		status                             pipeline.Status
	}{
		{"192.0.2.10", "www.example.com.", "1.1.1.1:53", "", "", pipeline.StatusForwarded},
		{"192.0.2.11", "ads.tracker.example.", "", "", "default/ads", pipeline.StatusBlockedList},
		{"2001:db8::1", "Host.Home.Example.", "", "home.example.", "", pipeline.StatusAuthoritative},
		{"10.0.0.5", "it's-a_%weird*name.example.", "9.9.9.9:53", "", "", pipeline.StatusCached},
	}
	var batch []*querylog.Event
	for i, r := range rows {
		batch = append(batch, &querylog.Event{
			ID: uint64(i + 1), Time: now.Add(time.Duration(i) * time.Second), Node: "fuzz", Proto: pipeline.ProtoUDP,
			Client: netip.MustParseAddr(r.client), ClientName: "client-" + r.client, Name: r.name, QType: 1, QClass: 1,
			Status: r.status, Upstream: r.upstream, Zone: r.zone, List: r.list,
		})
	}
	if err := w.Write(batch); err != nil {
		f.Fatal(err)
	}
	if err := w.Flush(ctx); err != nil {
		f.Fatal(err)
	}
	r := w.Reader()
	page, err := r.Queries(ctx, sqlitelog.QueryFilter{Domain: "*.example"})
	if err != nil || len(page.Queries) != 3 {
		f.Fatalf("fixture: *.example matched %v (%v), want 3 rows", page, err)
	}
	return r
}

// FuzzQueryLogFilter runs GET /queries filters (client, domain, upstream,
// zone, list, status class, type, cursor, limit) against a small
// querylog.db. Queries must not panic or hit an SQL error (the filters are
// bound parameters), and an exact or wildcard domain filter returns only
// matching rows.
func FuzzQueryLogFilter(f *testing.F) {
	r := queryLogFixture(f)
	f.Add("", "www.example.com", "", "", "", "", uint16(0), "", 0)
	f.Add("192.0.2.0/24", "*.example", "1.1.1.1:53", "", "", "permitted", uint16(1), "3", 1)
	f.Add("client-*", "it's-a_%weird*", "", "home.example.", "default/ads", "blocked", uint16(28), "18446744073709551615", 1000)
	f.Add("'; DROP TABLE query; --", "%", "\\", "\x00", "*", "error", uint16(65535), "-1", -5)
	f.Add("2001:db8::/32", "HOST.home.example.", "", "", "", "cached", uint16(0), "abc", 1<<30)
	f.Fuzz(func(t *testing.T, client, domain, upstream, zone, list, class string, qtype uint16, cursor string, limit int) {
		filter := sqlitelog.QueryFilter{
			Client: client, Domain: domain, Upstream: upstream, Zone: zone, List: list,
			StatusClass: pipeline.Class(class), QType: qtype, Cursor: cursor, Limit: limit,
		}
		page, err := r.Queries(context.Background(), filter)
		if err != nil {
			if msg := strings.ToLower(err.Error()); strings.Contains(msg, "sql logic error") || strings.Contains(msg, "syntax error") {
				t.Fatalf("filter reached SQL unescaped: %v (%+v)", err, filter)
			}
			return
		}
		d := strings.TrimSpace(domain)
		if d == "" {
			return
		}
		want := strings.TrimSuffix(strings.ToLower(d), ".")
		for _, row := range page.Queries {
			got := strings.TrimSuffix(strings.ToLower(row.Domain), ".")
			if strings.Contains(d, "*") {
				if ok, err := path.Match(globEscape(want), got); err == nil && !ok {
					t.Fatalf("domain filter %q returned %q", domain, row.Domain)
				}
			} else if got != want {
				t.Fatalf("domain filter %q returned %q", domain, row.Domain)
			}
		}
	})
}

// globEscape turns a query-log wildcard (only * is special) into a
// path.Match pattern.
func globEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `?`, `\?`, `[`, `\[`, `/`, "\x00")
	return r.Replace(s)
}

// FuzzQueryLogNames parses the query-type and status names the API
// accepts, and classifies special-use names for the SUDN exclusion.
func FuzzQueryLogNames(f *testing.F) {
	for _, s := range []string{"A", "aaaa", "TYPE65534", "type0", "blocked_list", "servfail_unsynced", "", "localhost.", "1.168.192.in-addr.arpa.", "x.local", "home.arpa"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if qt, ok := querylog.ParseQType(s); ok {
			back, ok2 := querylog.ParseQType(querylog.QTypeName(qt))
			if !ok2 || back != qt {
				t.Fatalf("qtype %q -> %d -> %q -> %d", s, qt, querylog.QTypeName(qt), back)
			}
		}
		if st, ok := querylog.ParseStatus(s); ok {
			if back, ok2 := querylog.ParseStatus(st.String()); !ok2 || back != st {
				t.Fatalf("status %q -> %v -> %q", s, st, st.String())
			}
		}
		_ = querylog.IsSpecialUse(s)
	})
}
