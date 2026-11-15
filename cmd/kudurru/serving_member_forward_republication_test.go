package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// A follower publishes its installed source again, without a new install,
// when a Wake asks for a new preparation of the same cut: after every pull on
// a node with managed root anchors the anchor source wakes the publisher while
// the install's own preparation runs, and since D405 that Wake is answered by
// one more publication of the same generation instead of canceling the first.
// A forwarded request captured before that publication still has its source:
// the follower's pull is held (D375) and the primary authorizes every item in
// its own transaction. Its items are forwarded and committed; none answers
// "the enrolled primary is unavailable" (PUB-FOLLOWER, the cut that failed
// every enrolled-cluster e2e test after D405).
func TestRootMemberForwardAcrossSameSourceRepublication(t *testing.T) {
	f := newMemberForwardHTTPFixture(t, true)
	primary := f.primary.s.opts.Store
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	// republish waits until the follower serves a new publication of the
	// very source it served before, as the coalesced Wake produces.
	republish := func(what string) {
		t.Helper()
		before := f.follower.publisher.Status()
		if !before.ProtectedReady {
			t.Fatalf("%s: the follower serves no protected publication: %+v", what, before)
		}
		f.follower.publisher.Wake()
		for {
			changed := f.follower.publisher.Subscribe()
			now := f.follower.publisher.Status()
			if now.ProtectedReady && now.Served.Publication > before.Served.Publication {
				if !now.Served.Base.Same(before.Served.Base) {
					t.Fatalf("%s: the follower installed a new source instead of publishing the same one again: %+v then %+v", what, before.Served, now.Served)
				}
				return
			}
			select {
			case <-changed:
			case <-ctx.Done():
				t.Fatalf("%s: the follower did not publish its source again: %+v", what, now)
			}
		}
	}
	type held struct {
		hold     *memberForwardBodyHold
		finished chan memberForwardAnswer
	}
	// Each request captures its publication and stops at its first body read;
	// the follower then publishes the same source again before the request
	// forwards anything.
	send := func(method, path string, body any) memberForwardAnswer {
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
		republish(method + " " + path)
		close(h.hold.release)
		select {
		case answer := <-h.finished:
			if answer.err != nil {
				t.Fatal(method, path, answer.err)
			}
			return answer
		case <-ctx.Done():
			t.Fatal("held request did not join", method, path)
		}
		return memberForwardAnswer{}
	}
	if got := send(http.MethodPut, "/clientgroups/default/republished-single", memberForwardGroup("republished-single", "192.0.2.95")); got.status != http.StatusCreated {
		t.Fatalf("a single write after the follower published its source again: status=%d body=%s", got.status, got.body)
	}
	names := []string{"republished-a", "republished-b", "republished-c"}
	var documents []any
	for i, name := range names {
		documents = append(documents, memberForwardGroup(name, fmt.Sprintf("192.0.2.%d", 60+i)))
	}
	got := send(http.MethodPost, "/apply", documents)
	var applied api.List[api.ApplyResult]
	if got.status != http.StatusOK || json.Unmarshal(got.body, &applied) != nil || len(applied.Items) != len(names) {
		t.Fatalf("apply after the follower published its source again: status=%d body=%s", got.status, got.body)
	}
	for i, item := range applied.Items {
		if item.Result != api.ResultCreated || item.ErrorKey != "" || item.Error != "" || item.ResourceVersion == "" {
			t.Errorf("apply item %d after the follower published its source again was not created: %+v", i, item)
		}
	}
	for _, name := range append([]string{"republished-single"}, names...) {
		if err := primary.Get(t.Context(), store.Key{Kind: "ClientGroup", Namespace: "default", Name: name}, &public.ClientGroup{}); err != nil {
			t.Errorf("%s is not on the primary: %v", name, err)
		}
	}
}
