package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// Object is a Kubernetes-shaped api/v1alpha1 object (apiVersion, kind,
// metadata, spec, status) as the generic object routes exchange it.
type Object = map[string]any

// listObjectsInput are the filters of GET /objects/{kind}.
type listObjectsInput struct {
	Kind          string `path:"kind" doc:"Lower-case plural kind: dnszones, dnsrecordsets, blocklists, clientgroups, policies."`
	Namespace     string `query:"namespace" doc:"Only objects in this namespace (namespaced kinds)."`
	LabelSelector string `query:"labelSelector" doc:"Equality selector, e.g. app=web,tier=edge."`
	Limit         int    `query:"limit" minimum:"0" maximum:"1000" doc:"Page size (default 500)."`
	Continue      string `query:"continue" doc:"Token from the previous page."`
}

// ObjectList is the list envelope of the object routes.
type ObjectList struct {
	Items []Object `json:"items" nullable:"false"`
	// Continue fetches the next page; empty on the last page.
	Continue string `json:"continue,omitempty" doc:"Opaque keyset token for the next page; empty on the last page."`
	Took
}

type objectListOutput struct {
	Body *ObjectList
}

type objectPathInput struct {
	Kind      string `path:"kind"`
	Namespace string `path:"namespace"`
	Name      string `path:"name"`
}

type clusterObjectPathInput struct {
	Kind string `path:"kind"`
	Name string `path:"name"`
}

type objectOutput struct {
	ETag     string `header:"ETag" doc:"The resourceVersion of the object."`
	Location string `header:"Location"`
	Status   int
	Body     Object
}

type putObjectInput struct {
	Kind      string `path:"kind"`
	Namespace string `path:"namespace"`
	Name      string `path:"name"`
	IfMatch   string `header:"If-Match" doc:"Replace only when the stored resourceVersion equals this value (412 otherwise)."`
	Body      Object
}

type putClusterObjectInput struct {
	Kind    string `path:"kind"`
	Name    string `path:"name"`
	IfMatch string `header:"If-Match" doc:"Replace only when the stored resourceVersion equals this value (412 otherwise)."`
	Body    Object
}

type createObjectInput struct {
	Kind string `path:"kind"`
	Body Object
}

type deleteObjectInput struct {
	Kind      string `path:"kind"`
	Namespace string `path:"namespace"`
	Name      string `path:"name"`
	IfMatch   string `header:"If-Match"`
}

type deleteClusterObjectInput struct {
	Kind    string `path:"kind"`
	Name    string `path:"name"`
	IfMatch string `header:"If-Match"`
}

// ApplyResult is the outcome of one document of POST /apply.
type ApplyResult struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	// Result is created, updated, unchanged or error.
	Result string `json:"result" enum:"created,updated,unchanged,error"`
	Error  string `json:"error,omitempty"`
	// ErrorKey is the envelope key of the error (invalid, managed_by, ...).
	ErrorKey string `json:"errorKey,omitempty"`
	// Hint is the error hint (the owner for managed_by).
	Hint string `json:"hint,omitempty"`
	// ResourceVersion is the stored version after the write.
	ResourceVersion string `json:"resourceVersion,omitempty"`
}

type applyInput struct {
	ContentType string `header:"Content-Type"`
	RawBody     []byte `contentType:"application/yaml"`
}

type applyOutput struct {
	Body *List[ApplyResult]
}

type emptyOutput struct{}

// registerObjects registers the generic object routes (what `kudurru
// get|apply|delete` use).
func (s *Server) registerObjects() {
	byKind := func(verb string) perm { return perm{byKind: true, verb: verb, scopable: true} }

	register(s, operation(http.MethodGet, "/objects/{kind}", "listObjects", "List objects of a kind", "objects"),
		byKind("read"), s.listObjects)

	register(s, operation(http.MethodGet, "/objects/{kind}/{name}", "getClusterObject", "Get a cluster-scoped object (dnszones)", "objects"),
		byKind("read"), func(ctx context.Context, in *clusterObjectPathInput) (*objectOutput, error) {
			return s.getObjectRoute(ctx, in.Kind, "", in.Name, false)
		})
	register(s, operation(http.MethodGet, "/objects/{kind}/{namespace}/{name}", "getObject", "Get a namespaced object", "objects"),
		byKind("read"), func(ctx context.Context, in *objectPathInput) (*objectOutput, error) {
			return s.getObjectRoute(ctx, in.Kind, in.Namespace, in.Name, true)
		})

	register(s, withStatus(operation(http.MethodPost, "/objects/{kind}", "createObject", "Create an object (409 when it exists)", "objects"), http.StatusCreated),
		byKind("write"), func(ctx context.Context, in *createObjectInput) (*objectOutput, error) {
			k, err := s.kindParam(in.Kind)
			if err != nil {
				return nil, err
			}
			return s.putObjectRoute(ctx, k, writeRequest{kind: k, body: in.Body, createOnly: true})
		})

	register(s, operation(http.MethodPut, "/objects/{kind}/{name}", "putClusterObject", "Create or replace a cluster-scoped object", "objects"),
		byKind("write"), func(ctx context.Context, in *putClusterObjectInput) (*objectOutput, error) {
			k, err := s.kindParamScoped(in.Kind, false)
			if err != nil {
				return nil, err
			}
			return s.putObjectRoute(ctx, k, writeRequest{kind: k, body: in.Body, name: in.Name, ifMatch: in.IfMatch})
		})
	register(s, operation(http.MethodPut, "/objects/{kind}/{namespace}/{name}", "putObject", "Create or replace a namespaced object", "objects"),
		byKind("write"), func(ctx context.Context, in *putObjectInput) (*objectOutput, error) {
			k, err := s.kindParamScoped(in.Kind, true)
			if err != nil {
				return nil, err
			}
			return s.putObjectRoute(ctx, k, writeRequest{kind: k, body: in.Body, namespace: in.Namespace, name: in.Name, ifMatch: in.IfMatch})
		})

	deleteClusterOp := withStatus(operation(http.MethodDelete, "/objects/{kind}/{name}", "deleteClusterObject", "Delete a cluster-scoped object", "objects"), http.StatusNoContent)
	deleteClusterOp.Description = "A DNSZone is deleted alone only when no DNSRecordSet references it: otherwise 409 zone_not_empty " +
		"(error.objects lists them; DELETE /zones/{zone} deletes the zone with its record sets). The root zone \".\" is " +
		"409 default_zone (D148)."
	register(s, deleteClusterOp,
		byKind("delete"), func(ctx context.Context, in *deleteClusterObjectInput) (*emptyOutput, error) {
			return s.deleteObjectRoute(ctx, in.Kind, "", in.Name, in.IfMatch, false)
		})
	register(s, withStatus(operation(http.MethodDelete, "/objects/{kind}/{namespace}/{name}", "deleteObject", "Delete a namespaced object", "objects"), http.StatusNoContent),
		byKind("delete"), func(ctx context.Context, in *deleteObjectInput) (*emptyOutput, error) {
			return s.deleteObjectRoute(ctx, in.Kind, in.Namespace, in.Name, in.IfMatch, true)
		})

	applyOp := operation(http.MethodPost, "/apply", "apply", "Apply objects (multi-document YAML or a JSON array)", "objects")
	applyOp.Description = "Whole-object apply with manager \"api\". The body is multi-document YAML (application/yaml) or JSON " +
		"(an object, an array of objects, or a kind: List). Every document is authorized, validated and applied on its own; " +
		"the response lists one result per document in order."
	applyOp.RequestBody = &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{
		"application/yaml": {Schema: &huma.Schema{Type: "string"}},
		"application/json": {Schema: &huma.Schema{Type: "array", Items: &huma.Schema{Type: "object", AdditionalProperties: true}}},
	}}
	applyOp.SkipValidateBody = true
	register(s, applyOp, perm{kindResources: kindResources(), verb: "write", scopable: true}, s.apply)

	for _, k := range kinds {
		k := k
		op := operation(http.MethodPost, "/"+k.plural+":batchDelete", "batchDelete"+exportName(k.plural), "Delete several "+k.plural, k.plural)
		if k.plural == "dnszones" {
			op.Description = "Each zone is deleted alone, as DELETE /objects/dnszones/{name} does: a zone that record sets " +
				"still reference fails with zone_not_empty and the root zone with default_zone (D148)."
		}
		register(s, op, perm{resource: k.resource, verb: "delete", scopable: k.zoneScoped()}, func(ctx context.Context, in *batchDeleteInput) (*processedOutput, error) {
			return s.batchDelete(ctx, k, in)
		})
	}
}

// withStatus sets the default status of an operation.
func withStatus(op huma.Operation, status int) huma.Operation {
	op.DefaultStatus = status
	return op
}

// exportName upper-cases the first letter ("dnszones" → "Dnszones").
func exportName(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// kindParam resolves the {kind} path parameter.
func (s *Server) kindParam(plural string) (kindInfo, error) {
	k, ok := kindByPlural(plural)
	if !ok {
		return kindInfo{}, newError(http.StatusNotFound, KeyNotFound, "unknown kind "+plural, "kinds: "+strings.Join(kindPlurals(), ", "))
	}
	return k, nil
}

// kindParamScoped resolves {kind} and checks that the URL shape matches
// the kind's scope.
func (s *Server) kindParamScoped(plural string, namespaced bool) (kindInfo, error) {
	k, err := s.kindParam(plural)
	if err != nil {
		return k, err
	}
	switch {
	case k.namespaced && !namespaced:
		return k, newError(http.StatusNotFound, KeyNotFound, k.plural+" is namespaced", "use /objects/"+k.plural+"/{namespace}/{name}")
	case !k.namespaced && namespaced:
		return k, newError(http.StatusNotFound, KeyNotFound, k.plural+" is cluster-scoped", "use /objects/"+k.plural+"/{name}")
	}
	return k, nil
}

// parseLabelSelector parses an equality selector "a=b,c==d".
func parseLabelSelector(sel string) (map[string]string, error) {
	sel = strings.TrimSpace(sel)
	if sel == "" {
		return nil, nil
	}
	out := map[string]string{}
	for _, part := range strings.Split(sel, ",") {
		part = strings.TrimSpace(part)
		if strings.Contains(part, "!=") || strings.ContainsAny(part, "()") {
			return nil, errInvalid("labelSelector: only equality terms (key=value) are supported, got %q", part)
		}
		k, v, ok := strings.Cut(part, "=")
		v = strings.TrimPrefix(v, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, errInvalid("labelSelector: %q is not key=value", part)
		}
		out[k] = strings.TrimSpace(v)
	}
	return out, nil
}

// clampLimit applies the default and maximum page size.
func clampLimit(n, def, maximum int) int {
	switch {
	case n <= 0:
		return def
	case n > maximum:
		return maximum
	}
	return n
}

func (s *Server) listObjects(ctx context.Context, in *listObjectsInput) (*objectListOutput, error) {
	k, err := s.kindParam(in.Kind)
	if err != nil {
		return nil, err
	}
	st := stateFrom(ctx)
	if !st.clusterAllowed && !k.zoneScoped() {
		return nil, s.require(ctx, k.resource, "read", clusterScope, "")
	}
	if s.opts.ObjectReads != nil && (len(in.LabelSelector) > 4096 || len(in.Namespace) > 1024) {
		return nil, errInvalid("object list query is too large")
	}
	sel, err := parseLabelSelector(in.LabelSelector)
	if err != nil {
		return nil, err
	}
	if s.opts.ObjectReads != nil {
		return s.listCapturedObjects(ctx, k, in, sel)
	}
	cont, err := decodeCursor(in.Continue)
	if err != nil {
		return nil, err
	}
	ns := in.Namespace
	if !k.namespaced {
		ns = ""
	}
	page, err := s.opts.Store.List(ctx, k.kind, store.ListOptions{Namespace: ns, LabelSelector: sel, Limit: clampLimit(in.Limit, 500, 1000), Continue: cont})
	if err != nil {
		return nil, storeError(err)
	}
	items := make([]Object, 0, len(page.Items))
	for _, obj := range page.Items {
		if !st.clusterAllowed && !s.allowed(ctx, k.resource, "read", objectScope(k, obj)) {
			continue
		}
		m, err := objectMap(k, obj)
		if err != nil {
			return nil, err
		}
		items = append(items, m)
	}
	return &objectListOutput{Body: &ObjectList{Items: items, Continue: encodeCursor(page.Continue)}}, nil
}

func (s *Server) getObjectRoute(ctx context.Context, plural, namespace, name string, namespaced bool) (*objectOutput, error) {
	k, err := s.kindParamScoped(plural, namespaced)
	if err != nil {
		return nil, err
	}
	if s.opts.ObjectReads != nil {
		return s.getCapturedObject(ctx, k, namespace, name)
	}
	obj, err := s.getObject(ctx, k, namespace, name)
	if err != nil {
		// Do not reveal existence to callers who may not read it.
		if aerr := s.require(ctx, k.resource, "read", s.guessScope(k, name), name); aerr != nil {
			return nil, aerr
		}
		return nil, err
	}
	if err := s.require(ctx, k.resource, "read", objectScope(k, obj), name); err != nil {
		return nil, err
	}
	m, err := objectMap(k, obj)
	if err != nil {
		return nil, err
	}
	return &objectOutput{ETag: obj.GetResourceVersion(), Body: m, Status: http.StatusOK}, nil
}

// guessScope is the scope of an object that could not be read: zone:<name>
// for zones, cluster otherwise.
func (s *Server) guessScope(k kindInfo, name string) rbac.Scope {
	if k.plural == "dnszones" {
		return rbac.Scope{Kind: rbac.ScopeZone, Name: name}
	}
	return clusterScope
}

// putObjectRoute authorizes and performs one write.
func (s *Server) putObjectRoute(ctx context.Context, k kindInfo, req writeRequest) (*objectOutput, error) {
	if err := s.authorizeWrite(ctx, k, req); err != nil {
		return nil, err
	}
	res, err := s.writeObject(ctx, req)
	if err != nil {
		return nil, err
	}
	m, err := objectMap(k, res.obj)
	if err != nil {
		return nil, err
	}
	out := &objectOutput{ETag: res.obj.GetResourceVersion(), Body: m, Status: http.StatusOK}
	if res.result == ResultCreated {
		out.Status = http.StatusCreated
		out.Location = s.prefix + BasePath + objectURL(k, res.key.Namespace, res.key.Name)
	}
	return out, nil
}

// objectURL is the /objects path of one object.
func objectURL(k kindInfo, namespace, name string) string {
	if k.namespaced {
		return "/objects/" + k.plural + "/" + url.PathEscape(namespace) + "/" + url.PathEscape(name)
	}
	return "/objects/" + k.plural + "/" + url.PathEscape(name)
}

// authorizeWrite checks resource.write for a whole-object write at the
// scope of the new document and of the stored object it replaces: a zone
// update needs zones.write on the zone, a zone create needs it at cluster
// scope (§8.5), a record set needs records.write on its zone (and on the
// zone it moves away from).
func (s *Server) authorizeWrite(ctx context.Context, k kindInfo, req writeRequest) error {
	if s.opts.ObjectCommand != nil {
		// The command authorizes both exact current scopes from its own read.
		return nil
	}
	name := req.name
	if name == "" {
		name, _ = nestedString(req.body, "metadata", "name")
	}
	ns := req.namespace
	if ns == "" {
		ns, _ = nestedString(req.body, "metadata", "namespace")
	}
	if k.namespaced && ns == "" {
		ns = s.opts.DefaultNamespace
	}
	var existing store.Object
	if name != "" {
		obj := k.newObject()
		if err := s.opts.Store.Get(ctx, keyFor(k, ns, name), obj); err == nil {
			existing = obj
		}
	}
	switch k.plural {
	case "dnszones":
		sc := clusterScope
		if existing != nil {
			sc = rbac.Scope{Kind: rbac.ScopeZone, Name: name}
		}
		return s.require(ctx, k.resource, "write", sc, name)
	case "dnsrecordsets":
		zoneName, _ := nestedString(req.body, "spec", "zoneRef", "name")
		if err := s.require(ctx, k.resource, "write", rbac.Scope{Kind: rbac.ScopeZone, Name: zoneName}, name); err != nil {
			return err
		}
		if existing != nil {
			if sc := objectScope(k, existing); sc.Name != zoneName {
				return s.require(ctx, k.resource, "write", sc, name)
			}
		}
		return nil
	}
	return s.require(ctx, k.resource, "write", clusterScope, name)
}

func nestedString(m map[string]any, path ...string) (string, bool) {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		cur = mm[p]
	}
	s, ok := cur.(string)
	return s, ok
}

func (s *Server) deleteObjectRoute(ctx context.Context, plural, namespace, name, ifMatch string, namespaced bool) (*emptyOutput, error) {
	k, err := s.kindParamScoped(plural, namespaced)
	if err != nil {
		return nil, err
	}
	if s.opts.ObjectCommand != nil {
		if err := s.commandDeleteObject(ctx, k, keyFor(k, namespace, name), ifMatch); err != nil {
			return nil, err
		}
		return &emptyOutput{}, nil
	}
	obj, err := s.getObject(ctx, k, namespace, name)
	if err != nil {
		if aerr := s.require(ctx, k.resource, "delete", s.guessScope(k, name), name); aerr != nil {
			return nil, aerr
		}
		return nil, err
	}
	if err := s.require(ctx, k.resource, "delete", objectScope(k, obj), name); err != nil {
		return nil, err
	}
	if err := s.deleteObject(ctx, k, obj, ifMatch); err != nil {
		return nil, err
	}
	return &emptyOutput{}, nil
}

// decodeDocuments splits an apply body into documents: a JSON object, a
// JSON array, multi-document YAML, with kind: List expanded.
func decodeDocuments(contentType string, body []byte) ([]map[string]any, error) {
	mt, _, _ := mime.ParseMediaType(contentType)
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, errInvalid("body: no documents")
	}
	var docs []map[string]any
	if mt == "application/json" || trimmed[0] == '[' || trimmed[0] == '{' {
		var v any
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil {
			return nil, errInvalid("body: %v", err)
		}
		if err := collectDocuments(v, &docs); err != nil {
			return nil, err
		}
	} else {
		dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(trimmed), 4096)
		for {
			var v any
			if err := dec.Decode(&v); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return nil, errInvalid("body: document %d: %v", len(docs)+1, err)
			}
			if v == nil {
				continue
			}
			if err := collectDocuments(v, &docs); err != nil {
				return nil, err
			}
		}
	}
	for i, d := range docs {
		docs[i] = normalizeNumbers(d).(map[string]any)
	}
	if len(docs) == 0 {
		return nil, errInvalid("body: no documents")
	}
	return docs, nil
}

func collectDocuments(v any, out *[]map[string]any) error {
	switch t := v.(type) {
	case []any:
		for _, item := range t {
			if err := collectDocuments(item, out); err != nil {
				return err
			}
		}
	case map[string]any:
		if kind, _ := t["kind"].(string); kind == "List" || strings.HasSuffix(kind, "List") && t["items"] != nil {
			items, _ := t["items"].([]any)
			for _, item := range items {
				if err := collectDocuments(item, out); err != nil {
					return err
				}
			}
			return nil
		}
		*out = append(*out, t)
	default:
		return errInvalid("body: document %d is not an object", len(*out)+1)
	}
	return nil
}

// normalizeNumbers turns json.Number into int64 or float64 so the
// unstructured converter sees the types it expects.
func normalizeNumbers(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			t[k] = normalizeNumbers(x)
		}
		return t
	case []any:
		for i, x := range t {
			t[i] = normalizeNumbers(x)
		}
		return t
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		f, _ := t.Float64()
		return f
	case int:
		return int64(t)
	case float64:
		if t == float64(int64(t)) && t >= -1<<53 && t <= 1<<53 {
			return int64(t)
		}
	}
	return v
}

func (s *Server) apply(ctx context.Context, in *applyInput) (*applyOutput, error) {
	docs, err := decodeDocuments(in.ContentType, in.RawBody)
	if err != nil {
		return nil, err
	}
	results := make([]ApplyResult, 0, len(docs))
	for _, d := range docs {
		results = append(results, s.applyOne(ctx, d))
	}
	return &applyOutput{Body: newList(results, "")}, nil
}

// applyOne authorizes and writes one document of POST /apply.
func (s *Server) applyOne(ctx context.Context, doc map[string]any) ApplyResult {
	kind, _ := doc["kind"].(string)
	name, _ := nestedString(doc, "metadata", "name")
	ns, _ := nestedString(doc, "metadata", "namespace")
	res := ApplyResult{Kind: kind, Namespace: ns, Name: name}
	fail := func(err error) ApplyResult {
		res.Result = ResultError
		res.Error = err.Error()
		var e *Error
		if errors.As(err, &e) {
			res.Error = e.Body.Message
			res.ErrorKey = e.Body.Key
			if h, ok := e.Body.Hint.(string); ok {
				res.Hint = h
			}
		}
		return res
	}
	k, ok := kindByKind(store.Kind(kind))
	if !ok {
		return fail(errInvalid("kind: %q is not a kudurru kind (%s)", kind, strings.Join(kindNames(), ", ")))
	}
	if k.namespaced && res.Namespace == "" {
		res.Namespace = s.opts.DefaultNamespace
	}
	req := writeRequest{kind: k, body: doc}
	if err := s.authorizeWrite(ctx, k, req); err != nil {
		return fail(err)
	}
	wr, err := s.writeObject(ctx, req)
	if err != nil {
		return fail(err)
	}
	res.Result = wr.result
	res.Namespace, res.Name = wr.key.Namespace, wr.key.Name
	res.ResourceVersion = wr.obj.GetResourceVersion()
	return res
}

// batchDeleteInput is the body of POST /{kind}:batchDelete.
type batchDeleteInput struct {
	Body struct {
		Names     []string `json:"names" minItems:"1" maxItems:"1000"`
		Namespace string   `json:"namespace,omitempty" doc:"Namespace of the objects (namespaced kinds); defaults to the server's default namespace (the release namespace in Kubernetes, or \"default\" for legacy installs and other modes)."`
	}
}

type processedOutput struct {
	Body *Processed
}

func (s *Server) batchDelete(ctx context.Context, k kindInfo, in *batchDeleteInput) (*processedOutput, error) {
	out := newProcessed()
	ns := in.Body.Namespace
	if k.namespaced && ns == "" {
		ns = s.opts.DefaultNamespace
	}
	for _, name := range in.Body.Names {
		label := name
		if k.namespaced {
			label = ns + "/" + name
		}
		if s.opts.ObjectCommand != nil {
			if err := s.commandDeleteObject(ctx, k, keyFor(k, ns, name), ""); err != nil {
				out.fail(label, err)
			} else {
				out.ok(label)
			}
			continue
		}
		obj, err := s.getObject(ctx, k, ns, name)
		if err == nil {
			err = s.require(ctx, k.resource, "delete", objectScope(k, obj), name)
		}
		if err == nil {
			err = s.deleteObject(ctx, k, obj, "")
		}
		if err != nil {
			out.fail(label, err)
			continue
		}
		out.ok(label)
	}
	return &processedOutput{Body: out}, nil
}

// kindNames lists the store kinds, for messages.
func kindNames() []string {
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = string(k.kind)
	}
	return out
}
