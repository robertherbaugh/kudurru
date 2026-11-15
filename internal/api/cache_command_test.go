package api

import (
	"context"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

func TestCacheFlushCommandValidationRoutingAndAudit(t *testing.T) {
	var names []string
	h := newHarness(t, func(o *Options) {
		o.ResponseCache = nil // complete callback owns its exact mounted instance
		o.CacheFlushCommand = func(_ context.Context, name string) (int, error) {
			names = append(names, name)
			return 7, nil
		}
	})
	var result FlushResult
	h.do(http.MethodPost, "/api/v1/cache/flush?name=ignored.example", "operator", map[string]string{"name": " TARGET.Example "}).want(http.StatusOK).json(&result)
	if result.Name != "target.example." || result.Flushed != 7 || len(names) != 1 || names[0] != result.Name {
		t.Fatal("command did not receive normalized body precedence", result, names)
	}
	h.do(http.MethodPost, "/api/v1/cache/flush?name=.", "operator", nil).want(http.StatusOK)
	h.do(http.MethodPost, "/api/v1/cache/flush", "operator", nil).want(http.StatusOK)
	if len(names) != 3 || names[1] != "." || names[2] != "" {
		t.Fatal("root and full clear are indistinguishable", names)
	}
	for _, name := range []string{" ", strings.Repeat("a", 1025), strings.Repeat("a", 64) + ".example", "a..example"} {
		h.do(http.MethodPost, "/api/v1/cache/flush", "operator", map[string]string{"name": name}).want(http.StatusBadRequest)
	}
	h.do(http.MethodPost, "/api/v1/cache/flush", "viewer", nil).want(http.StatusForbidden)
	if len(names) != 3 {
		t.Fatal("invalid or unauthorized name reached callback")
	}
	count := 0
	for _, event := range h.audit.Events() {
		if event.Action == "cache.flush" && event.Result == "success" {
			if count == 0 && event.Resource.Name != "target.example." {
				t.Fatal("flush command omitted its exact audit observation")
			}
			count++
		}
	}
	if count != 3 {
		t.Fatal("wrong flush audit count", count)
	}
}

func TestCacheFlushCommandFailureNeverFallsBack(t *testing.T) {
	rc := cache.New(cache.Options{MaxEntries: 100})
	key := cache.MakeKey("target.example.", 1, 1, false, false, netip.Prefix{})
	rc.Set(key, []byte("live"), cache.Positive, time.Minute)
	before := rc.Generation()
	h := newHarness(t, func(o *Options) {
		o.ResponseCache = rc
		o.CacheFlushCommand = func(context.Context, string) (int, error) {
			return 3, context.Canceled // a completed local effect can precede error
		}
	})
	h.do(http.MethodPost, "/api/v1/cache/flush", "operator", nil).want(http.StatusServiceUnavailable)
	if rc.Len() != 1 || rc.Generation() != before {
		t.Fatal("failed current command fell back to legacy cache")
	}
	events := h.audit.Events()
	if len(events) != 1 || events[0].Result == "success" || !strings.Contains(string(events[0].After), `"flushed":3`) {
		t.Fatal("post-effect failure hid its actual removal count")
	}
	h.srv.opts.CacheFlushCommand = nil
	h.srv.opts.DNSQueryCapture = func(*pipeline.Request) (pipeline.ServingLease, error) { return nil, nil }
	h.do(http.MethodPost, "/api/v1/cache/flush", "operator", nil).want(http.StatusServiceUnavailable)
	if rc.Len() != 1 || rc.Generation() != before {
		t.Fatal("partial complete mount used legacy cache")
	}
}
