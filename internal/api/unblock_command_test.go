package api

import (
	"context"
	"net/http"
	"net/netip"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/unblock"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestUnblockCommandSQLiteRoutingPostcommitScopedInvalidation(t *testing.T) {
	h, st := objectCommandHarness(t, nil)
	rc := cache.New(cache.Options{MaxEntries: 100})
	h.srv.opts.ResponseCache = rc
	const domain = "requested.example"
	var keys []cache.Key
	for i, scope := range []pipeline.CacheScope{{ID: [32]byte{1}, Epoch: 1}, {ID: [32]byte{2}, Epoch: 2}} {
		key := cache.MakeScopedKey(scope, domain+".", 65400, uint16(i+1), true, true, netip.MustParsePrefix("192.0.2.0/24"))
		rc.Set(key, []byte("blocked"), cache.Positive, time.Minute)
		keys = append(keys, key)
	}
	sibling := cache.MakeKey("child."+domain+".", 1, 1, false, false, netip.Prefix{})
	rc.Set(sibling, []byte("sibling"), cache.Positive, time.Minute)
	generation := rc.Generation()
	request := &public.UnblockRequest{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "request"}, Spec: public.UnblockRequestSpec{Domain: domain}}
	zoneSeed(t, st, unblock.Manager, request)
	// A stale live Store would approve a different domain; only the supplied
	// concrete transaction may determine this request's effects.
	h.seed(unblock.Manager, &public.UnblockRequest{ObjectMeta: request.ObjectMeta, Spec: public.UnblockRequestSpec{Domain: "decoy.example"}})
	calls := 0
	h.srv.opts.UnblockCommand = func(ctx context.Context, target unblock.Target, d unblock.Decision) (*unblock.Outcome, error) {
		calls++
		var out *unblock.Outcome
		err := st.Command(ctx, st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			if rc.Generation() != generation {
				t.Fatal("cache invalidated under transaction pin")
			}
			var err error
			out, err = unblock.ExecuteCurrent(ctx, tx, target, d, time.Now())
			return err
		})
		return out, err
	}
	var result UnblockDecisionResult
	h.do(http.MethodPost, "/api/v1/unblockrequests/default/request/approve", "operator", UnblockApproval{Duration: "1h"}).want(http.StatusOK).json(&result)
	if calls != 1 || result.Request.Domain != domain || result.Flushed != 2 || result.Request.Allow == nil || rc.Generation() != generation+1 {
		t.Fatal("command result/cache did not follow committed current request", result, calls)
	}
	for _, key := range keys {
		if _, ok := rc.Peek(key); ok {
			t.Fatal("scoped blocked answer survived approval")
		}
	}
	if _, ok := rc.Peek(sibling); !ok {
		t.Fatal("exact approval flush crossed name boundary")
	}
	rc.SetIfGeneration(keys[0], []byte("detached stale fill"), cache.Positive, time.Minute, cache.Metadata{}, generation)
	if _, ok := rc.Peek(keys[0]); ok {
		t.Fatal("approval did not fence a detached old fill")
	}
	// Even an empty valid flush fences a detached fill for the same name.
	generation = rc.Generation()
	if h.srv.flushName(domain) != 0 || rc.Generation() != generation+1 {
		t.Fatal("empty invalidation omitted fill fence")
	}
	// An installed refusal and a partially mounted current server cannot use
	// the legacy live request book, report success, or invalidate cached data.
	h.srv.opts.UnblockCommand = func(context.Context, unblock.Target, unblock.Decision) (*unblock.Outcome, error) {
		return nil, errUnavailable("fenced")
	}
	h.do(http.MethodPost, "/api/v1/unblockrequests/default/request/deny", "operator", UnblockDenial{}).want(http.StatusServiceUnavailable)
	h.srv.opts.UnblockCommand = nil
	h.do(http.MethodPost, "/api/v1/unblockrequests/default/request/deny", "operator", UnblockDenial{}).want(http.StatusServiceUnavailable)
	var current public.UnblockRequest
	if err := st.Get(t.Context(), store.KeyOf(request), &current); err != nil || current.EffectiveState() != public.UnblockApproved || rc.Generation() != generation+1 {
		t.Fatal("refusal changed committed decision or cache", err)
	}
}
