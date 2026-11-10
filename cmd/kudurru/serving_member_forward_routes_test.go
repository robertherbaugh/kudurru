package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// POST /apply, the kind routes and :batchDelete on an enrolled follower reach
// the primary through the same typed object command as the /objects routes:
// one member exchange for each document or name, inside the request's single
// bounded lifetime, with the primary's exact per-item result.
func TestRootMemberForwardApplyKindRoutesAndBatchDelete(t *testing.T) {
	f := newMemberForwardHTTPFixture(t, false)
	primary, follower := f.primary.s.opts.Store, f.follower.opts.Store
	call := func(method, path string, body any) (int, []byte) {
		t.Helper()
		var encoded []byte
		if body != nil {
			var err error
			if encoded, err = json.Marshal(body); err != nil {
				t.Fatal(err)
			}
		}
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		request, err := http.NewRequestWithContext(ctx, method, f.live.URL+api.BasePath+path, bytes.NewReader(encoded))
		if err != nil {
			t.Fatal(err)
		}
		request.GetBody = nil
		if body != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		request.Header.Set("Authorization", "Bearer "+f.primary.token)
		response, err := f.live.Client().Do(request)
		if err != nil {
			t.Fatal("actual follower exchange", method, path, err)
		}
		defer func() { _ = response.Body.Close() }()
		raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, raw
	}
	expect := func(method, path string, body any, want int) []byte {
		t.Helper()
		status, raw := call(method, path, body)
		if status != want {
			t.Fatalf("%s %s through the follower: status=%d want=%d body=%s", method, path, status, want, raw)
		}
		return raw
	}
	exists := func(st *sqlite.Store, obj store.Object, key store.Key) bool {
		t.Helper()
		err := st.Get(t.Context(), key, obj)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
		return err == nil
	}
	meta := func(name string, namespaced bool) map[string]any {
		if namespaced {
			return map[string]any{"name": name, "namespace": "default"}
		}
		return map[string]any{"name": name}
	}
	zone := map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSZone", "metadata": meta("bound", false),
		"spec": map[string]any{"zoneName": "bound.test.", "type": "primary", "nameservers": []any{"ns1.bound.test."},
			"soa": map[string]any{"mname": "ns1.bound.test.", "rname": "hostmaster.bound.test."}}}
	record := map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSRecordSet", "metadata": meta("bound-www", true),
		"spec": map[string]any{"zoneRef": map[string]any{"name": "bound"}, "endpoints": []any{
			map[string]any{"dnsName": "www.bound.test.", "recordType": "A", "targets": []any{"192.0.2.60"}}}}}
	list := func(name, domain string) map[string]any {
		return map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": "BlockList", "metadata": meta(name, true),
			"spec": map[string]any{"mode": "block", "sources": []any{map[string]any{"name": "inline", "inline": []any{domain}}}}}
	}
	group := func(address string) map[string]any {
		return map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": "ClientGroup", "metadata": meta("bound-group", true),
			"spec": map[string]any{"clients": []any{map[string]any{"ip": address}}}}
	}
	before := follower.Authority()

	// kudurru apply: three documents, three member commands, one result each.
	var applied api.List[api.ApplyResult]
	if err := json.Unmarshal(expect(http.MethodPost, "/apply", []any{zone, record, list("bound-list", "blocked.bound.test")}, http.StatusOK), &applied); err != nil || len(applied.Items) != 3 {
		t.Fatal("apply result list", err)
	}
	for _, item := range applied.Items {
		if item.Result != api.ResultCreated || item.ErrorKey != "" || item.Error != "" || item.ResourceVersion == "" {
			t.Fatalf("apply item %s/%s was not committed by the primary: %+v", item.Kind, item.Name, item)
		}
	}
	if !exists(primary, &public.DNSZone{}, store.Key{Kind: "DNSZone", Name: "bound"}) ||
		!exists(primary, &public.DNSRecordSet{}, store.Key{Kind: "DNSRecordSet", Namespace: "default", Name: "bound-www"}) ||
		!exists(primary, &public.BlockList{}, store.Key{Kind: "BlockList", Namespace: "default", Name: "bound-list"}) {
		t.Fatal("applied documents missing on the primary")
	}
	// A refused document keeps its own error beside a committed neighbor.
	invalid := map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSRecordSet", "metadata": meta("bound-invalid", true),
		"spec": map[string]any{"zoneRef": map[string]any{"name": "bound"}, "unknownField": true, "endpoints": []any{
			map[string]any{"dnsName": "invalid.bound.test.", "recordType": "A", "targets": []any{"192.0.2.61"}}}}}
	var mixed api.List[api.ApplyResult]
	if err := json.Unmarshal(expect(http.MethodPost, "/apply", []any{invalid, list("bound-second", "second.bound.test")}, http.StatusOK), &mixed); err != nil || len(mixed.Items) != 2 {
		t.Fatal("mixed apply result list", err)
	}
	if mixed.Items[0].Result != api.ResultError || mixed.Items[0].ErrorKey == "" || mixed.Items[0].ErrorKey == api.KeyUnavailable || mixed.Items[1].Result != api.ResultCreated {
		t.Fatalf("mixed apply lost its per-item answers: %+v", mixed.Items)
	}
	if exists(primary, &public.DNSRecordSet{}, store.Key{Kind: "DNSRecordSet", Namespace: "default", Name: "bound-invalid"}) {
		t.Fatal("a refused document was applied")
	}

	// The dashboard's list, group and policy editors use the kind routes.
	expect(http.MethodPost, "/clientgroups", group("192.0.2.70"), http.StatusCreated)
	var conflict api.Error
	if err := json.Unmarshal(expect(http.MethodPost, "/clientgroups", group("192.0.2.70"), http.StatusConflict), &conflict); err != nil || conflict.Body.Key == "" {
		t.Fatal("create-only conflict was not the primary's relayed refusal", err)
	}
	expect(http.MethodPut, "/clientgroups/default/bound-group", group("192.0.2.71"), http.StatusOK)
	var stored public.ClientGroup
	if err := primary.Get(t.Context(), store.Key{Kind: "ClientGroup", Namespace: "default", Name: "bound-group"}, &stored); err != nil || len(stored.Spec.Clients) != 1 || stored.Spec.Clients[0].IP != "192.0.2.71" || store.ManagedBy(&stored) != store.ManagerAPI {
		t.Fatal("kind-route replacement not committed with the public manager", err)
	}
	expect(http.MethodPut, "/blocklists/default/bound-editor", list("bound-editor", "editor.bound.test"), http.StatusCreated)
	expect(http.MethodDelete, "/blocklists/default/bound-editor", nil, http.StatusNoContent)
	if exists(primary, &public.BlockList{}, store.Key{Kind: "BlockList", Namespace: "default", Name: "bound-editor"}) {
		t.Fatal("kind-route deletion left the list on the primary")
	}

	// Batch deletion: one command for each name.
	var removed api.Processed
	if err := json.Unmarshal(expect(http.MethodPost, "/dnsrecordsets:batchDelete", map[string]any{"names": []string{"bound-www"}}, http.StatusOK), &removed); err != nil ||
		len(removed.Processed.Success) != 1 || removed.Processed.Success[0].Item != "default/bound-www" || len(removed.Processed.Errors) != 0 {
		t.Fatalf("record set batch deletion: %+v %v", removed.Processed, err)
	}
	if err := json.Unmarshal(expect(http.MethodPost, "/clientgroups:batchDelete", map[string]any{"names": []string{"bound-group"}}, http.StatusOK), &removed); err != nil ||
		len(removed.Processed.Success) != 1 || len(removed.Processed.Errors) != 0 {
		t.Fatalf("client group batch deletion: %+v %v", removed.Processed, err)
	}
	if exists(primary, &public.DNSRecordSet{}, store.Key{Kind: "DNSRecordSet", Namespace: "default", Name: "bound-www"}) ||
		exists(primary, &public.ClientGroup{}, store.Key{Kind: "ClientGroup", Namespace: "default", Name: "bound-group"}) {
		t.Fatal("batch deletion left objects on the primary")
	}

	// Every effect was the primary's commit with an attributed audit row; the
	// follower installed nothing by itself.
	if before.InstallSequence != follower.Authority().InstallSequence {
		t.Fatal("a forwarded command installed its own response on the follower")
	}
	entries, err := primary.ReadAudit(t.Context(), sqlite.AuditQuery{Kind: "BlockList", Namespace: "default", Name: "bound-list"})
	attributed := 0
	for _, entry := range entries {
		if entry.RequestPath == clusterreplica.PathWrite && entry.RequestID != "" && entry.Actor != "" && entry.Authz != "" {
			attributed++
		}
	}
	if err != nil || attributed != 1 {
		t.Fatal("applied list lacks exactly one attributed primary audit row", attributed, err)
	}
	// The follower serves the results after its own pull.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := f.puller.Pull(ctx); err != nil {
		t.Fatal("actual subsequent pull", err)
	}
	if !exists(follower, &public.DNSZone{}, store.Key{Kind: "DNSZone", Name: "bound"}) ||
		!exists(follower, &public.BlockList{}, store.Key{Kind: "BlockList", Namespace: "default", Name: "bound-list"}) ||
		exists(follower, &public.DNSRecordSet{}, store.Key{Kind: "DNSRecordSet", Namespace: "default", Name: "bound-www"}) ||
		exists(follower, &public.ClientGroup{}, store.Key{Kind: "ClientGroup", Namespace: "default", Name: "bound-group"}) ||
		exists(follower, &public.BlockList{}, store.Key{Kind: "BlockList", Namespace: "default", Name: "bound-editor"}) {
		t.Fatal("follower did not converge on the primary's forwarded results")
	}
}

// A multi-item request stays bound to the source the follower had when the
// request began. Once the follower installs a successor, every remaining item
// is refused with that cause and nothing reaches the primary.
func TestRootMemberForwardApplyOriginalSourceCutRefusesItems(t *testing.T) {
	f := newMemberForwardHTTPFixture(t, true)
	held := &memberForwardBodyHold{entered: make(chan struct{}), release: make(chan struct{})}
	f.hold.Store(held)
	var once sync.Once
	release := func() { once.Do(func() { close(held.release) }) }
	defer release()
	documents, err := json.Marshal([]any{
		map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": "ClientGroup", "metadata": map[string]any{"name": "cut-a", "namespace": "default"},
			"spec": map[string]any{"clients": []any{map[string]any{"ip": "192.0.2.80"}}}},
		map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": "ClientGroup", "metadata": map[string]any{"name": "cut-b", "namespace": "default"},
			"spec": map[string]any{"clients": []any{map[string]any{"ip": "192.0.2.81"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		status int
		body   []byte
		err    error
	}
	finished := make(chan result, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	go func() {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, f.live.URL+api.BasePath+"/apply", bytes.NewReader(documents))
		if err != nil {
			finished <- result{err: err}
			return
		}
		request.GetBody = nil
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+f.primary.token)
		response, err := f.live.Client().Do(request)
		if err != nil {
			finished <- result{err: err}
			return
		}
		defer func() { _ = response.Body.Close() }()
		raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		finished <- result{response.StatusCode, raw, err}
	}()
	select {
	case <-held.entered:
	case <-ctx.Done():
		t.Fatal("request did not reach original body read")
	}
	// The primary advances and the follower installs that successor while the
	// original request still waits for its body.
	writer, err := f.primary.s.opts.Store.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.primary.s.opts.Store.Command(sqlite.WithWriterToken(ctx, writer), writer.Term(), func(ctx context.Context, tx *sqlite.CommandTx) error {
		return tx.PutSection(ctx, "dedup", replication.Entry{Key: "apply-interposition", Data: []byte(`{"changed":true}`)})
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.puller.Pull(ctx); err != nil {
		t.Fatal(err)
	}
	release()
	var got result
	select {
	case got = <-finished:
	case <-ctx.Done():
		t.Fatal("original request did not join")
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	// Apply keeps its per-item answer: one refusal for each document, naming
	// the changed source. Never a commit.
	var applied api.List[api.ApplyResult]
	if got.status != http.StatusOK || json.Unmarshal(got.body, &applied) != nil || len(applied.Items) != 2 {
		t.Fatalf("apply after the source cut: status=%d body=%s", got.status, got.body)
	}
	for _, item := range applied.Items {
		if item.Result != api.ResultError || item.ErrorKey != api.KeyUnavailable || !strings.Contains(item.Error, "the follower's source changed") {
			t.Fatalf("item after the source cut: %+v", item)
		}
	}
	for _, name := range []string{"cut-a", "cut-b"} {
		err := f.primary.s.opts.Store.Get(t.Context(), store.Key{Kind: "ClientGroup", Namespace: "default", Name: name}, &public.ClientGroup{})
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatal("an item after the source cut reached the primary", name, err)
		}
	}
}
