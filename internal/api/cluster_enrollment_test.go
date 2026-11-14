package api

import (
	"net/http"
	"sync/atomic"
	"testing"
)

func TestClusterEnrollmentRefusesLegacyAfterPermission(t *testing.T) {
	var forwarded atomic.Int32
	h := newHarness(t, func(o *Options) {
		o.AuthHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { forwarded.Add(1); w.WriteHeader(200) })
	})
	h.do("POST", BasePath+"/cluster/activate", "", ActivateClusterCommand{}).want(http.StatusUnauthorized)
	h.do("POST", BasePath+"/cluster/activate", "viewer", ActivateClusterCommand{}).want(http.StatusForbidden)
	h.do("POST", BasePath+"/cluster/activate", "admin", ActivateClusterCommand{}).want(http.StatusServiceUnavailable)
	h.do("GET", BasePath+"/cluster/enrollment", "", nil).want(http.StatusUnauthorized)
	h.do("GET", BasePath+"/cluster/enrollment", "admin", nil).want(http.StatusServiceUnavailable)
	if forwarded.Load() != 0 {
		t.Fatal("enrollment reached legacy handler")
	}
}

func TestClusterEnrollmentStaticRefusesBeforeForwarding(t *testing.T) {
	var forwarded atomic.Int32
	h := newHarness(t, func(o *Options) {
		o.Config.Cluster.Role = "static"
		o.AuthHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { forwarded.Add(1); w.WriteHeader(200) })
	})
	h.do("POST", BasePath+"/cluster/activate", "", ActivateClusterCommand{}).want(http.StatusUnauthorized)
	h.do("POST", BasePath+"/cluster/activate", "viewer", ActivateClusterCommand{}).want(http.StatusForbidden)
	if r := h.do("POST", BasePath+"/cluster/activate", "admin", ActivateClusterCommand{}).want(http.StatusServiceUnavailable); r.errKey() != KeyReadOnly {
		t.Fatal("static enrollment bypassed mutation refusal", r.errKey())
	}
	if forwarded.Load() != 0 {
		t.Fatal("static activation forwarded")
	}
}
