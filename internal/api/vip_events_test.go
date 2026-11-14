package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/rbac"
)

type vipTestAuthorizer struct {
	inner  rbac.Authorizer
	denied atomic.Bool
	calls  atomic.Int64
}

func (a *vipTestAuthorizer) Authorize(ctx context.Context, p rbac.Principal, attrs rbac.Attributes) (rbac.Decision, error) {
	a.calls.Add(1)
	if a.denied.Load() {
		return rbac.Decision{}, nil
	}
	return a.inner.Authorize(ctx, p, attrs)
}

func vipTestFrame(reader *bufio.Reader) (string, string, error) {
	var event, data string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", "", err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			return event, data, nil
		}
		if value, ok := strings.CutPrefix(line, "event: "); ok {
			event = value
		}
		if value, ok := strings.CutPrefix(line, "data: "); ok {
			data = value
		}
	}
}

func TestVIPNotificationStreamActualRouterPermissionsAndRevocation(t *testing.T) {
	hub := &VIPEventHub{}
	var authorizer *vipTestAuthorizer
	h := newHarness(t, func(o *Options) {
		o.VIPEvents = hub
		authorizer = &vipTestAuthorizer{inner: o.Authorizer}
		o.Authorizer = authorizer
	})
	h.do(http.MethodGet, "/api/v1/edge/vip/stream", "", nil).want(http.StatusUnauthorized)
	h.do(http.MethodGet, "/api/v1/edge/vip/stream", "zone-admin", nil).want(http.StatusForbidden)
	h.do(http.MethodGet, "/api/v1/edge/vip/stream?cursor=old", "admin", nil).want(http.StatusBadRequest)
	replay := httptest.NewRequest(http.MethodGet, "/api/v1/edge/vip/stream", nil)
	replay.Header.Set("Authorization", "Bearer test:admin")
	replay.Header.Set("Last-Event-ID", "old")
	replayResponse := httptest.NewRecorder()
	h.srv.ServeHTTP(replayResponse, replay)
	if replayResponse.Code != http.StatusBadRequest {
		t.Fatal("Last-Event-ID silently admitted unsupported replay")
	}
	server := httptest.NewServer(h.srv)
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/v1/edge/vip/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer test:admin")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal("stream did not open")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatal("authorized SSE route failed")
	}
	reader := bufio.NewReader(response.Body)
	if _, _, err := vipTestFrame(reader); err != nil {
		t.Fatal("missing bounded connected frame")
	}
	hub.Publish(VIPStreamEvent{Node: "node-a", VIP: "dns", VRID: 53, Previous: "BACKUP", State: "MASTER", Session: "source-a", Sequence: 2, At: time.Now(), ExpiresAt: time.Now().Add(time.Minute)})
	name, data, err := vipTestFrame(reader)
	var event VIPStreamEvent
	if err != nil || name != "vip.master_changed" || json.Unmarshal([]byte(data), &event) != nil || event.VRID != 53 || event.Previous != "BACKUP" || event.Node != "node-a" {
		t.Fatal("typed transition changed at actual router")
	}
	before := authorizer.calls.Load()
	authorizer.denied.Store(true)
	hub.Publish(VIPStreamEvent{Node: "node-a", VIP: "dns", State: "FAULT", At: time.Now(), ExpiresAt: time.Now().Add(time.Minute)})
	if _, _, err := vipTestFrame(reader); err == nil || authorizer.calls.Load() <= before {
		t.Fatal("stream used cached route permission after refusal")
	}
}

func TestVIPNotificationHubBoundsCancellationAndNoReplay(t *testing.T) {
	hub := &VIPEventHub{}
	type subscription struct {
		events  <-chan VIPStreamEvent
		release func()
	}
	subs := []subscription{}
	for range 16 {
		events, release, err := hub.Subscribe(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		subs = append(subs, subscription{events, release})
		defer release()
	}
	if _, _, err := hub.Subscribe(t.Context()); err == nil {
		t.Fatal("unbounded subscriptions admitted")
	}
	for range 32 {
		if hub.Publish(VIPStreamEvent{}) != 0 {
			t.Fatal("bounded queue prematurely dropped")
		}
	}
	if hub.Publish(VIPStreamEvent{}) != 16 {
		t.Fatal("slow subscribers were not explicitly disconnected")
	}
	for _, sub := range subs {
		for range sub.events {
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	events, release, err := hub.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	select {
	case <-events:
		t.Fatal("new subscription replayed history")
	default:
	}
	cancel()
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("canceled stream retained notification")
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not release subscription")
	}
	if _, _, err := hub.Subscribe(ctx); err == nil {
		t.Fatal("canceled acquisition admitted")
	}
	hub.SetAvailable(false)
	if _, _, err := hub.Subscribe(t.Context()); err == nil {
		t.Fatal("unavailable source admitted new subscriber")
	}
	hub.SetAvailable(true)
	_, release, err = hub.Subscribe(t.Context())
	if err != nil {
		t.Fatal("source recovery did not restore bounded admission")
	}
	release()
}

func TestVIPNotificationStreamDisabledAndCanceled(t *testing.T) {
	h := newHarness(t)
	h.do(http.MethodGet, "/api/v1/edge/vip/stream", "admin", nil).want(http.StatusServiceUnavailable)
	hub := &VIPEventHub{}
	h = newHarness(t, func(o *Options) { o.VIPEvents = hub })
	server := httptest.NewServer(h.srv)
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/v1/edge/vip/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer test:admin")
	response, err := server.Client().Do(r)
	if err != nil {
		t.Fatal("cancel qualification could not open stream")
	}
	cancel()
	_, err = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err == nil {
		t.Fatal("client cancellation did not end body")
	}
}

func TestVIPNotificationStreamOriginalSourceExpiry(t *testing.T) {
	hub := &VIPEventHub{}
	h := newHarness(t, func(o *Options) { o.VIPEvents = hub })
	server := httptest.NewServer(h.srv)
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/v1/edge/vip/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer test:admin")
	response, err := server.Client().Do(r)
	if err != nil {
		t.Fatal("source expiry qualification could not connect")
	}
	defer func() { _ = response.Body.Close() }()
	reader := bufio.NewReader(response.Body)
	if _, _, err := vipTestFrame(reader); err != nil {
		t.Fatal("missing stream connection")
	}
	hub.Publish(VIPStreamEvent{Node: "node-a", VIP: "dns", State: "MASTER", At: time.Now().Add(-2 * time.Second), ExpiresAt: time.Now().Add(-time.Second)})
	if _, _, err := vipTestFrame(reader); err == nil {
		t.Fatal("queued stale source event gained a fresh publication deadline")
	}
}
