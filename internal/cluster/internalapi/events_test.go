package internalapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/robertherbaugh/kudurru/internal/cluster"
)

func testEvent() Event {
	return Event{Type: "secondary-notify", Identity: "sha256:" + strings.Repeat("a", 64)}
}

func TestPeerEventsGuardAndBoundedBody(t *testing.T) {
	creds, err := NewCredentials(CredentialOptions{Key: key(1), NodeName: "receiver"})
	if err != nil {
		t.Fatal(err)
	}
	var called atomic.Int32
	server, err := NewServer(ServerOptions{Credentials: creds, Event: func(e Event) bool { called.Add(1); return e == testEvent() }})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(testEvent())
	for _, tc := range []struct {
		name, body, token, path string
		want                    int
	}{
		{"valid", string(body), creds.tokensToTry()[0], PathEvents, 202},
		{"unauthenticated", string(body), "wrong", PathEvents, 401},
		{"bad type", strings.Replace(string(body), "secondary-notify", "unknown", 1), creds.tokensToTry()[0], PathEvents, 400},
		{"bad identity", `{"type":"secondary-notify","identity":"a"}`, creds.tokensToTry()[0], PathEvents, 400},
		{"unknown field", strings.TrimSuffix(string(body), "}") + `,"records":[]}`, creds.tokensToTry()[0], PathEvents, 400},
		{"trailing object", string(body) + ` {}`, creds.tokensToTry()[0], PathEvents, 400},
		{"oversized", strings.Repeat(" ", 1025) + string(body), creds.tokensToTry()[0], PathEvents, 400},
		{"read route still rejects body", string(body), creds.tokensToTry()[0], PathStatus, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			r.Header.Set("Authorization", "Bearer "+tc.token)
			r.RemoteAddr = "127.0.0.1:4000"
			w := httptest.NewRecorder()
			server.Handler().ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
		})
	}
	if called.Load() != 1 {
		t.Fatalf("invalid hint invoked receiver %d times", called.Load())
	}
	if got := testutil.ToFloat64(server.requests.WithLabelValues("events", "202")); got != 1 {
		t.Fatalf("request counted %f times", got)
	}
}

func TestPeerEventClientAndRotation(t *testing.T) {
	var calls atomic.Int32
	server := startNode(t, CredentialOptions{Key: key(1), NodeName: "old-key-peer"}, ServerOptions{Event: func(Event) bool { calls.Add(1); return true }})
	creds, err := NewCredentials(CredentialOptions{Key: key(2), PreviousKey: key(1), NodeName: "rotated-sender"})
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(creds, ClientOptions{})
	if err := client.SendEvent(t.Context(), server.addr, testEvent()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("delivered %d times", calls.Load())
	}
	// A peer callback only schedules local work. It neither calls SendEvent nor
	// publishes an event to another node; the external NOTIFY path owns fan-out.
	rejected := startNode(t, CredentialOptions{Key: key(1), NodeName: "different-generation"}, ServerOptions{Event: func(Event) bool { return false }})
	if err := client.SendEvent(t.Context(), rejected.addr, testEvent()); err == nil {
		t.Fatal("inapplicable generation accepted")
	}
}

func TestEventFanoutCoalescesAndSkipsSelf(t *testing.T) {
	membership := &cluster.Static{SelfName: "a", Members: []cluster.Peer{{Name: "a", Address: "self"}, {Name: "b", Address: "peer-b"}, {Name: "c", Address: "peer-c"}, {Name: "duplicate", Address: "peer-c"}}}
	f := NewEventFanout(membership, nil, nil, nil)
	calls := make(chan string, 8)
	f.send = func(_ context.Context, address string, _ Event) error { calls <- address; return nil }
	for range 1000 {
		f.Enqueue(testEvent())
	}
	if len(f.queue) != 1 {
		t.Fatalf("duplicate queued %d times", len(f.queue))
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = f.Run(ctx) }()
	defer func() { cancel(); <-done }()
	seen := map[string]bool{}
	for range 2 {
		select {
		case address := <-calls:
			seen[address] = true
		case <-time.After(5 * time.Second):
			t.Fatal("peer not notified")
		}
	}
	if len(seen) != 2 || !seen["peer-b"] || !seen["peer-c"] {
		t.Fatalf("delivered to %+v", seen)
	}
	cancel()
	<-done
	if len(calls) != 0 {
		t.Fatal("duplicate or self delivery")
	}
}
