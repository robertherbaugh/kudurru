package api

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestLocalUserForwardingBoundsAndPermissions(t *testing.T) {
	for _, tc := range []struct {
		path, body string
		status     int
	}{
		{"/iam/users", `{"name":"alice","password":"only-forwarded-test-password"}`, http.StatusCreated},
		{"/iam/users/alice/password", `{"uid":"exact-uid","resourceVersion":"7","newPassword":"only-forwarded-test-password"}`, http.StatusNoContent},
	} {
		t.Run(tc.path, func(t *testing.T) {
			calls := 0
			h := newHarness(t, func(o *Options) {
				o.AuthHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					raw, err := io.ReadAll(r.Body)
					if err != nil || string(raw) != tc.body || r.URL.Path != BasePath+tc.path {
						t.Error("forwarder changed the admitted request")
					}
					w.WriteHeader(tc.status)
				})
			})
			for user, status := range map[string]int{"": http.StatusUnauthorized, "viewer": http.StatusForbidden, "zoneadmin": http.StatusForbidden} {
				h.do(http.MethodPost, BasePath+tc.path, user, tc.body, "Content-Type", "application/json").want(status)
			}
			h.do(http.MethodPost, BasePath+tc.path, "admin", strings.Repeat("x", (16<<10)+1), "Content-Type", "application/json").want(http.StatusRequestEntityTooLarge)
			if calls != 0 {
				t.Fatal("refused request reached credential handler")
			}
			h.do(http.MethodPost, BasePath+tc.path, "admin", tc.body, "Content-Type", "application/json").want(tc.status)
			if calls != 1 {
				t.Fatal("admitted request was not forwarded exactly once")
			}
		})
	}
}

func TestLocalUserSecretSchemas(t *testing.T) {
	h := newHarness(t)
	schemas := h.srv.OpenAPI().Components.Schemas.Map()
	for name, secret := range map[string]string{"CreateLocalUserRequest": "password", "ResetLocalUserPasswordRequest": "newPassword"} {
		schema := schemas[name]
		if schema == nil || schema.Properties[secret] == nil || !schema.Properties[secret].WriteOnly {
			t.Fatal("local user credential schema omitted writeOnly", name)
		}
	}
	user := schemas["IAMUser"]
	if user == nil || user.Properties["uid"] == nil || user.Properties["resourceVersion"] == nil {
		t.Fatal("user inventory schema omitted exact account CAS identity")
	}
	for _, secret := range []string{"password", "newPassword", "passwordHash", "totpSecret"} {
		if user.Properties[secret] != nil {
			t.Fatal("safe inventory schema exposes a credential field")
		}
	}
}
