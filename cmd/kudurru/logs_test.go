package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestLogsPages(t *testing.T) {
	h := loggedIn(t)
	golden(t, h, "logs", h.mustRun("logs", "--limit", "5"))
	// Pages of two rows: the CLI followed cursor_next twice.
	got := h.api.requested("GET /api/v1/queries")
	if len(got) != 3 || !strings.Contains(got[1], "cursor=4") || !strings.Contains(got[2], "cursor=2") {
		t.Fatalf("query requests %v", got)
	}
	golden(t, h, "logs-wide-filtered", h.mustRun("logs", "--client", "192.168.1.24", "-o", "wide", "--since", "1h"))
	if got := h.api.requested("GET /api/v1/queries?client=192.168.1.24"); len(got) != 1 || !strings.Contains(got[0], "since=2026-10-11T11%3A00%3A00Z") {
		t.Fatalf("filter request %v", h.api.requested("GET /api/v1/queries"))
	}
	out := h.mustRun("logs", "--limit", "3", "-o", "json")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("ndjson lines: %d\n%s", len(lines), out)
	}
	for _, l := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil || m["domain"] == nil {
			t.Fatalf("ndjson line %q: %v", l, err)
		}
	}
	if _, _, err := h.run("logs", "--since", "yesterday"); err == nil {
		t.Fatal("bad --since accepted")
	}
}

// sseEvent formats one SSE event.
func sseEvent(id int, data string) string {
	return fmt.Sprintf("id: %d\nevent: query\ndata: %s\n\n", id, data)
}

func TestLogsFollow(t *testing.T) {
	h := loggedIn(t)
	row := func(id int, domain, status string) string {
		return fmt.Sprintf(`{"id":%d,"ts":"2026-10-11T12:00:%02dZ","node":"node-a","client":{"ip":"192.168.1.30"},"domain":%q,"qtype":"AAAA","status":%q,"rcode":"NOERROR","reply":{"time_ms":2}}`, id, id, domain, status)
	}
	h.api.stream = [][]string{
		// First connection: one duplicate of the last paged row (id 5), one
		// new row, one filtered out, then the server drops the stream.
		{sseEvent(5, row(5, "host5.example.com", "forwarded")), sseEvent(6, row(6, "cdn.example.net", "cached")), sseEvent(7, row(7, "other.example.org", "forwarded")),
			": keep-alive\n\n", "event: notice\ndata: {\"lagged\":3}\n\n"},
		// Second connection (resumed with Last-Event-ID 7): the live tail's
		// event shape (name, numeric qtype and rcode, client string).
		{sseEvent(8, `{"id":8,"ts":"2026-10-11T12:00:08Z","node":"node-a","client":"192.168.1.24","name":"ads.example.net.","qtype":1,"status":"blocked_list","rcode":0,"list":"ads","rule":"||ads.example.net^","latency":250000}`)},
	}
	var out, errOut syncBuffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- h.runCtx(ctx, &out, &errOut, "logs", "--follow", "--limit", "1", "--domain", "*.example.net")
	}()
	deadline := time.After(8 * time.Second)
	for !strings.Contains(out.String(), "ads.example.net") {
		select {
		case err := <-done:
			t.Fatalf("logs --follow ended early: %v\n%s\n%s", err, out.String(), errOut.String())
		case <-deadline:
			t.Fatalf("no streamed row arrived:\n%s\n%s", out.String(), errOut.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("logs --follow after cancel: %v", err)
	}
	golden(t, h, "logs-follow", out.String())
	h.api.mu.Lock()
	ids := append([]string(nil), h.api.lastIDs...)
	h.api.mu.Unlock()
	if len(ids) != 2 || ids[0] != "5" || ids[1] != "7" {
		t.Fatalf("Last-Event-ID per connection = %q, want [5 7]", ids)
	}
	if q := h.api.requested("GET /api/v1/queries/stream"); len(q) != 2 || !strings.Contains(q[0], "domain=%2A.example.net") {
		t.Fatalf("stream requests %v", q)
	}
}

func TestLogsFollowStreamUnauthorized(t *testing.T) {
	h := loggedIn(t)
	h.api.mu.Lock()
	h.api.tokens = map[string]string{}
	h.api.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out, errOut syncBuffer
	err := h.runCtx(ctx, &out, &errOut, "logs", "-f", "--limit", "0")
	if err == nil || !strings.Contains(err.Error(), "unauthenticated") {
		t.Fatalf("err = %v", err)
	}
}
