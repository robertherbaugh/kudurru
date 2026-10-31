package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/adminsock"
	"github.com/robertherbaugh/kudurru/internal/auth/stack"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// TestForwardedShapes keeps the OpenAPI documentation of the routes the
// auth stack serves honest: every documented mirror in internal/api has
// the JSON shape of the stack type it documents, request bodies built from
// the documented types decode in the real handlers (which reject unknown
// fields), and the real answers decode into the documented types with
// unknown fields rejected.
func TestForwardedShapes(t *testing.T) {
	for _, pair := range []struct{ doc, real any }{
		{api.AuthStatus{}, stack.StatusResponse{}},
		{api.LoginRequest{}, stack.LoginRequest{}},
		{api.SessionInfo{}, stack.MeResponse{}},
		{api.PasswordChangeRequest{}, stack.PasswordRequest{}},
		{api.CreateTokenRequest{}, stack.CreateTokenRequest{}},
		{api.TokenInfo{}, stack.TokenInfo{}},
		{api.CreatedToken{}, stack.CreatedToken{}},
		{api.IAMUser{}, adminsock.User{}},
		{api.ServiceAccountInfo{}, stack.ServiceAccountInfo{}},
		{api.RoleBindingInfo{}, stack.RoleBindingInfo{}},
		{api.IdentityProviderInfo{}, stack.IdentityProviderInfo{}},
	} {
		doc, actual := reflect.TypeOf(pair.doc), reflect.TypeOf(pair.real)
		if d, r := jsonShape(doc), jsonShape(actual); d != r {
			t.Errorf("api.%s does not document %s:\n doc  %s\n real %s", doc.Name(), actual, d, r)
		}
	}

	ts, st := authAPI(t, func(cfg *config.Config) {
		cfg.RBAC.Bindings = []map[string]any{{"role": "viewer", "scope": map[string]any{"kind": "cluster"},
			"subjects": []any{map[string]any{"kind": "Group", "name": "staff"}}}}
	})
	ctx := context.Background()
	for _, o := range []store.Object{
		&internalv1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "default"}, Spec: internalv1.ServiceAccountSpec{Description: "CI"}},
		&internalv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "ci-viewer", Namespace: "default"}, Spec: internalv1.RoleBindingSpec{
			Role: "viewer", Scope: internalv1.BindingScope{Kind: "cluster"},
			Subjects: []internalv1.BindingSubject{{Kind: "ServiceAccount", Name: "ci", Namespace: "default"}},
		}},
	} {
		if _, err := st.Apply(ctx, o, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}

	jar, _ := cookiejar.New(nil)
	c := &contractClient{t: t, base: ts.URL + "/api/v1", http: &http.Client{Jar: jar}}
	var status api.AuthStatus
	c.call(http.MethodGet, "/auth/status", nil, http.StatusOK, &status)
	if !status.LocalLogin || status.LocalLoginMode != "enabled" {
		t.Fatalf("status %+v", status)
	}
	var login api.SessionInfo
	c.call(http.MethodPost, "/auth/login", api.LoginRequest{Username: "admin", Password: "admin-password-1", TOTPCode: "123456"}, http.StatusServiceUnavailable, nil)
	c.call(http.MethodPost, "/auth/login", api.LoginRequest{Username: "admin", Password: "admin-password-1"}, http.StatusOK, &login)
	if login.CSRF == "" || !slices.Equal(login.Roles, []string{"admin"}) {
		t.Fatalf("login %+v", login)
	}
	c.csrf = login.CSRF
	c.call(http.MethodGet, "/auth/sessions", nil, http.StatusServiceUnavailable, nil)
	c.call(http.MethodDelete, "/auth/sessions/unavailable", nil, http.StatusServiceUnavailable, nil)
	c.call(http.MethodGet, "/auth/totp", nil, http.StatusServiceUnavailable, nil)
	c.call(http.MethodPost, "/auth/totp/setup", api.TOTPProofRequest{CurrentPassword: "unused-password"}, http.StatusServiceUnavailable, nil)
	c.call(http.MethodPost, "/auth/totp/confirm", api.TOTPConfirmRequest{CurrentPassword: "unused-password", Challenge: "unavailable", Code: "123456"}, http.StatusServiceUnavailable, nil)
	c.call(http.MethodPost, "/auth/totp/disable", api.TOTPProofRequest{CurrentPassword: "unused-password"}, http.StatusServiceUnavailable, nil)
	c.call(http.MethodPost, "/iam/users/admin/totp/recover", api.TOTPRecoverRequest{UID: "unavailable"}, http.StatusServiceUnavailable, nil)
	c.call(http.MethodPost, "/iam/users", api.CreateLocalUserRequest{Name: "unavailable", Password: "unused-password"}, http.StatusServiceUnavailable, nil)
	c.call(http.MethodPost, "/iam/users/admin/password", api.ResetLocalUserPasswordRequest{UID: "unavailable", ResourceVersion: "unavailable", NewPassword: "unused-password"}, http.StatusServiceUnavailable, nil)
	var me api.Me
	c.call(http.MethodGet, "/auth/me", nil, http.StatusOK, &me)
	if !slices.Equal(me.Roles, login.Roles) || me.CSRF != login.CSRF || me.MustChangePassword || len(me.Permissions) == 0 {
		t.Fatalf("me %+v, login roles %v", me, login.Roles)
	}

	var created api.CreatedToken
	c.call(http.MethodPost, "/auth/tokens", api.CreateTokenRequest{Description: "contract", ExpiresIn: "1h", Scopes: []string{"viewer"}}, http.StatusCreated, &created)
	if !strings.HasPrefix(created.Token, "kdr_") || created.Description != "contract" || created.ExpiresAt == nil || !slices.Equal(created.Scopes, []string{"viewer"}) {
		t.Fatalf("created %+v", created)
	}
	var tokens api.AuthList[api.TokenInfo]
	c.call(http.MethodGet, "/auth/tokens", nil, http.StatusOK, &tokens)
	if len(tokens.Items) != 1 || tokens.Items[0].ID != created.ID {
		t.Fatalf("tokens %+v", tokens)
	}
	var one api.TokenInfo
	c.call(http.MethodGet, "/auth/tokens/"+created.ID, nil, http.StatusOK, &one)
	if one.ID != created.ID || one.Owner.Name != "admin" {
		t.Fatalf("token %+v", one)
	}

	var users api.AuthList[api.IAMUser]
	c.call(http.MethodGet, "/iam/users", nil, http.StatusOK, &users)
	if len(users.Items) != 1 || users.Items[0].Name != "admin" || users.Items[0].UID == "" || users.Items[0].ResourceVersion == "" || !users.Items[0].BuiltinAdmin {
		t.Fatalf("users %+v", users)
	}
	var sas api.AuthList[api.ServiceAccountInfo]
	var rbs api.AuthList[api.RoleBindingInfo]
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.call(http.MethodGet, "/iam/serviceaccounts", nil, http.StatusOK, &sas)
		c.call(http.MethodGet, "/iam/rolebindings", nil, http.StatusOK, &rbs)
		if slices.ContainsFunc(sas.Items, func(s api.ServiceAccountInfo) bool { return s.Name == "ci" }) && len(rbs.Items) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("service accounts %+v, role bindings %+v", sas, rbs)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if rbs.Items[0].Source != "config" || rbs.Items[1].Source != "object" || rbs.Items[1].Subjects[0].Kind != "ServiceAccount" {
		t.Fatalf("role bindings %+v", rbs)
	}
	var idps api.AuthList[api.IdentityProviderInfo]
	c.call(http.MethodGet, "/iam/identityproviders", nil, http.StatusOK, &idps)

	c.call(http.MethodDelete, "/auth/tokens/"+created.ID, nil, http.StatusNoContent, nil)
	var changed api.SessionInfo
	c.call(http.MethodPost, "/auth/password", api.PasswordChangeRequest{CurrentPassword: "admin-password-1", NewPassword: "admin-password-2"}, http.StatusOK, &changed)
	if changed.CSRF == "" || changed.MustChangePassword {
		t.Fatalf("password change %+v", changed)
	}
}

// contractClient calls the API with a cookie session and decodes answers
// strictly.
type contractClient struct {
	t    *testing.T
	base string
	http *http.Client
	csrf string
}

func (c *contractClient) call(method, path string, body any, code int, out any) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.csrf != "" {
		req.Header.Set("X-Kudurru-CSRF", c.csrf)
	}
	res, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != code {
		c.t.Fatalf("%s %s: %d, want %d: %s", method, path, res.StatusCode, code, raw)
	}
	if out == nil {
		return
	}
	reflect.ValueOf(out).Elem().SetZero()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		c.t.Fatalf("%s %s: the answer does not match the documented %T: %v\n%s", method, path, out, err, raw)
	}
}

// jsonShape renders the JSON encoding of t: field names with their
// omitempty flag and value shapes, sorted; pointers are marked, named
// string or number types reduce to their kind, embedded structs are
// flattened.
func jsonShape(t reflect.Type) string {
	ptr := ""
	for t.Kind() == reflect.Pointer {
		t, ptr = t.Elem(), "*"
	}
	if t == reflect.TypeFor[time.Time]() {
		return ptr + "time"
	}
	switch t.Kind() {
	case reflect.Struct:
		var fields []string
		var walk func(reflect.Type)
		walk = func(t reflect.Type) {
			for i := range t.NumField() {
				f := t.Field(i)
				tag := f.Tag.Get("json")
				if f.Anonymous && tag == "" {
					walk(f.Type)
					continue
				}
				name, opts, _ := strings.Cut(tag, ",")
				if !f.IsExported() || name == "-" {
					continue
				}
				if name == "" {
					name = f.Name
				}
				if opts != "" {
					name += "," + opts
				}
				fields = append(fields, name+":"+jsonShape(f.Type))
			}
		}
		walk(t)
		sort.Strings(fields)
		return ptr + "{" + strings.Join(fields, " ") + "}"
	case reflect.Slice:
		return ptr + "[]" + jsonShape(t.Elem())
	case reflect.Map:
		return ptr + "map[" + jsonShape(t.Key()) + "]" + jsonShape(t.Elem())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return ptr + "int"
	case reflect.Float32, reflect.Float64:
		return ptr + "float"
	}
	return ptr + t.Kind().String()
}
