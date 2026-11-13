package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/store"
)

type memberForwardAnswer struct {
	status int
	body   []byte
	err    error
}

// refusal decodes the error envelope of a refused single-item route.
func (a memberForwardAnswer) refusal() (key, message string) {
	var envelope struct {
		Error struct {
			Key     string `json:"key"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(a.body, &envelope)
	return envelope.Error.Key, envelope.Error.Message
}

func memberForwardGroup(name, address string) map[string]any {
	return map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": "ClientGroup", "metadata": map[string]any{"name": name, "namespace": "default"},
		"spec": map[string]any{"clients": []any{map[string]any{"ip": address}}}}
}

// exchange sends one public request to the follower over its actual listener.
func (f *memberForwardHTTPFixture) exchange(ctx context.Context, method, path string, body any) memberForwardAnswer {
	var encoded []byte
	if body != nil {
		var err error
		if encoded, err = json.Marshal(body); err != nil {
			return memberForwardAnswer{err: err}
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, f.live.URL+api.BasePath+path, bytes.NewReader(encoded))
	if err != nil {
		return memberForwardAnswer{err: err}
	}
	request.GetBody = nil
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Authorization", "Bearer "+f.primary.token)
	response, err := f.live.Client().Do(request)
	if err != nil {
		return memberForwardAnswer{err: err}
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	return memberForwardAnswer{response.StatusCode, raw, err}
}

// A follower request whose 30-second lifetime ended before an item was sent
// stays a forward. Every forwarded route answers `unavailable` with that cause
// and says that nothing was applied; none answers the local command's "current
// credential is invalid or expired", and nothing reaches the primary.
func TestRootMemberForwardAfterLifetimeEndedIsRefusedAsAForward(t *testing.T) {
	f := newMemberForwardHTTPFixture(t, true)
	primary := f.primary.s.opts.Store
	ctx, cancel := context.WithTimeout(t.Context(), memberForwardLifetime+45*time.Second)
	defer cancel()
	// The late batch deletion must leave this object on the primary.
	if kept := f.exchange(ctx, http.MethodPut, "/clientgroups/default/late-kept", memberForwardGroup("late-kept", "192.0.2.89")); kept.err != nil || kept.status != http.StatusCreated {
		t.Fatalf("forwarded write before the held requests: status=%d err=%v body=%s", kept.status, kept.err, kept.body)
	}
	type held struct {
		hold     *memberForwardBodyHold
		finished chan memberForwardAnswer
	}
	// Each request stops at its first body read, after the follower has
	// started the request's forwarding lifetime.
	start := func(method, path string, body any) held {
		t.Helper()
		h := held{&memberForwardBodyHold{entered: make(chan struct{}), release: make(chan struct{})}, make(chan memberForwardAnswer, 1)}
		f.hold.Store(h.hold)
		go func() { h.finished <- f.exchange(ctx, method, path, body) }()
		select {
		case <-h.hold.entered:
		case answer := <-h.finished:
			t.Fatalf("%s %s answered before its body was read: status=%d err=%v body=%s", method, path, answer.status, answer.err, answer.body)
		case <-ctx.Done():
			t.Fatal("request did not reach its body read", method, path)
		}
		return h
	}
	apply := start(http.MethodPost, "/apply", []any{memberForwardGroup("late-a", "192.0.2.90"), memberForwardGroup("late-b", "192.0.2.91")})
	single := start(http.MethodPut, "/clientgroups/default/late-c", memberForwardGroup("late-c", "192.0.2.92"))
	batch := start(http.MethodPost, "/clientgroups:batchDelete", map[string]any{"names": []string{"late-kept"}})
	record := start(http.MethodPost, "/zones/late-zone/records", map[string]any{"dnsName": "www.late.test.", "recordType": "A", "targets": []any{"192.0.2.93"}})
	select {
	case <-time.After(memberForwardLifetime + time.Second):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	answers := map[string]memberForwardAnswer{}
	for name, h := range map[string]held{"apply": apply, "single": single, "batch": batch, "record": record} {
		close(h.hold.release)
		select {
		case answers[name] = <-h.finished:
		case <-ctx.Done():
			t.Fatal("held request did not join", name)
		}
		if answers[name].err != nil {
			t.Fatal(name, answers[name].err)
		}
	}
	const cause, effect = "30-second forwarding lifetime ended", "not applied"
	var applied api.List[api.ApplyResult]
	if got := answers["apply"]; got.status != http.StatusOK || json.Unmarshal(got.body, &applied) != nil || len(applied.Items) != 2 {
		t.Fatalf("apply after its lifetime ended lost its per-item answer: status=%d body=%s", got.status, got.body)
	}
	for _, item := range applied.Items {
		if item.Result != api.ResultError || item.ErrorKey != api.KeyUnavailable || !strings.Contains(item.Error, cause) || !strings.Contains(item.Error, effect) || item.ResourceVersion != "" {
			t.Errorf("apply item after the lifetime ended: %+v", item)
		}
	}
	for _, name := range []string{"single", "record"} {
		got := answers[name]
		if key, message := got.refusal(); got.status != http.StatusServiceUnavailable || key != api.KeyUnavailable || !strings.Contains(message, cause) || !strings.Contains(message, effect) {
			t.Errorf("%s route after the lifetime ended: status=%d body=%s", name, got.status, got.body)
		}
	}
	var removed api.Processed
	if got := answers["batch"]; got.status != http.StatusOK || json.Unmarshal(got.body, &removed) != nil || len(removed.Processed.Success) != 0 || len(removed.Processed.Errors) != 1 ||
		!strings.Contains(removed.Processed.Errors[0].Error, api.KeyUnavailable) || !strings.Contains(removed.Processed.Errors[0].Error, cause) || !strings.Contains(removed.Processed.Errors[0].Error, effect) {
		t.Errorf("batch deletion after the lifetime ended: status=%d body=%s", got.status, got.body)
	}
	for name, got := range answers {
		if bytes.Contains(got.body, []byte("credential")) || bytes.Contains(got.body, []byte(api.KeyUnauthenticated)) {
			t.Errorf("%s reported an ended lifetime as a credential failure: %s", name, got.body)
		}
	}
	for _, name := range []string{"late-a", "late-b", "late-c"} {
		if err := primary.Get(t.Context(), store.Key{Kind: "ClientGroup", Namespace: "default", Name: name}, &public.ClientGroup{}); !errors.Is(err, store.ErrNotFound) {
			t.Error("an item after the lifetime ended reached the primary", name, err)
		}
	}
	if err := primary.Get(t.Context(), store.Key{Kind: "ClientGroup", Namespace: "default", Name: "late-kept"}, &public.ClientGroup{}); err != nil {
		t.Error("a batch deletion after the lifetime ended reached the primary", err)
	}
	if err := primary.Get(t.Context(), store.Key{Kind: "DNSRecordSet", Namespace: "default", Name: "api-late-zone"}, &public.DNSRecordSet{}); !errors.Is(err, store.ErrNotFound) {
		t.Error("a zone command after the lifetime ended reached the primary", err)
	}
}

// A follower request that was canceled before its write was sent is refused
// as a forward too. The recorder stands in for a client that is still
// listening; an actual client has gone away by then.
func TestRootMemberForwardCanceledRequestIsRefusedAsAForward(t *testing.T) {
	f := newMemberForwardHTTPFixture(t, false)
	document, err := json.Marshal(memberForwardGroup("canceled", "192.0.2.94"))
	if err != nil {
		t.Fatal(err)
	}
	hold := &memberForwardBodyHold{entered: make(chan struct{}), release: make(chan struct{})}
	f.hold.Store(hold)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request := httptest.NewRequest(http.MethodPut, api.BasePath+"/clientgroups/default/canceled", bytes.NewReader(document)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+f.primary.token)
	recorder := httptest.NewRecorder()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		f.live.Config.Handler.ServeHTTP(recorder, request)
	}()
	select {
	case <-hold.entered:
	case <-finished:
		t.Fatalf("request answered before its body was read: status=%d body=%s", recorder.Code, recorder.Body)
	case <-time.After(20 * time.Second):
		t.Fatal("request did not reach its body read")
	}
	cancel()
	close(hold.release)
	select {
	case <-finished:
	case <-time.After(20 * time.Second):
		t.Fatal("canceled request did not join")
	}
	got := memberForwardAnswer{status: recorder.Code, body: recorder.Body.Bytes()}
	if key, message := got.refusal(); got.status != http.StatusServiceUnavailable || key != api.KeyUnavailable ||
		!strings.Contains(message, "the request was canceled before this write was sent") || !strings.Contains(message, "not applied") {
		t.Fatalf("canceled follower request: status=%d body=%s", got.status, got.body)
	}
	if err := f.primary.s.opts.Store.Get(t.Context(), store.Key{Kind: "ClientGroup", Namespace: "default", Name: "canceled"}, &public.ClientGroup{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a canceled request reached the primary", err)
	}
}

// A forwarded write that the primary committed and answered, but whose answer
// the follower can no longer relay, answers 503 completion_unknown and tells
// the caller to inspect the object: it was applied. It never answers
// unavailable, which says that nothing was made, and nothing is sent again
// (HA-OPEN5 finding 6, D368). The follower holds its Store lock from the
// forward's acquisition through the relay (WithMemberForward), so it cannot
// install a newer commit in between; what can end the relay is a pinned
// dependency or the request's own lifetime. Here the 30-second forwarding
// lifetime ends while the follower holds the primary's answer, so this test
// covers objectForwardError's deadline answer only. Its answer for a source
// change after the primary answered (sent with ErrCAS or ErrFenced) has no
// cause this fixture can produce while the follower holds that lock, and is
// pinned by TestObjectForwardErrorAfterTheAnswerIsUnknown alone.
func TestRootMemberForwardAnswerNotRelayedIsUnknown(t *testing.T) {
	f := newMemberForwardHTTPFixture(t, true)
	primary := f.primary.s.opts.Store
	ctx, cancel := context.WithTimeout(t.Context(), memberForwardLifetime+30*time.Second)
	defer cancel()
	relays := 0
	f.follower.memberRelay = func(scope context.Context) {
		relays++
		<-scope.Done()
	}
	got := f.exchange(ctx, http.MethodPut, "/clientgroups/default/relayed", memberForwardGroup("relayed", "192.0.2.91"))
	f.follower.memberRelay = nil
	if got.err != nil {
		t.Fatal(got.err)
	}
	key, message := got.refusal()
	if got.status != http.StatusServiceUnavailable || key != "completion_unknown" || !strings.Contains(message, "outcome is unknown") ||
		!strings.Contains(message, "inspect the object") || !strings.Contains(message, "do not replay") {
		t.Fatalf("answer not relayed: status=%d key=%q message=%q", got.status, key, message)
	}
	if relays != 1 {
		t.Fatal("the forwarded write was relayed more than once", relays)
	}
	if err := primary.Get(t.Context(), store.Key{Kind: "ClientGroup", Namespace: "default", Name: "relayed"}, &public.ClientGroup{}); err != nil {
		t.Fatal("the write the primary answered is not on the primary", err)
	}
}
