package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/robertherbaugh/kudurru/internal/license"
)

// Me is GET /auth/me.
type Me struct {
	Principal MePrincipal `json:"principal"`
	// Roles are the principal's effective grants: role, or role@kind:name
	// for scoped grants (Options.Roles; without it the session's roles
	// snapshot).
	Roles []string `json:"roles" nullable:"false" doc:"Effective grants: role, or role@kind:name for scoped grants."`
	// CSRF is the X-Kudurru-CSRF value of a cookie session.
	CSRF string `json:"csrf,omitempty"`
	// Permissions maps every resource to the verbs granted at cluster
	// scope.
	Permissions map[string][]string `json:"permissions"`
	// MustChangePassword is set for a local user whose password was
	// generated or reset: every route but /auth/* answers 403
	// password_change_required until POST /auth/password.
	MustChangePassword bool `json:"mustChangePassword,omitempty" doc:"The local user must change the password (POST /auth/password) before using the API."`
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

// AuditEvent is one row of GET /audit.
type AuditEvent = audit.Event

type auditInput struct {
	Kind   string `query:"kind" doc:"Resource kind (DNSZone, blocking, ...)."`
	Name   string `query:"name"`
	Actor  string `query:"actor" doc:"Actor name or <kind>:<name>."`
	Since  string `query:"since" doc:"RFC 3339."`
	Limit  int    `query:"limit" minimum:"0" maximum:"1000"`
	Cursor string `query:"cursor" doc:"cursor_next (or continue) of the previous page, read with the same order."`
	// Continue is the list-envelope name of Cursor.
	Continue string `query:"continue" doc:"Same as cursor."`
	Order    string `query:"order" enum:"asc,desc" default:"asc" doc:"asc: oldest first (the default); desc: newest first."`
}

// AuditPage is GET /audit: oldest first (newest first with order=desc),
// keyset cursor on the event id.
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
	// check; the routes therefore need a principal only here, and the auth
	// handler checks tokens.own at any scope the caller holds and scopes
	// them to the caller. A token never carries more than its owner's
	// bindings (§8.6).
	tokens := authed
	bounded := func(method, path, id, summary, tag string) huma.Operation {
		op := operation(method, path, id, summary, tag)
		op.MaxBodyBytes = 16 << 10
		return op
	}
	s.forward(operation(http.MethodGet, "/auth/status", "authStatus", "Login options", "auth"), public, nil, AuthStatus{})
	s.forward(withRateLimit(operation(http.MethodPost, "/auth/login", "login", "Local login (sets the session cookie)", "auth")), public, LoginRequest{}, SessionInfo{})
	start := operation(http.MethodGet, "/auth/oidc/{idp}/start", "oidcStart", "Start an OIDC login (302 to the IdP)", "auth")
	start.Parameters = append(start.Parameters, &huma.Param{Name: "next", In: "query", Description: "Same-origin path to return to after the login.", Schema: &huma.Schema{Type: "string"}})
	s.forward(start, public, nil, nil)
	s.forward(operation(http.MethodGet, "/auth/oidc/{idp}/callback", "oidcCallback", "OIDC redirect target (302 to the app)", "auth"), public, nil, nil)
	s.forward(withStatus(operation(http.MethodGet, "/auth/oidc/{idp}/logout", "oidcLogout", "Continue a committed browser logout (302 to the IdP)", "auth"), http.StatusFound), public, nil, nil)
	s.forward(withStatus(operation(http.MethodGet, "/auth/oidc/{idp}/logout/callback", "oidcLogoutCallback", "Validate the logout return state (302 to the app)", "auth"), http.StatusFound), public, nil, nil)
	s.forward(operation(http.MethodPost, "/auth/logout/browser", "browserLogout", "Revoke the session and optionally continue provider logout", "auth"), authed, nil, BrowserLogoutResponse{})
	s.forward(withStatus(operation(http.MethodPost, "/auth/logout", "logout", "Clear the session cookie", "auth"), http.StatusNoContent), authed, nil, nil)
	s.forward(operation(http.MethodPost, "/auth/password", "changePassword", "Change the local user's own password (a cookie session gets a new cookie)", "auth"), authed, PasswordChangeRequest{}, SessionInfo{})
	s.forward(operation(http.MethodGet, "/auth/sessions", "listSessions", "List own enrolled sessions", "auth"), authed, nil, AuthList[SessionRecordInfo]{})
	s.forward(withStatus(operation(http.MethodDelete, "/auth/sessions/{id}", "revokeSession", "Revoke an enrolled session of the current account", "auth"), http.StatusNoContent), authed, nil, nil)
	s.forward(bounded(http.MethodGet, "/auth/totp", "totpStatus", "Own local authenticator status", "auth"), authed, nil, TOTPStatus{})
	s.forward(withRateLimit(bounded(http.MethodPost, "/auth/totp/setup", "beginTOTP", "Prepare an authenticator (provisioning secret returned once)", "auth")), authed, TOTPProofRequest{}, TOTPSetupResponse{})
	s.forward(withStatus(withRateLimit(bounded(http.MethodPost, "/auth/totp/confirm", "confirmTOTP", "Activate the authenticator and revoke existing sessions", "auth")), http.StatusNoContent), authed, TOTPConfirmRequest{}, nil)
	s.forward(withStatus(withRateLimit(bounded(http.MethodPost, "/auth/totp/disable", "disableTOTP", "Disable own authenticator and revoke existing sessions", "auth")), http.StatusNoContent), authed, TOTPProofRequest{}, nil)
	s.forward(withStatus(operation(http.MethodPost, "/auth/tokens", "createToken", "Create a personal API token (the secret is returned once)", "auth"), http.StatusCreated), tokens, CreateTokenRequest{}, CreatedToken{})
	list := operation(http.MethodGet, "/auth/tokens", "listTokens", "List own API tokens", "auth")
	list.Parameters = append(list.Parameters, &huma.Param{Name: "all", In: "query", Description: "true lists every owner's tokens (needs tokens.read).", Schema: &huma.Schema{Type: "boolean"}})
	s.forward(list, tokens, nil, AuthList[TokenInfo]{})
	s.forward(operation(http.MethodGet, "/auth/tokens/{id}", "getToken", "Get one API token (someone else's needs tokens.read)", "auth"), tokens, nil, TokenInfo{})
	s.forward(withStatus(operation(http.MethodDelete, "/auth/tokens/{id}", "deleteToken", "Revoke one API token (someone else's needs tokens.delete)", "auth"), http.StatusNoContent), tokens, nil, nil)
	register(s, operation(http.MethodGet, "/auth/me", "me", "The authenticated principal, roles, permissions and CSRF token", "auth"), authed, s.me)

	// IAM inventory and explicitly supported commands use the auth handler.
	iam := func(path, resource, id string, resp any) {
		s.forward(operation(http.MethodGet, path, id, "List "+resource, "iam"),
			perm{resource: resource, verb: "read"}, nil, resp)
	}
	iam("/iam/users", "users", "listUsers", AuthList[IAMUser]{})
	createUser := withStatus(bounded(http.MethodPost, "/iam/users", "createLocalUser", "Create an API-managed local user requiring a password change", "iam"), http.StatusCreated)
	createUser.Description = "Creates an ordinary local account with no groups or authenticator. Requires the complete SQLite runtime; existing accounts and other managers cannot be adopted. No session or token is issued."
	s.forward(createUser, perm{resource: "users", verb: "write"}, CreateLocalUserRequest{}, IAMUser{})
	resetUser := withStatus(bounded(http.MethodPost, "/iam/users/{name}/password", "resetLocalUserPassword", "Reset an exact API-managed local account's password", "iam"), http.StatusNoContent)
	resetUser.Description = "Requires exact UID and resourceVersion. Preserves the authenticator and disabled state, requires a password change, revokes enrolled sessions, and deletes exact-owned local personal tokens atomically. Built-in and non-API-managed accounts are excluded. No automatic retry or replacement session."
	s.forward(resetUser, perm{resource: "users", verb: "write"}, ResetLocalUserPasswordRequest{}, nil)
	s.forward(withStatus(bounded(http.MethodPost, "/iam/users/{name}/totp/recover", "recoverTOTP", "Disable a lost authenticator for an exact account UID", "iam"), http.StatusNoContent),
		perm{resource: "users", verb: "write"}, TOTPRecoverRequest{}, nil)
	iam("/iam/serviceaccounts", "serviceaccounts", "listServiceAccounts", AuthList[ServiceAccountInfo]{})
	iam("/iam/rolebindings", "rolebindings", "listRoleBindings", AuthList[RoleBindingInfo]{})
	iam("/iam/identityproviders", "identityproviders", "listIdentityProviders", AuthList[IdentityProviderInfo]{})
}

func withRateLimit(op huma.Operation) huma.Operation {
	op.Description = "Brute-force limited per client address and user: 429 with Retry-After."
	op.Errors = append(op.Errors, http.StatusTooManyRequests)
	return op
}

// forward registers a route served by Options.AuthHandler: the operation
// is documented (request and response schemas from the given examples,
// the mirrors of authtypes.go)
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
	if op.MaxBodyBytes == 0 || op.MaxBodyBytes > maxAuthBodyBytes {
		op.MaxBodyBytes = maxAuthBodyBytes
	}
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
		// huma documents a RawBody as a binary string: put the schema of
		// the body the auth handler decodes back.
		if doc := documented(s.api.OpenAPI(), op.Method, op.Path); doc != nil {
			doc.RequestBody.Content["application/json"] = &huma.MediaType{Schema: reg.Schema(reflect.TypeOf(reqExample), true, "")}
		}
		return
	}
	register(s, op, p, func(_ context.Context, _ *struct{}) (*huma.StreamResponse, error) { return serve(nil) })
}

// documented returns the OpenAPI operation registered for method and path.
func documented(oapi *huma.OpenAPI, method, path string) *huma.Operation {
	item := oapi.Paths[path]
	if item == nil {
		return nil
	}
	switch method {
	case http.MethodGet:
		return item.Get
	case http.MethodPost:
		return item.Post
	case http.MethodPut:
		return item.Put
	case http.MethodPatch:
		return item.Patch
	case http.MethodDelete:
		return item.Delete
	}
	return nil
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
		Principal:          MePrincipal{Kind: string(p.Kind), ID: p.ID, Name: p.Name, Groups: p.Groups, Auth: p.AuthMethod},
		Roles:              []string{},
		Permissions:        map[string][]string{},
		MustChangePassword: p.MustChangePassword,
	}
	if out.Principal.Groups == nil {
		out.Principal.Groups = []string{}
	}
	sess, hasSession := auth.SessionFrom(ctx)
	if hasSession {
		out.CSRF = sess.CSRF
	}
	// A local user's session carries no roles snapshot (only OIDC sessions
	// freeze their grants, D48), so the live grants are the answer for
	// every principal; Options.Roles honours a snapshot while its bindings
	// still provide it.
	switch {
	case s.opts.Roles != nil:
		out.Roles = append(out.Roles, s.opts.Roles(ctx, p)...)
	case hasSession:
		out.Roles = append(out.Roles, sess.RolesSnapshot...)
	}
	if p.MustChangePassword {
		// Every permission route answers 403 password_change_required.
		return &meOutput{Body: out}, nil
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

func (s *Server) registerAudit() {
	register(s, operation(http.MethodGet, "/audit/verify", "verifyAudit", "Verify the complete retained audit chain and available batch seals", "audit"),
		perm{resource: "audit", verb: "read"}, s.verifyAudit)
	register(s, operation(http.MethodGet, "/audit", "listAudit", "Audit events of this node (oldest first; order=desc for newest first)", "audit"),
		perm{resource: "audit", verb: "read"}, s.listAudit)
	exportOp := operation(http.MethodGet, "/audit/export", "exportAudit", "Audit events as JSON lines", "audit")
	exportOp.Responses = map[string]*huma.Response{"200": {Description: "application/x-ndjson, one AuditEvent per line",
		Content: map[string]*huma.MediaType{"application/x-ndjson": {Schema: &huma.Schema{Type: "string"}}}}}
	register(s, exportOp, perm{resource: "audit", verb: "export"}, s.exportAudit)
}

// AuditVerification is the result of checking the retained store, independent
// of the filters or pagination of GET /audit. Storage failures answer 503;
// Verified=false reports an integrity failure. Results are cached for 30 seconds.
type AuditVerification struct {
	Verified  bool      `json:"verified"`
	CheckedAt time.Time `json:"checkedAt"`
	Reason    string    `json:"reason,omitempty"`
}

type auditVerifyOutput struct{ Body *AuditVerification }

func (s *Server) auditReader() (audit.Reader, error) {
	if err := s.requireFeature(license.FeatureAudit, "the audit trail"); err != nil {
		return nil, err
	}
	if s.opts.AuditReader != nil {
		return s.opts.AuditReader, nil
	}
	if rd, ok := audit.ReaderOf(s.opts.Audit); ok {
		return rd, nil
	}
	return nil, errAuditNotStored()
}

// auditError maps audit reader failures: a cursor another store issued is
// a 400.
func auditError(err error) error {
	if errors.Is(err, audit.ErrBadCursor) {
		return errInvalid("cursor: not a cursor returned by this API")
	}
	return storeError(err)
}

// errAuditNotStored is the 503 of the audit routes when no recorder
// stores events (audit.ErrNotStored).
func errAuditNotStored() *Error {
	return newError(http.StatusServiceUnavailable, KeyUnavailable, "audit events are not stored on this node",
		"the sqlite store keeps them in state.db; in Kubernetes they are the \"log\":\"audit\" lines")
}

func auditQuery(in *auditInput) (audit.Query, error) {
	q := audit.Query{Kind: in.Kind, Name: in.Name, Actor: in.Actor, Limit: in.Limit, Desc: in.Order == "desc"}
	if in.Since != "" {
		t, err := time.Parse(time.RFC3339, in.Since)
		if err != nil {
			return q, errInvalid("since: %v", err)
		}
		q.Since = t
	}
	cursor := in.Cursor
	if cursor == "" {
		cursor = in.Continue
	}
	if cursor != "" {
		// A decimal cursor is an event id; anything else is the opaque
		// cursor of a cluster-wide store, which checks it itself.
		id, err := strconv.ParseInt(cursor, 10, 64)
		switch {
		case err != nil:
			q.Cursor = cursor
		case id < 0 || q.Desc && id == 0:
			return q, errInvalid("cursor: not a cursor returned by this API")
		default:
			q.After(audit.Event{ID: id})
		}
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
		return nil, auditError(err)
	}
	out := &AuditPage{Items: make([]AuditEvent, len(evs)), Node: s.opts.NodeName}
	for i, ev := range evs {
		out.Items[i] = audit.RedactDocuments(ev)
	}
	if out.Items == nil {
		out.Items = []AuditEvent{}
	}
	if len(evs) == limit {
		out.CursorNext = evs[len(evs)-1].Position()
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
				if enc.Encode(audit.RedactDocuments(ev)) != nil {
					return
				}
			}
			if len(evs) < q.Limit {
				return
			}
			q.After(evs[len(evs)-1])
		}
	}}, nil
}
