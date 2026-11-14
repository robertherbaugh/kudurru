package api

import (
	"net/http"
	"testing"
)

func TestIntegrationAdministrationRequiresExplicitCapability(t *testing.T) {
	h := newHarness(t)
	h.do(http.MethodGet, "/api/v1/integrations?namespace=default", "admin", nil).want(http.StatusServiceUnavailable)
	for _, command := range []string{"check", "sync", "release"} {
		body := map[string]any{"uid": "reviewed-uid", "revision": "1"}
		h.do(http.MethodPost, "/api/v1/integrations/default/demo/"+command, "viewer", body).want(http.StatusForbidden)
		h.do(http.MethodPost, "/api/v1/integrations/default/demo/"+command, "admin", body).want(http.StatusServiceUnavailable)
	}
}
