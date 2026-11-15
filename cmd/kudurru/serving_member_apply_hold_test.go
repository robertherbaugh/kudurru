package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// memberApplyDocuments is the size of the file that
// TestRootMemberFollowerApplyIsNotCutByItsOwnPull applies. Under the race
// detector one forwarded document takes about 300 ms here, so 200 documents
// do not fit the request's 30-second lifetime; the process test
// (test/e2e, enrolledFollowerApply) applies 200 without it.
const memberApplyDocuments = 40

// One POST /apply of 40 documents through an enrolled follower whose
// scheduled pull runs every 100 ms applies every document (D375). The
// follower forwards each document as its own member command on the source
// its request captured; its own pull, which installs the request's earlier
// documents, used to change that source and cut every later document
// ("the follower's source changed"; in HA-OPEN6's process runs a 160-document
// file was never applied completely). The request now holds the scheduled
// pull while it runs, so the pull installs the documents after the request.
func TestRootMemberFollowerApplyIsNotCutByItsOwnPull(t *testing.T) {
	f := newMemberForwardHTTPFixture(t, false, memberForwardScheduledPull, func(f *memberForwardHTTPFixture) { f.pullInterval = 100 * time.Millisecond })
	var documents []any
	for i := range memberApplyDocuments {
		documents = append(documents, map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": "ClientGroup",
			"metadata": map[string]any{"name": fmt.Sprintf("held-%03d", i), "namespace": "default"},
			"spec":     map[string]any{"clients": []any{map[string]any{"ip": fmt.Sprintf("198.51.100.%d", i%250+1)}}}})
	}
	body, err := json.Marshal(documents)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, f.live.URL+api.BasePath+"/apply", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+f.primary.token)
	began := time.Now()
	response, err := f.live.Client().Do(request)
	if err != nil {
		t.Fatal("apply through the follower", err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		t.Fatal(err)
	}
	took := time.Since(began)
	var applied api.List[api.ApplyResult]
	if response.StatusCode != http.StatusOK || json.Unmarshal(raw, &applied) != nil || len(applied.Items) != memberApplyDocuments {
		t.Fatalf("apply through the follower: status=%d body=%.512s", response.StatusCode, raw)
	}
	created := 0
	for i, item := range applied.Items {
		if item.Result != api.ResultCreated || item.ErrorKey != "" || item.ResourceVersion == "" {
			t.Fatalf("document %d of %d after %s (%d created before it): %+v", i, memberApplyDocuments, took, created, item)
		}
		created++
	}
	t.Logf("apply of %d documents through the follower: all created in %s", memberApplyDocuments, took)
	for i := range memberApplyDocuments {
		if err := f.primary.s.opts.Store.Get(t.Context(), store.Key{Kind: "ClientGroup", Namespace: "default", Name: fmt.Sprintf("held-%03d", i)}, &public.ClientGroup{}); err != nil {
			t.Fatal("a created document is not on the primary", i, err)
		}
	}
	// The held pull runs after the request and installs every document.
	awaitServingRuntime(t, f.follower, func() bool {
		return f.follower.opts.Store.Get(t.Context(), store.Key{Kind: "ClientGroup", Namespace: "default", Name: fmt.Sprintf("held-%03d", memberApplyDocuments-1)}, &public.ClientGroup{}) == nil
	})
}
