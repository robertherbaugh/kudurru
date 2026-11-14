package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"reflect"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RoleBindingMetadata contains operator-controlled metadata only. Incarnation,
// revision, ownership and lifecycle metadata come from the current transaction.
type RoleBindingMetadata struct {
	Name        string            `json:"name" minLength:"1" maxLength:"253"`
	Namespace   string            `json:"namespace" minLength:"1" maxLength:"63"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// RoleBindingFields uses the existing IAM subject/scope contract. Its wire shape
// is identical to the public kind without colliding with Kubernetes type names.
type RoleBindingFields struct {
	Role     string           `json:"role" minLength:"1" maxLength:"63"`
	Scope    BindingScope     `json:"scope,omitempty"`
	Subjects []BindingSubject `json:"subjects" minItems:"1" maxItems:"256"`
}

// RoleBindingDocument includes the exact server-owned metadata for review.
type RoleBindingDocument struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   metav1.ObjectMeta `json:"metadata"`
	Spec       RoleBindingFields `json:"spec"`
}

// RoleBindingDesired is a public RoleBinding manifest without server metadata.
type RoleBindingDesired struct {
	APIVersion string              `json:"apiVersion"`
	Kind       string              `json:"kind"`
	Metadata   RoleBindingMetadata `json:"metadata"`
	Spec       RoleBindingFields   `json:"spec"`
}

// RoleBindingReview requires the exact independently reviewed incarnation and
// revision. A conflict requires a new operator decision, never an automatic read.
type RoleBindingReview struct {
	UID             string `json:"uid" minLength:"1" maxLength:"128"`
	ResourceVersion string `json:"resourceVersion" minLength:"1" maxLength:"128"`
}

// ReplaceRoleBindingCommand carries desired state and explicit target review.
type ReplaceRoleBindingCommand struct {
	RoleBindingReview
	Binding RoleBindingDesired `json:"binding"`
}

// RoleBindingCommandRuntime is mounted only by the complete atomic SQLite
// runtime. Legacy and Kubernetes handlers must not fall back to ordinary CRUD.
type RoleBindingCommandRuntime interface{ RoleBindingCommandsAvailable() bool }

// forwardRoleBinding performs capability admission after the ordinary declared
// permission middleware. The body is preserved byte-for-byte for strict review.
func (s *Server) forwardRoleBinding(op huma.Operation, p perm, req, resp any) {
	s.forwardReviewedCommand(op, p, req, resp, func() error {
		runtime, ok := s.opts.CanonicalSettings.(RoleBindingCommandRuntime)
		if !ok || !runtime.RoleBindingCommandsAvailable() || s.opts.AuthHandler == nil {
			return newError(http.StatusServiceUnavailable, KeyUnavailable, "public RoleBinding command unavailable", "requires the complete SQLite runtime")
		}
		return nil
	})
}

// forwardReviewedCommand registers a reviewed IAM command that the complete
// SQLite runtime serves. available runs after authentication and the declared
// permission, before the auth handler sees the body; it refuses every runtime
// that does not offer the command. The body is preserved byte-for-byte.
func (s *Server) forwardReviewedCommand(op huma.Operation, p perm, req, resp any, available func() error) {
	reg := s.api.OpenAPI().Components.Schemas
	schema := reg.Schema(reflect.TypeOf(req), true, "")
	op.RequestBody = &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{"application/json": {Schema: schema}}}
	status := op.DefaultStatus
	if status == 0 {
		status = http.StatusOK
	}
	response := &huma.Response{Description: http.StatusText(status)}
	if resp != nil {
		response.Content = map[string]*huma.MediaType{"application/json": {Schema: reg.Schema(reflect.TypeOf(resp), true, "")}}
	}
	op.Responses = map[string]*huma.Response{strconv.Itoa(status): response}
	op.SkipValidateBody = true
	op.MaxBodyBytes = 64 << 10
	for _, m := range pathParamRE.FindAllStringSubmatch(op.Path, -1) {
		op.Parameters = append(op.Parameters, &huma.Param{Name: m[1], In: "path", Required: true, Schema: &huma.Schema{Type: "string"}})
	}
	register(s, op, p, func(_ context.Context, in *forwardBodyInput) (*huma.StreamResponse, error) {
		if err := available(); err != nil {
			return nil, err
		}
		return &huma.StreamResponse{Body: func(ctx huma.Context) {
			r, w := humago.Unwrap(ctx)
			r.Body = io.NopCloser(bytes.NewReader(in.RawBody))
			r.ContentLength = int64(len(in.RawBody))
			s.opts.AuthHandler.ServeHTTP(w, r)
		}}, nil
	})
	// RawBody is transport plumbing; expose the reviewed command schema.
	if doc := documented(s.api.OpenAPI(), op.Method, op.Path); doc != nil {
		doc.RequestBody.Content["application/json"] = &huma.MediaType{Schema: schema}
	}
}

func (s *Server) registerRoleBindings() {
	path := "/iam/rolebindings/{namespace}/{name}"
	s.forward(operation(http.MethodGet, path, "getRoleBinding", "Read a public RoleBinding and its current UID/resourceVersion", "iam"), perm{resource: "rolebindings", verb: "read"}, nil, RoleBindingDocument{})
	create := withStatus(operation(http.MethodPost, "/iam/rolebindings", "createRoleBinding", "Create a public RoleBinding only if absent", "iam"), http.StatusCreated)
	create.Description = "Complete SQLite runtime only. Requires current cluster rolebindings.write authority and cookie CSRF when applicable. Creates once; never adopts an existing binding. Only public operator metadata is accepted."
	s.forwardRoleBinding(create, perm{resource: "rolebindings", verb: "write"}, RoleBindingDesired{}, RoleBindingDocument{})
	replace := operation(http.MethodPut, path, "replaceRoleBinding", "Replace an explicitly reviewed public RoleBinding", "iam")
	replace.Description = "Complete SQLite runtime only. Requires exact reviewed UID and resourceVersion and current cluster rolebindings.write authority. Only API-owned bindings may be replaced. Object and grant revocation floors commit atomically. No implicit refresh or retry."
	s.forwardRoleBinding(replace, perm{resource: "rolebindings", verb: "write"}, ReplaceRoleBindingCommand{}, RoleBindingDocument{})
	remove := withStatus(operation(http.MethodDelete, path, "deleteRoleBinding", "Delete an explicitly reviewed public RoleBinding", "iam"), http.StatusNoContent)
	remove.Description = "Complete SQLite runtime only. Requires exact reviewed UID and resourceVersion and current cluster rolebindings.delete authority. Only API-owned bindings may be deleted. Object and grant revocation floors commit atomically. No implicit refresh or retry."
	if op := documented(s.api.OpenAPI(), http.MethodPut, path); op != nil {
		delete(op.Responses, "201")
	}
	s.forwardRoleBinding(remove, perm{resource: "rolebindings", verb: "delete"}, RoleBindingReview{}, nil)
}
