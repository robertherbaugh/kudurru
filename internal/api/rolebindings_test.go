package api

import (
	"net/http"
	"sync/atomic"
	"testing"
)

func TestRoleBindingCommandsRefuseLegacyRuntimeAfterPermission(t *testing.T) {
	var forwarded atomic.Int32
	h := newHarness(t, func(o *Options) {
		o.AuthHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { forwarded.Add(1); w.WriteHeader(http.StatusOK) })
	})
	for _, op := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/api/v1/iam/rolebindings", RoleBindingDesired{}},
		{http.MethodPut, "/api/v1/iam/rolebindings/apps/one", ReplaceRoleBindingCommand{}},
		{http.MethodDelete, "/api/v1/iam/rolebindings/apps/one", RoleBindingReview{}},
	} {
		h.do(op.method, op.path, "", op.body).want(http.StatusUnauthorized)
		h.do(op.method, op.path, "viewer", op.body).want(http.StatusForbidden)
		h.do(op.method, op.path, "admin", op.body).want(http.StatusServiceUnavailable)
	}
	if forwarded.Load() != 0 {
		t.Fatal("unmounted mutation forwarded to a legacy handler")
	}
	// Existing IAM list forwarding is independent of mutation capability.
	h.do(http.MethodGet, "/api/v1/iam/rolebindings", "admin", nil).want(http.StatusOK)
	if forwarded.Load() != 1 {
		t.Fatal("existing list capability changed")
	}
	path := h.srv.OpenAPI().Paths["/iam/rolebindings/{namespace}/{name}"]
	if path == nil || path.Get == nil || path.Put == nil || path.Delete == nil || path.Put.Responses["201"] != nil {
		t.Fatal("review routes missing or replacement advertises creation")
	}
}
