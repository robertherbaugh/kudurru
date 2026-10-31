package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cache"
)

func TestCacheInspectionValidationPermissionAndPrivacy(t *testing.T) {
	var calls int
	h := newHarness(t, func(o *Options) {
		o.CacheInspect = func(_ context.Context, in CacheInspectRequest) (cache.Lookup, bool, bool, error) {
			calls++
			if in.Name != "inspect.example." || in.Type != 1 || in.Client.String() != "127.0.0.1" {
				t.Fatal("unexpected key selection", in)
			}
			return cache.Lookup{Wire: []byte("private answer"), Metadata: cache.Metadata{Upstream: "private upstream"}, Kind: cache.Negative, State: cache.Stale, Age: 90 * time.Second, TTL: time.Minute}, true, true, nil
		}
	})
	var out CacheInspection
	response := h.do(http.MethodGet, "/api/v1/cache/inspect?name=INSPECT.example", "viewer", nil).want(http.StatusOK)
	response.json(&out)
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("cache metadata may be stored by HTTP clients")
	}
	if !out.Found || !out.Cacheable || out.Entry == nil || out.Entry.State != "stale" || out.Entry.RemainingSeconds != 0 || out.Entry.AgeSeconds != 90 {
		t.Fatal(out)
	}
	raw, err := json.Marshal(out)
	if err != nil || strings.Contains(string(raw), "private") {
		t.Fatal("private cache material exposed", err)
	}
	for _, path := range []string{"?name=inspect.example&type=nonsense", "?name=inspect.example&client=fe80::1%25eth0", "?name=inspect.example&client=bad"} {
		h.do(http.MethodGet, "/api/v1/cache/inspect"+path, "viewer", nil).want(http.StatusBadRequest)
	}
	h.do(http.MethodGet, "/api/v1/cache/inspect?name=", "viewer", nil).want(http.StatusBadRequest)
	if calls != 1 {
		t.Fatal("invalid input reached cache", calls)
	}
	h.do(http.MethodGet, "/api/v1/cache/inspect?name=inspect.example", "", nil).want(http.StatusUnauthorized)
	h.do(http.MethodGet, "/api/v1/cache/inspect?name=inspect.example", "zoneadmin", nil).want(http.StatusForbidden)
	if calls != 1 {
		t.Fatal("denied request reached cache")
	}
}
