package api

import (
	"net/http"
	"sync/atomic"
	"testing"
)

func TestClusterJoinTokenPermissionAndRuntimeRefusals(t *testing.T) {
	for _, role := range []string{"primary", "static"} {
		t.Run(role, func(t *testing.T) {
			var forwarded atomic.Int32
			h := newHarness(t, func(o *Options) {
				o.Config.Cluster.Role = role
				o.AuthHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { forwarded.Add(1); w.WriteHeader(200) })
			})
			h.do("POST", BasePath+"/cluster/join-token", "", ClusterJoinTokenCommand{}).want(http.StatusUnauthorized)
			h.do("POST", BasePath+"/cluster/join-token", "viewer", ClusterJoinTokenCommand{}).want(http.StatusForbidden)
			h.do("POST", BasePath+"/cluster/join-token", "admin", ClusterJoinTokenCommand{}).want(http.StatusServiceUnavailable)
			if forwarded.Load() != 0 {
				t.Fatal("invitation reached unsupported runtime")
			}
		})
	}
}
