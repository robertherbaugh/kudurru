package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/auth"
)

// The request and response shapes of the auth routes the auth package
// serves (Options.AuthHandler). They are documented here so the OpenAPI
// document and the generated clients cover them.
type (
	// AuthStatus is GET /auth/status.
	AuthStatus struct {
		// LocalLogin is enabled, admins-only or disabled.
		LocalLogin            string         `json:"localLogin"`
		Providers             []AuthProvider `json:"providers"`
		HasDefaultCredentials bool           `json:"hasDefaultCredentials"`
	}
	// AuthProvider is one OIDC login option.
	AuthProvider struct {
		ID          string `json:"id"`
		DisplayName string `json:"displayName"`
	}
	// LoginRequest is the body of POST /auth/login.
	LoginRequest struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	// TokenRequest is the body of POST /auth/tokens.
	TokenRequest struct {
		Name string `json:"name"`
		// ExpiresIn is a Go duration ("720h"); empty is the server default.
		ExpiresIn string `json:"expiresIn,omitempty"`
	}
	// Token is one personal API token; its token field (the kdr_ secret)
	// is only in the POST response, once.
	Token struct {
		ID         string     `json:"id"`
		Name       string     `json:"name"`
		Token      string     `json:"token,omitempty"`
		CreatedAt  *time.Time `json:"createdAt,omitempty"`
		ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
		LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	}
)

// Me is GET /auth/me.
type Me struct {
	Principal MePrincipal `json:"principal"`
	// Roles are the principal's grants: role, or role@kind:name for
	// scoped grants (the session's roles snapshot, or Options.Roles).
	Roles []string `json:"roles" nullable:"false"`
	// CSRF is the X-Kudurru-CSRF value of a cookie session.
	CSRF string `json:"csrf,omitempty"`
	// Permissions maps every resource to the verbs granted at cluster
	// scope.
	Permissions map[string][]string `json:"permissions"`
	Took
}

// MePrincipal is the principal of GET /auth/me.
type MePrincipal struct {
	Kind   string   `json:"kind"`
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Groups []string `json:"groups" nullable:"false"`
	// Auth is how the request authenticated: cookie, token, jwt, k8s.
	Auth string `json:"auth,omitempty"`
}

type meOutput struct{ Body *Me }

// IAMItem is one identity or binding of the read-only IAM routes.
type IAMItem = map[string]any

// IAMList is the list envelope of the IAM routes.
type IAMList struct {
	Items []IAMItem `json:"items" nullable:"false"`
	Took
}

type iamOutput struct{ Body *IAMList }

// AuditEvent is one row of GET /audit.
type AuditEvent = audit.Event

type auditInput struct {
	Kind   string `query:"kind" doc:"Resource kind (DNSZone, blocking, ...)."`
	Name   string `query:"name"`
	Actor  string `query:"actor" doc:"Actor name or <kind>:<name>."`
	Since  string `query:"since" doc:"RFC 3339."`
	Limit  int    `query:"limit" minimum:"0" maximum:"1000"`
	Cursor string `query:"cursor"`
}

// AuditPage is GET /audit: oldest first, keyset cursor on the event id.
type AuditPage struct {
	Items      []AuditEvent `json:"items" nullable:"false"`
	CursorNext string       `json:"cursor_next,omitempty"`
	Continue   string       `json:"continue,omitempty"`
	Node       string       `json:"node"`
	Took
}

type auditOutput struct{ Body *AuditPage }

func (s *Server) registerAuth() {
	public := perm{public: true}
	authed := perm{authenticated: true}
	// Own tokens: every built-in role holds tokens.own, including the
	// zone-scoped zone-admin, whose binding cannot satisfy a cluster-scope
	// check; the routes therefore need a principal only, and the auth
	// handler scopes them to the caller. A token never carries more than
	// its owner's bindings (§8.6).
	tokens := authed
	s.forward(operation(http.MethodGet, "/auth/status", "authStatus", "Login options", "auth"), public, nil, AuthStatus{})
	s.forward(withRateLimit(operation(http.MethodPost, "/auth/login", "login", "Local login (sets the session cookie)", "auth")), public, LoginRequest{}, Me{})
	s.forward(operation(http.MethodGet, "/auth/oidc/{idp}/start", "oidcStart", "Start an OIDC login (302 to the IdP)", "auth"), public, nil, nil)
	s.forward(operation(http.MethodGet, "/auth/oidc/{idp}/callback", "oidcCallback", "OIDC redirect target (302 to the app)", "auth"), public, nil, nil)
	s.forward(withStatus(operation(http.MethodPost, "/auth/logout", "logout", "Clear the session cookie", "auth"), http.StatusNoContent), authed, nil, nil)
	s.forward(withStatus(operation(http.MethodPost, "/auth/tokens", "createToken", "Create a personal API token (the secret is returned once)", "auth"), http.StatusCreated), tokens, TokenRequest{}, Token{})
	s.forward(operation(http.MethodGet, "/auth/tokens", "listTokens", "List own API tokens", "auth"), tokens, nil, List[Token]{})
	s.forward(operation(http.MethodGet, "/auth/tokens/{id}", "getToken", "Get one own API token", "auth"), tokens, nil, Token{})
	s.forward(withStatus(operation(http.MethodDelete, "/auth/tokens/{id}", "deleteToken", "Revoke one own API token", "auth"), http.StatusNoContent), tokens, nil, nil)
	register(s, operation(http.MethodGet, "/auth/me", "me", "The authenticated principal, roles, permissions and CSRF token", "auth"), authed, s.me)

	for _, r := range []struct{ path, resource, id string }{
		{"/iam/users", "users", "listUsers"},
		{"/iam/serviceaccounts", "serviceaccounts", "listServiceAccounts"},
		{"/iam/rolebindings", "rolebindings", "listRoleBindings"},
		{"/iam/identityproviders", "identityproviders", "listIdentityProviders"},
	} {
		r := r
		register(s, operation(http.MethodGet, r.path, r.id, "List "+r.resource+" (read-only; the write API is v0.2)", "iam"),
			perm{resource: r.resource, verb: "read"}, func(ctx context.Context, _ *struct{}) (*iamOutput, error) {
				return s.iamList(ctx, r.resource)
			})
	}
}

func withRateLimit(op huma.Operation) huma.Operation {
	op.Description = "Brute-force limited per client address and user: 429 with Retry-After."
	op.Errors = append(op.Errors, http.StatusTooManyRequests)
	return op
}

// forward registers a route served by Options.AuthHandler: the operation
// is documented (request and response schemas from the given examples)
// and authorized like any other, then the original request is handed to
// the auth handler untouched.
func (s *Server) forward(op huma.Operation, p perm, reqExample, respExample any) {
	reg := s.api.OpenAPI().Components.Schemas
	if reqExample != nil {
		op.RequestBody = &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{
			"application/json": {Schema: reg.Schema(reflect.TypeOf(reqExample), true, "")},
		}}
	}
	status := op.DefaultStatus
	if status == 0 {
		status = http.StatusOK
	}
	resp := &huma.Response{Description: http.StatusText(status)}
	if respExample != nil {
		resp.Content = map[string]*huma.MediaType{"application/json": {Schema: reg.Schema(reflect.TypeOf(respExample), true, "")}}
	}
	op.Responses = map[string]*huma.Response{strconv.Itoa(status): resp}
	// The auth handler reads the body itself. Login and token bodies are
	// tiny and the login route is public: never buffer more than the auth
	// handler accepts (64 KiB) for an anonymous caller.
	op.SkipValidateBody = true
	op.MaxBodyBytes = maxAuthBodyBytes
	for _, m := range pathParamRE.FindAllStringSubmatch(op.Path, -1) {
		op.Parameters = append(op.Parameters, &huma.Param{Name: m[1], In: "path", Required: true, Schema: &huma.Schema{Type: "string"}})
	}
	serve := func(body []byte) (*huma.StreamResponse, error) {
		if s.opts.AuthHandler == nil {
			return nil, errNotImplemented("authentication (no auth handler is configured)")
		}
		return &huma.StreamResponse{Body: func(ctx huma.Context) {
			r, w := humago.Unwrap(ctx)
			if body != nil {
				// huma consumed the body for the documented schema; hand
				// the auth handler a fresh reader over the same bytes.
				r.Body = io.NopCloser(bytes.NewReader(body))
				r.ContentLength = int64(len(body))
			}
			s.opts.AuthHandler.ServeHTTP(w, r)
		}}, nil
	}
	if reqExample != nil {
		register(s, op, p, func(_ context.Context, in *forwardBodyInput) (*huma.StreamResponse, error) {
			if in.RawBody == nil {
				in.RawBody = []byte{}
			}
			return serve(in.RawBody)
		})
		return
	}
	register(s, op, p, func(_ context.Context, _ *struct{}) (*huma.StreamResponse, error) { return serve(nil) })
}

// maxAuthBodyBytes caps the bodies of the forwarded auth routes (the auth
// handler refuses larger ones too).
const maxAuthBodyBytes = 64 << 10

// forwardBodyInput captures the body of a forwarded auth route.
type forwardBodyInput struct {
	RawBody []byte `contentType:"application/json"`
}

// pathParamRE finds the {name} wildcards of a route path.
var pathParamRE = regexp.MustCompile(`\{([a-zA-Z_][a-zA-Z0-9_]*)\}`)

// me answers GET /auth/me.
func (s *Server) me(ctx context.Context, _ *struct{}) (*meOutput, error) {
	p, ok := auth.PrincipalFrom(ctx)
	if !ok {
		return nil, errUnauthenticated()
	}
	out := &Me{
		Principal:   MePrincipal{Kind: string(p.Kind), ID: p.ID, Name: p.Name, Groups: p.Groups, Auth: p.AuthMethod},
		Roles:       []string{},
		Permissions: map[string][]string{},
	}
	if out.Principal.Groups == nil {
		out.Principal.Groups = []string{}
	}
	if sess, ok := auth.SessionFrom(ctx); ok {
		out.CSRF = sess.CSRF
		out.Roles = append(out.Roles, sess.RolesSnapshot...)
	} else if s.opts.Roles != nil {
		out.Roles = append(out.Roles, s.opts.Roles(ctx, p)...)
	}
	for _, res := range s.perms.ResourceNames() {
		for _, verb := range s.perms.Resources[res] {
			dec, err := s.authorize(ctx, p, res, verb, clusterScope, "")
			if err == nil && dec.Allowed {
				out.Permissions[res] = append(out.Permissions[res], verb)
			}
		}
	}
	return &meOutput{Body: out}, nil
}

func (s *Server) iamList(ctx context.Context, resource string) (*iamOutput, error) {
	var items []IAMItem
	var err error
	if s.opts.IAM != nil {
		switch resource {
		case "users":
			items, err = s.opts.IAM.Users(ctx)
		case "serviceaccounts":
			items, err = s.opts.IAM.ServiceAccounts(ctx)
		case "rolebindings":
			items, err = s.opts.IAM.RoleBindings(ctx)
		case "identityproviders":
			items, err = s.opts.IAM.IdentityProviders(ctx)
		}
	}
	if err != nil {
		return nil, storeError(err)
	}
	if items == nil {
		items = []IAMItem{}
	}
	return &iamOutput{Body: &IAMList{Items: items}}, nil
}

func (s *Server) registerAudit() {
	register(s, operation(http.MethodGet, "/audit", "listAudit", "Audit events of this node (oldest first)", "audit"),
		perm{resource: "audit", verb: "read"}, s.listAudit)
	exportOp := operation(http.MethodGet, "/audit/export", "exportAudit", "Audit events as JSON lines", "audit")
	exportOp.Responses = map[string]*huma.Response{"200": {Description: "application/x-ndjson, one AuditEvent per line",
		Content: map[string]*huma.MediaType{"application/x-ndjson": {Schema: &huma.Schema{Type: "string"}}}}}
	register(s, exportOp, perm{resource: "audit", verb: "export"}, s.exportAudit)
}

func (s *Server) auditReader() (audit.Reader, error) {
	if rd, ok := s.opts.Audit.(audit.Reader); ok {
		return rd, nil
	}
	return nil, newError(http.StatusServiceUnavailable, KeyUnavailable, "audit events are not stored on this node",
		"the sqlite store keeps them in state.db; in Kubernetes they are the \"log\":\"audit\" lines")
}

func auditQuery(in *auditInput) (audit.Query, error) {
	q := audit.Query{Kind: in.Kind, Name: in.Name, Actor: in.Actor, Limit: in.Limit}
	if in.Since != "" {
		t, err := time.Parse(time.RFC3339, in.Since)
		if err != nil {
			return q, errInvalid("since: %v", err)
		}
		q.Since = t
	}
	if in.Cursor != "" {
		id, err := strconv.ParseInt(in.Cursor, 10, 64)
		if err != nil || id < 0 {
			return q, errInvalid("cursor: not a cursor returned by this API")
		}
		q.AfterID = id
	}
	return q, nil
}

func (s *Server) listAudit(ctx context.Context, in *auditInput) (*auditOutput, error) {
	rd, err := s.auditReader()
	if err != nil {
		return nil, err
	}
	q, err := auditQuery(in)
	if err != nil {
		return nil, err
	}
	limit := clampLimit(q.Limit, 100, 1000)
	q.Limit = limit
	evs, err := rd.Read(ctx, q)
	if err != nil {
		return nil, storeError(err)
	}
	out := &AuditPage{Items: evs, Node: s.opts.NodeName}
	if out.Items == nil {
		out.Items = []AuditEvent{}
	}
	if len(evs) == limit {
		out.CursorNext = strconv.FormatInt(evs[len(evs)-1].ID, 10)
		out.Continue = out.CursorNext
	}
	return &auditOutput{Body: out}, nil
}

func (s *Server) exportAudit(ctx context.Context, in *auditInput) (*huma.StreamResponse, error) {
	rd, err := s.auditReader()
	if err != nil {
		return nil, err
	}
	q, err := auditQuery(in)
	if err != nil {
		return nil, err
	}
	s.recordAction(ctx, "audit.export", audit.Resource{Kind: "audit"}, nil, nil, nil)
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		r, w := humago.Unwrap(hctx)
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Content-Disposition", `attachment; filename="kudurru-audit-`+s.opts.NodeName+`.ndjson"`)
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		q.Limit = 1000
		for {
			evs, err := rd.Read(r.Context(), q)
			if err != nil {
				_, _ = fmt.Fprintf(w, `{"error":%q}`+"\n", err.Error())
				return
			}
			for _, ev := range evs {
				if enc.Encode(ev) != nil {
					return
				}
			}
			if len(evs) < q.Limit {
				return
			}
			q.AfterID = evs[len(evs)-1].ID
		}
	}}, nil
}
