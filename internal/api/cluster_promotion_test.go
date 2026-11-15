package api

import (
	"net/http"
	"sync/atomic"
	"testing"
)

func TestClusterPromotionRefusesLegacyAfterPermission(t *testing.T) {
	var forwarded atomic.Int32
	h := newHarness(t, func(o *Options) {
		o.AuthHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { forwarded.Add(1); w.WriteHeader(200) })
	})
	h.do("POST", BasePath+"/cluster/promote", "", ClusterPromoteCommand{}).want(http.StatusUnauthorized)
	h.do("POST", BasePath+"/cluster/promote", "viewer", ClusterPromoteCommand{}).want(http.StatusForbidden)
	h.do("POST", BasePath+"/cluster/promote", "admin", ClusterPromoteCommand{}).want(http.StatusServiceUnavailable)
	h.do("GET", BasePath+"/cluster/promotion", "", nil).want(http.StatusUnauthorized)
	h.do("GET", BasePath+"/cluster/promotion", "admin", nil).want(http.StatusServiceUnavailable)
	if forwarded.Load() != 0 {
		t.Fatal("promotion reached legacy handler")
	}
}

func TestClusterPromotionStaticRefusesBeforeForwarding(t *testing.T) {
	var forwarded atomic.Int32
	h := newHarness(t, func(o *Options) {
		o.Config.Cluster.Role = "static"
		o.AuthHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { forwarded.Add(1); w.WriteHeader(200) })
	})
	h.do("POST", BasePath+"/cluster/promote", "", ClusterPromoteCommand{}).want(http.StatusUnauthorized)
	h.do("POST", BasePath+"/cluster/promote", "viewer", ClusterPromoteCommand{}).want(http.StatusForbidden)
	if r := h.do("POST", BasePath+"/cluster/promote", "admin", ClusterPromoteCommand{}).want(http.StatusServiceUnavailable); r.errKey() != KeyReadOnly {
		t.Fatal("static promotion bypassed mutation refusal", r.errKey())
	}
	if forwarded.Load() != 0 {
		t.Fatal("static promotion forwarded")
	}
}
