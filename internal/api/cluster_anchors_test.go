package api

import (
	"net/http"
	"sync/atomic"
	"testing"
)

func TestClusterAnchorsRequiresPermissionAndCompleteRuntime(t *testing.T) {
	var forwarded atomic.Int32
	h := newHarness(t, func(o *Options) {
		o.AuthHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { forwarded.Add(1); w.WriteHeader(200) })
	})
	h.do("GET", BasePath+"/cluster/anchors", "", nil).want(http.StatusUnauthorized)
	h.do("GET", BasePath+"/cluster/anchors", "nobody", nil).want(http.StatusForbidden)
	h.do("GET", BasePath+"/cluster/anchors", "admin", nil).want(http.StatusServiceUnavailable)
	if forwarded.Load() != 0 {
		t.Fatal("anchor status reached the legacy handler")
	}
}
