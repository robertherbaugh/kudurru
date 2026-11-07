package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"time"

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

type servingHTTPHandler func(http.ResponseWriter, *http.Request) error

func (h *servingHTTP) route(method, path string, fn servingHTTPHandler) {
	h.mux.HandleFunc(method+" "+h.opts.RoutePrefix+path, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if err := fn(w, r); err != nil {
			servingHTTPError(w, r, err)
		}
	})
}
func servingHTTPError(w http.ResponseWriter, r *http.Request, err error) {
	var ae *auth.Error
	if errors.As(err, &ae) {
		auth.WriteError(w, r, ae)
		return
	}
	var e *api.Error
	if errors.As(currentCommandAPIError(err), &e) {
		auth.WriteError(w, r, &auth.Error{Status: e.Status, Key: e.Body.Key, Message: e.Body.Message, Hint: e.Body.Hint})
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		auth.WriteError(w, r, servingHTTPMissing())
		return
	}
	auth.WriteError(w, r, &auth.Error{Status: 503, Key: "unavailable", Message: "authentication operation unavailable"})
}
func servingHTTPInvalid(message string) error {
	return &auth.Error{Status: 400, Key: "invalid", Message: message}
}
func servingHTTPMissing() *auth.Error {
	return &auth.Error{Status: 404, Key: "not_found", Message: "resource not found"}
}
func servingHTTPDecode(w http.ResponseWriter, r *http.Request, out any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	raw, err := io.ReadAll(r.Body)
	if err != nil || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		return servingHTTPInvalid("invalid request body")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return servingHTTPInvalid("invalid request body")
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return servingHTTPInvalid("request must contain one JSON object")
	}
	return nil
}
func servingHTTPJSON(w http.ResponseWriter, status int, out any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(out)
	return nil
}
func servingHTTPList[T any](w http.ResponseWriter, r *http.Request, items []T) error {
	if items == nil {
		items = []T{}
	}
	return servingHTTPJSON(w, 200, api.AuthList[T]{Items: items, TookMS: auth.TookMS(r)})
}
func (h *servingHTTP) now() time.Time { return h.opts.Runtime.opts.Now().UTC() }
func (h *servingHTTP) clientKey(r *http.Request) string {
	raw := r.RemoteAddr
	if h.opts.ClientIP != nil {
		raw = h.opts.ClientIP(r)
	}
	if host, _, err := net.SplitHostPort(raw); err == nil {
		raw = host
	}
	if ip, err := netip.ParseAddr(raw); err == nil {
		ip = ip.Unmap()
		if ip.Is6() {
			return netip.PrefixFrom(ip, 64).Masked().String()
		}
		return ip.String()
	}
	return "unknown" // Never create attacker-controlled unbounded limiter keys.
}
func (h *servingHTTP) checkLoginLimit(r *http.Request, name string) error {
	d := max(h.ipLimit.Locked(h.clientKey(r)), h.userLimit.Locked(users.NormalizeUsername(name)))
	if d > 0 {
		return &auth.Error{Status: 429, Key: "rate_limited", Message: "too many failed attempts; try again later", RetryAfter: d}
	}
	return nil
}
func (h *servingHTTP) loginResult(r *http.Request, name string, err error) {
	if err == nil {
		h.userLimit.Reset(users.NormalizeUsername(name))
		return
	}
	if errors.Is(err, servingauth.ErrCredentials) {
		h.ipLimit.Fail(h.clientKey(r))
		h.userLimit.Fail(users.NormalizeUsername(name))
	}
}
func servingHTTPCluster(resource, verb string) rbac.Attributes {
	return rbac.Attributes{Resource: resource, Verb: verb, Scope: rbac.Scope{Kind: rbac.ScopeCluster}}
}
func (h *servingHTTP) permission(ctx context.Context, resource, verb string) (*servingauth.View, auth.Principal, error) {
	v, p, err := h.readPrincipal(ctx)
	if err != nil {
		return nil, p, err
	}
	d, err := v.Authorize(p, servingHTTPCluster(resource, verb), h.now())
	if err != nil {
		return nil, p, err
	}
	if !d.Allowed {
		return nil, p, servingauth.ErrDenied
	}
	return v, p, nil
}

// anyTokenScope chooses a candidate from the original view only. Every write
// independently verifies that exact candidate in its actual CommandTx.
func (h *servingHTTP) anyTokenScope(ctx context.Context) (rbac.Attributes, error) {
	v, p, err := h.readPrincipal(ctx)
	if err != nil {
		return rbac.Attributes{}, err
	}
	bindings, err := rbac.ParseBindings(h.opts.Runtime.opts.Config.RBAC.Bindings)
	if err != nil {
		return rbac.Attributes{}, err
	}
	return servingHTTPTokenScope(v, p, bindings, h.now())
}

// token scopes can cover a wider scope than the owner's actual grant. Their
// intersection must still permit own-token management at the owner's scope.
// These are candidate scopes only; every choice is checked by the same View.
func servingHTTPTokenScope(v *servingauth.View, p auth.Principal, bindings []rbac.Binding, now time.Time) (rbac.Attributes, error) {
	scopes := []rbac.Scope{{Kind: rbac.ScopeCluster}}
	for _, wire := range append(append([]string{}, p.Scopes...), p.Grants...) {
		g, err := rbac.ParseGrant(wire)
		if err == nil {
			scopes = append(scopes, g.Normalized().Scope)
		}
	}
	objects, err := v.Objects("RoleBinding")
	if err != nil {
		return rbac.Attributes{}, err
	}
	for _, o := range objects {
		bindings = append(bindings, rbac.FromRoleBinding(o.(*publicv1.RoleBinding)))
	}
	for _, b := range bindings {
		if rbac.SubjectsMatch(b.Subjects, auth.RBACPrincipal(p)) {
			scopes = append(scopes, (rbac.Grant{Role: b.Role, Scope: b.Scope}).Normalized().Scope)
		}
	}
	seen := map[rbac.Scope]bool{}
	for _, s := range scopes {
		if seen[s] {
			continue
		}
		seen[s] = true
		a := rbac.Attributes{Resource: "tokens", Verb: "own", Scope: s}
		d, err := v.Authorize(p, a, now)
		if err == nil && d.Allowed {
			return a, nil
		}
	}
	return rbac.Attributes{}, servingauth.ErrDenied
}

func servingHTTPSecurity(ctx context.Context, tx *sqlite.CommandTx) (servingauth.Security, error) {
	if err := tx.CheckSectionBounds(ctx, "security", servingauth.MaxRows, servingauth.MaxSecurityBytes); err != nil {
		return servingauth.Security{}, err
	}
	rows, err := tx.ListSections(ctx, "security")
	if err != nil {
		return servingauth.Security{}, err
	}
	return servingauth.DecodeSecurity(rows)
}
