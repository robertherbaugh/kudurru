package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/robertherbaugh/kudurru/internal/rbac"
)

// permKey is the operation metadata key holding the route's perm.
const permKey = "kudurru.perm"

// operation builds a huma operation. The id becomes the operationId
// (and the generated clients' method names).
func operation(method, path, id, summary string, tags ...string) huma.Operation {
	return huma.Operation{Method: method, Path: path, OperationID: id, Summary: summary, Tags: tags}
}

// register adds one operation with its permission declaration: the
// Authorize middleware, the rbac.Route for the lint, the
// x-kudurru-permission extension in the OpenAPI document, and the body
// cap.
func register[I, O any](s *Server, op huma.Operation, p perm, h func(context.Context, *I) (*O, error)) {
	s.declare(&op, p)
	huma.Register(s.api, op, h)
	// Routes that create or replace document 201 Created next to 200.
	if op.Method == http.MethodPut && strings.Contains(op.Path, "{name}") {
		if item := s.api.OpenAPI().Paths[op.Path]; item != nil && item.Put != nil {
			if ok := item.Put.Responses["200"]; ok != nil && item.Put.Responses["201"] == nil {
				created := *ok
				created.Description = "Created"
				item.Put.Responses["201"] = &created
			}
		}
	}
}

func (s *Server) declare(op *huma.Operation, p perm) {
	if op.Metadata == nil {
		op.Metadata = map[string]any{}
	}
	op.Metadata[permKey] = p
	op.Middlewares = append(op.Middlewares, s.authorizeMW(p))
	if op.MaxBodyBytes == 0 {
		op.MaxBodyBytes = maxBodyBytes
	}
	if op.Extensions == nil {
		op.Extensions = map[string]any{}
	}
	route := rbac.Route{Method: op.Method, Path: BasePath + op.Path, Public: p.public, Authenticated: p.authenticated}
	switch {
	case p.public:
		op.Extensions["x-kudurru-permission"] = "public"
	case p.authenticated:
		op.Extensions["x-kudurru-permission"] = "authenticated"
	case p.byKind || len(p.kindResources) > 0:
		route.Resources, route.Verb = p.kindResources, p.verb
		if len(route.Resources) == 0 {
			route.Resources = kindResources()
		}
		op.Extensions["x-kudurru-permission"] = map[string]any{"resources": route.Resources, "verb": p.verb, "scope": scopeDoc(p)}
	default:
		route.Resource, route.Verb = p.resource, p.verb
		op.Extensions["x-kudurru-permission"] = map[string]any{"resource": p.resource, "verb": p.verb, "scope": scopeDoc(p)}
	}
	s.routes = append(s.routes, route)
	if !p.public {
		addErrorResponses(op, http.StatusUnauthorized, http.StatusForbidden)
	}
	// The envelope statuses every handler may answer (§10).
	switch op.Method {
	case http.MethodGet:
		addErrorResponses(op, http.StatusBadRequest, http.StatusNotFound, http.StatusServiceUnavailable)
	default:
		addErrorResponses(op, http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable)
		if op.Method == http.MethodPut || op.Method == http.MethodDelete {
			addErrorResponses(op, http.StatusPreconditionFailed)
		}
	}
}

func scopeDoc(p perm) string {
	switch {
	case p.scope != nil:
		return "zone"
	case p.scopable:
		return "cluster or per object"
	}
	return "cluster"
}

// addErrorResponses documents error statuses with the envelope schema.
func addErrorResponses(op *huma.Operation, codes ...int) {
	op.Errors = append(op.Errors, codes...)
}

// registerRoutes registers every route group.
func (s *Server) registerRoutes() {
	s.registerMeta()
	s.registerAuth()
	s.registerObjects()
	s.registerImports()
	s.registerZones()
	s.registerLists()
	s.registerDomainLists()
	s.registerCatalogue()
	s.registerPolicies()
	s.registerRuntime()
	s.registerStats()
	s.registerSettings()
	s.registerBlockPage()
	s.registerUnblock()
	s.registerNodes()
	s.registerLicense()
	s.registerDoctor()
	s.registerAudit()
}

// registerMeta declares the routes huma serves itself (the OpenAPI
// document) and the docs page with its bundle so the lint sees them, and
// GET /version.
func (s *Server) registerMeta() {
	for _, path := range []string{"/openapi.json", "/openapi.yaml", "/openapi-3.0.json", "/openapi-3.0.yaml", docsPath, scalarPath} {
		s.routes = append(s.routes, rbac.Route{Method: http.MethodGet, Path: BasePath + path, Public: true})
	}
	s.registerDocs()
	register(s, operation(http.MethodGet, "/version", "getVersion", "Build information of this node", "meta"),
		perm{public: true}, func(_ context.Context, _ *struct{}) (*versionOutput, error) {
			out := &versionOutput{}
			out.Body.Version = s.opts.Version.Version
			out.Body.Commit = s.opts.Version.Commit
			out.Body.Date = s.opts.Version.Date
			out.Body.Go = s.opts.Version.GoVersion
			out.Body.Platform = s.opts.Version.Platform
			out.Body.Node = s.opts.NodeName
			out.Body.API = APIVersion
			return out, nil
		})
}

type versionOutput struct {
	Body struct {
		Version  string `json:"version"`
		Commit   string `json:"commit"`
		Date     string `json:"date"`
		Go       string `json:"go"`
		Platform string `json:"platform"`
		Node     string `json:"node"`
		API      string `json:"api" doc:"API version (the /api/v1 contract)."`
		Took
	}
}
