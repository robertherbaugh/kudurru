package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// perm is the permission a route declares (decision D31). Exactly one of
// public, authenticated, resource or byKind is set.
type perm struct {
	resource string
	verb     string
	// byKind derives the resource from the {kind} path parameter
	// (dnszones → zones, dnsrecordsets → records, ...).
	byKind bool
	// kindResources lists the resources a byKind or multi-kind route may
	// check, for the lint.
	kindResources []string
	// scope derives the attribute scope from the request (zone:<name>);
	// nil means cluster.
	scope func(huma.Context) rbac.Scope
	// scopable routes defer a cluster-scope denial to the handler, which
	// authorizes each object at its own scope (zone-admin) or filters
	// lists.
	scopable bool
	// public routes are served to anonymous callers; authenticated routes
	// need a principal but no permission.
	public        bool
	authenticated bool
}

// zoneScope scopes a route to the zone named by the path parameter.
func zoneScope(param string) func(huma.Context) rbac.Scope {
	return func(ctx huma.Context) rbac.Scope {
		return rbac.Scope{Kind: rbac.ScopeZone, Name: ctx.Param(param)}
	}
}

var clusterScope = rbac.Scope{Kind: rbac.ScopeCluster}

// scopeString renders a scope as "cluster" or "<kind>:<name>".
func scopeString(sc rbac.Scope) string {
	if sc.Kind == "" || sc.Kind == rbac.ScopeCluster {
		return string(rbac.ScopeCluster)
	}
	return string(sc.Kind) + ":" + sc.Name
}

// rbacPrincipal derives the authorizer's view of a principal. Kubernetes
// service accounts ("system:serviceaccount:<ns>:<name>") and kudurru
// service accounts ("<ns>/<name>" or a bare name) become
// ServiceAccount "<ns>/<name>" for binding subjects.
func rbacPrincipal(p auth.Principal) rbac.Principal {
	// auth.RBACPrincipal carries what the authorizer needs beyond name and
	// groups: the builtin-admin flag, grants frozen in a session or token,
	// implicit grants and token scopes (restrictions).
	out := auth.RBACPrincipal(p)
	if out.ServiceAccount != "" {
		return out
	}
	switch p.Kind {
	case auth.KindKubernetesServiceAccount:
		if rest, ok := strings.CutPrefix(p.Name, "system:serviceaccount:"); ok {
			if ns, name, ok := strings.Cut(rest, ":"); ok {
				out.ServiceAccount = ns + "/" + name
			}
		}
	case auth.KindServiceAccount:
		if strings.Contains(p.Name, "/") {
			out.ServiceAccount = p.Name
		} else {
			out.ServiceAccount = "/" + p.Name
		}
	}
	return out
}

// actorOf is the audit actor of a principal.
func actorOf(p auth.Principal, ok bool) audit.Actor {
	if !ok {
		return audit.Actor{}
	}
	return audit.Actor{Kind: string(p.Kind), ID: p.ID, Name: p.Name, Groups: p.Groups, Auth: p.AuthMethod}
}

// actorMW records the principal as the actor of store writes (the sqlite
// driver chains them in its audit table, §11.4).
func (s *Server) actorMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := auth.PrincipalFrom(r.Context()); ok {
			actor := actorOf(p, true)
			ctx := store.WithActor(r.Context(), actor.String())
			st := stateFrom(ctx)
			ctx = audit.WithRequest(ctx, actor, audit.Request{ID: st.id, IP: st.clientIP.String(), Method: st.method, Path: st.path, UA: st.userAgent}, func() string { return st.authz })
			r = r.WithContext(ctx)
		}
		next.ServeHTTP(w, r)
	})
}

// authorize asks the authorizer; a nil authorizer denies.
func (s *Server) authorize(ctx context.Context, p auth.Principal, resource, verb string, sc rbac.Scope, name string) (rbac.Decision, error) {
	return s.authorizeGroup(ctx, p, resource, verb, sc, name, s.zoneGroup)
}

// zoneGroupResolver states the zone group of the zone named name: "" for a
// zone in no group or an absent zone, an error when it cannot be read.
type zoneGroupResolver func(ctx context.Context, name string) (string, error)

// storedZoneGroup is the resolver of a caller that holds the zone it
// authorizes for: the group of that read, never another one.
func storedZoneGroup(z *v1alpha1.DNSZone) zoneGroupResolver {
	return func(context.Context, string) (string, error) { return v1alpha1.ZoneGroupOf(z), nil }
}

// authorizeGroup asks the authorizer with the zone group of a zone scope
// taken from resolve (D385). The group is resolved only when the decision
// without it is a denial: Scope.Admits only adds what a zonegroup scope
// covers, so a grant without the group is a grant with it, and principals
// whose cluster or zone bindings already allow never cost a zone read.
func (s *Server) authorizeGroup(ctx context.Context, p auth.Principal, resource, verb string, sc rbac.Scope, name string, resolve zoneGroupResolver) (rbac.Decision, error) {
	if p.Kind == auth.KindLocalRoot {
		return rbac.Decision{Allowed: true, Path: "local"}, nil
	}
	a := rbac.Attributes{Resource: resource, Verb: verb, Scope: sc, Name: name}
	if s.opts.Authorizer == nil {
		return rbac.Decision{Path: "none", Missing: &a, Reason: "no authorizer configured"}, nil
	}
	principal := rbacPrincipal(p)
	dec, err := s.opts.Authorizer.Authorize(ctx, principal, a)
	if err != nil || dec.Allowed || sc.Kind != rbac.ScopeZone || sc.Name == "" {
		return dec, err
	}
	group, err := resolve(ctx, sc.Name)
	if err != nil {
		return rbac.Decision{}, err
	}
	if group == "" {
		return dec, nil
	}
	a.ZoneGroup = group
	return s.opts.Authorizer.Authorize(ctx, principal, a)
}

// zoneGroup resolves the zone group of the DNSZone named name for one
// authorization decision (D385): from this request's captured publication
// when the runtime serves captured zone reads (the same cut the handler then
// reads), otherwise from the store. The result is kept for the rest of the
// request, so a list filter or GET /auth/me reads each zone once. An absent
// zone is in no group, so a zonegroup binding grants nothing for it; a zone
// that cannot be read is an error (the caller answers 503, not a denial).
// Zone and cluster bindings do not depend on it. A command transaction
// resolves the group again from its own read and ignores this value.
func (s *Server) zoneGroup(ctx context.Context, name string) (string, error) {
	st := stateFrom(ctx)
	if group, ok := st.knownZoneGroup(name); ok {
		return group, nil
	}
	group := ""
	switch {
	case s.opts.ZoneReads != nil:
		objects, err := s.opts.ZoneReads.ZoneObjects(ctx)
		if err != nil {
			return "", fmt.Errorf("zone group of %s: %w", name, err)
		}
		z, _ := objects.Zone(name)
		group = v1alpha1.ZoneGroupOf(z)
	case s.opts.Store != nil:
		var z v1alpha1.DNSZone
		if err := s.opts.Store.Get(ctx, store.Key{Kind: "DNSZone", Name: name}, &z); err == nil {
			group = v1alpha1.ZoneGroupOf(&z)
		} else if !errors.Is(err, store.ErrNotFound) {
			return "", fmt.Errorf("zone group of %s: %w", name, err)
		}
	}
	st.noteZoneGroup(name, group)
	return group, nil
}

// noteZoneGroups records the zone group of each listed zone for this
// request, so the list filter decides each zone on the object it shows
// instead of reading it again.
func noteZoneGroups(ctx context.Context, zones []*v1alpha1.DNSZone) {
	st := stateFrom(ctx)
	for _, z := range zones {
		st.noteZoneGroup(z.Name, v1alpha1.ZoneGroupOf(z))
	}
}

// require authorizes resource.verb at sc for the request principal and
// returns a 401/403 envelope error when it is not allowed. Handlers of
// scopable routes call it per object.
func (s *Server) require(ctx context.Context, resource, verb string, sc rbac.Scope, name string) error {
	return s.requireGroup(ctx, resource, verb, sc, name, s.zoneGroup)
}

// requireGroup is require with the zone group of a zone scope taken from
// resolve.
func (s *Server) requireGroup(ctx context.Context, resource, verb string, sc rbac.Scope, name string, resolve zoneGroupResolver) error {
	p, ok := auth.PrincipalFrom(ctx)
	if !ok {
		return errUnauthenticated()
	}
	st := stateFrom(ctx)
	if st.clusterAllowed && st.allowedResource == resource && st.allowedVerb == verb {
		return nil
	}
	dec, err := s.authorizeGroup(ctx, p, resource, verb, sc, name, resolve)
	if _, current := auth.PrincipalFrom(ctx); !current {
		return errUnauthenticated()
	}
	if err != nil {
		return errUnavailable("authorizer: %v", err)
	}
	if dec.Allowed {
		if st.authz == "" {
			st.authz = dec.Path
		}
		return nil
	}
	s.auditDenied(ctx, resource, verb, sc, name, dec)
	return errForbidden(Missing{Resource: resource, Verb: verb, Scope: scopeString(sc)}, "")
}

// allowed reports whether the principal holds resource.verb at sc,
// without auditing (list filtering).
func (s *Server) allowed(ctx context.Context, resource, verb string, sc rbac.Scope) bool {
	p, ok := auth.PrincipalFrom(ctx)
	if !ok {
		return false
	}
	st := stateFrom(ctx)
	if st.clusterAllowed && st.allowedResource == resource && st.allowedVerb == verb {
		return true
	}
	dec, err := s.authorize(ctx, p, resource, verb, sc, "")
	_, current := auth.PrincipalFrom(ctx)
	return current && err == nil && dec.Allowed
}

// auditDenied records a denied attempt (§11.4: denied attempts are audited
// from the authz layer).
func (s *Server) auditDenied(ctx context.Context, resource, verb string, sc rbac.Scope, name string, dec rbac.Decision) {
	res := audit.Resource{Kind: resource, Name: name}
	if name == "" && sc.Kind != "" && sc.Kind != rbac.ScopeCluster {
		res.Name = sc.Name
	}
	reason := dec.Reason
	if reason == "" {
		reason = "not allowed"
	}
	s.record(ctx, audit.Event{
		Action: resource + "." + verb, Resource: res, Result: audit.ResultDenied,
		Reason: reason + " (scope " + scopeString(sc) + ")", Authz: dec.Path,
	})
}

// record fills the request context of ev and hands it to the recorder.
func (s *Server) record(ctx context.Context, ev audit.Event) {
	if s.opts.Audit == nil {
		return
	}
	p, ok := auth.PrincipalFrom(ctx)
	ev.Actor = actorOf(p, ok)
	ev.Node = s.opts.NodeName
	if ev.Time.IsZero() {
		ev.Time = s.opts.Now()
	}
	st := stateFrom(ctx)
	if ev.Authz == "" {
		ev.Authz = st.authz
	}
	ev.Request = audit.Request{ID: st.id, IP: st.clientIP.String(), Method: st.method, Path: st.path, UA: st.userAgent}
	if err := s.opts.Audit.Record(context.WithoutCancel(ctx), audit.Sanitize(ev)); err != nil {
		s.log.Warn("audit record failed", "action", ev.Action, "err", err)
	}
}

// recordAction audits a successful or failed non-store mutation.
func (s *Server) recordAction(ctx context.Context, action string, res audit.Resource, err error, before, after any) {
	ev := audit.Event{Action: action, Resource: res, Result: audit.ResultSuccess, Before: audit.Document(res.Kind, before), After: audit.Document(res.Kind, after)}
	if err != nil {
		ev.Result, ev.Reason = audit.ResultError, err.Error()
	}
	s.record(ctx, ev)
}

func unsafeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return false
	}
	return true
}

// authorizeMW is the Authorize step of §8.8 for one operation: 401 without
// a principal (403 for a dropped session's CSRF failure), the CSRF check
// for cookie sessions, then the route's declared permission (403 with
// missing{} otherwise).
func (s *Server) authorizeMW(p perm) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		op := ctx.Operation()
		st := stateFrom(ctx.Context())
		st.route = op.Method + " " + BasePath + op.Path
		if p.public {
			next(ctx)
			return
		}
		pr, ok := auth.PrincipalFrom(ctx.Context())
		if !ok {
			if e := auth.CSRFFailure(ctx.Context()); e != nil {
				s.writeHumaError(ctx, &Error{Status: e.Status, Body: ErrorBody{Key: e.Key, Message: e.Message, Hint: e.Hint}})
			} else {
				s.writeHumaError(ctx, errUnauthenticated())
			}
			return
		}
		if s.opts.CSRF != nil && pr.AuthMethod == "cookie" && unsafeMethod(ctx.Method()) {
			r, _ := humago.Unwrap(ctx)
			if err := s.opts.CSRF(r); err != nil {
				s.writeHumaError(ctx, newError(http.StatusForbidden, KeyCSRF, "CSRF check failed: "+err.Error(),
					"send the X-Kudurru-CSRF header with the csrf value from GET /api/v1/auth/me"))
				return
			}
		}
		if p.authenticated {
			next(ctx)
			return
		}
		if pr.MustChangePassword {
			// Only the self-service routes (authenticated: me, password,
			// logout) serve a local user who must change the password.
			s.writeHumaError(ctx, newError(http.StatusForbidden, KeyPasswordChange, "change your password before using the API",
				"POST "+BasePath+"/auth/password"))
			return
		}
		resource := p.resource
		if p.byKind {
			k, ok := kindByPlural(ctx.Param("kind"))
			if !ok {
				s.writeHumaError(ctx, errNotFound("kind "+ctx.Param("kind")))
				return
			}
			resource = k.resource
		}
		if resource == "" && p.scopable {
			// Multi-kind routes (POST /apply) authorize every document in
			// the handler.
			next(ctx)
			return
		}
		sc := clusterScope
		if p.scope != nil {
			sc = p.scope(ctx)
		}
		dec, err := s.authorize(ctx.Context(), pr, resource, p.verb, sc, "")
		// A delegated SAR may cross this held proof's original expiry.
		if _, current := auth.PrincipalFrom(ctx.Context()); !current {
			s.writeHumaError(ctx, errUnauthenticated())
			return
		}
		if err != nil {
			s.writeHumaError(ctx, errUnavailable("authorizer: %v", err))
			return
		}
		switch {
		case dec.Allowed:
			st.authz = dec.Path
			st.clusterAllowed = sc.Kind == rbac.ScopeCluster
			st.allowedResource, st.allowedVerb = resource, p.verb
			next(ctx)
		case p.scopable:
			next(ctx)
		default:
			s.auditDenied(ctx.Context(), resource, p.verb, sc, "", dec)
			s.writeHumaError(ctx, errForbidden(Missing{Resource: resource, Verb: p.verb, Scope: scopeString(sc)}, ""))
		}
	}
}

// writeHumaError writes the envelope from a huma middleware.
func (s *Server) writeHumaError(ctx huma.Context, e *Error) {
	e.TookMS = tookMS(ctx.Context())
	for k, vs := range e.headers {
		for _, v := range vs {
			ctx.AppendHeader(k, v)
		}
	}
	ctx.SetHeader("Content-Type", "application/json")
	ctx.SetStatus(e.Status)
	enc := json.NewEncoder(ctx.BodyWriter())
	enc.SetEscapeHTML(false)
	_ = enc.Encode(e)
}
