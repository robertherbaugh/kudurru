package api

import (
	"net/http"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func TestNotifyOptionsPublicObjectRoundTripAndRejection(t *testing.T) {
	for _, tc := range []struct {
		name, key, source string
		status            int
	}{
		{"explicit", "notify.example.", "127.0.0.2", http.StatusCreated},
		{"ipv6", "notify.example.", "::1", http.StatusCreated},
		{"inherit", "", "", http.StatusCreated},
		{"invalid-key", "a..b", "127.0.0.2", http.StatusBadRequest},
		{"invalid-source", "notify.example.", "localhost", http.StatusBadRequest},
		{"unspecified-source", "notify.example.", "::", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			doc := zoneDoc("example", "example.com.")
			doc["spec"].(map[string]any)["policy"] = map[string]any{"notify": "specified", "notifyTargets": []string{"192.0.2.1"}, "notifyTSIGKeyRef": tc.key, "notifySource": tc.source}
			h.do(http.MethodPut, "/api/v1/objects/dnszones/example", "viewer", doc).want(http.StatusForbidden)
			h.do(http.MethodPut, "/api/v1/objects/dnszones/example", "admin", doc).want(tc.status)
			if tc.status != http.StatusCreated {
				h.do(http.MethodGet, "/api/v1/objects/dnszones/example", "admin", nil).want(http.StatusNotFound)
				return
			}
			var got public.DNSZone
			h.do(http.MethodGet, "/api/v1/objects/dnszones/example", "viewer", nil).want(http.StatusOK).json(&got)
			if got.Spec.Policy.NotifyTSIGKeyRef != tc.key || got.Spec.Policy.NotifySource != tc.source {
				t.Fatal("public API did not retain independent NOTIFY choices")
			}
		})
	}
}
