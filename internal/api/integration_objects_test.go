package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api/apitest"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// pastedValue stands for a credential value an operator pasted into a
// reference field by mistake. No answer may repeat it.
const pastedValue = "pasted-credential-value-0123456789"

// integrationWriterAuthorizer holds integrations.write but not
// integrations.secrets for user "writer", as a Kubernetes ClusterRole that
// grants only some verbs on integrations would.
type integrationWriterAuthorizer struct{ rbac.Authorizer }

func (a integrationWriterAuthorizer) Authorize(ctx context.Context, p rbac.Principal, at rbac.Attributes) (rbac.Decision, error) {
	if p.Name == "writer" {
		if at.Resource == "integrations" && at.Verb == "secrets" {
			return rbac.Decision{Path: "none", Missing: &at}, nil
		}
		p.Name = "admin"
	}
	return a.Authorizer.Authorize(ctx, p, at)
}

func integrationDoc(name string, credentials map[string]any, suspend bool) map[string]any {
	spec := map[string]any{"type": "cloudflare", "suspend": suspend,
		"cloudflare": map[string]any{"zones": []any{map[string]any{"zone": "example.test"}}}}
	if credentials != nil {
		spec["credentials"] = credentials
	}
	return map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": "Integration",
		"metadata": map[string]any{"name": name, "namespace": "default"}, "spec": spec}
}

func integrationHarness(t *testing.T, mods ...func(*Options)) *harness {
	t.Helper()
	return newHarness(t, append([]func(*Options){func(o *Options) {
		o.Authorizer = integrationWriterAuthorizer{apitest.Authorizer(append(apitest.StandardBindings(), apitest.Grant("admin", "writer"))...)}
	}}, mods...)...)
}

// TestIntegrationObjectWrites proves the Integration writes of the ordinary
// write path (the Kubernetes driver's, over the memory store here and over
// kube-apiserver in TestIntegrationObjectWritesKube).
func TestIntegrationObjectWrites(t *testing.T) {
	runIntegrationObjectWrites(t, integrationHarness(t))
}

func runIntegrationObjectWrites(t *testing.T, h *harness) {
	t.Helper()
	path := "/api/v1/objects/integrations/default/edge"
	token := map[string]any{"apiToken": map[string]any{"env": "CF_API_TOKEN"}}
	key := store.Key{Kind: "Integration", Namespace: "default", Name: "edge"}
	noEcho := func(r response) response {
		t.Helper()
		if strings.Contains(r.Body.String(), pastedValue) {
			t.Fatalf("answer repeats a pasted credential value: %s", r.Body.String())
		}
		return r
	}

	// No integrations.write: refused before anything else.
	h.do(http.MethodPut, path, "operator", integrationDoc("edge", token, false)).want(http.StatusForbidden)
	// A new Integration needs the integrations license.
	h.srv.opts.License = licenseManager(t, 0)
	if r := h.do(http.MethodPut, path, "admin", integrationDoc("edge", token, false)).want(http.StatusPaymentRequired); r.errKey() != KeyLicense {
		t.Fatalf("unlicensed create: %s", r.Body.String())
	}
	h.srv.opts.License = license.Unrestricted()
	// Binding a credential needs integrations.secrets.
	r := h.do(http.MethodPut, path, "writer", integrationDoc("edge", token, false)).want(http.StatusForbidden)
	if !strings.Contains(r.Body.String(), "secrets") {
		t.Fatalf("refusal does not name integrations.secrets: %s", r.Body.String())
	}
	if integrationExists(t, h, key) {
		t.Fatal("a refused create stored the Integration")
	}
	// A value is never accepted, and the refusal does not repeat it.
	noEcho(h.do(http.MethodPut, path, "admin", integrationDoc("edge", map[string]any{"apiToken": map[string]any{"value": pastedValue}}, false)).want(http.StatusBadRequest))
	noEcho(h.do(http.MethodPut, path, "admin", integrationDoc("edge", map[string]any{"apiToken": map[string]any{"env": pastedValue + "!"}}, false)).want(http.StatusBadRequest))
	if integrationExists(t, h, key) {
		t.Fatal("an invalid document stored the Integration")
	}

	created := h.do(http.MethodPut, path, "admin", integrationDoc("edge", token, false)).want(http.StatusCreated)
	rv := created.Header().Get("ETag")
	var stored public.Integration
	created.json(&stored)
	if rv == "" || stored.UID == "" || stored.Spec.Credentials["apiToken"].Env != "CF_API_TOKEN" {
		t.Fatalf("created Integration: %s", created.Body.String())
	}
	if !integrationExists(t, h, key) {
		t.Fatal("created Integration is not stored")
	}
	// The same credentials need only integrations.write; a changed one also
	// needs integrations.secrets, decided on the stored object.
	h.do(http.MethodPut, path, "admin", integrationDoc("edge", token, true), "If-Match", "stale").want(http.StatusPreconditionFailed)
	updated := h.do(http.MethodPut, path, "writer", integrationDoc("edge", token, true), "If-Match", rv).want(http.StatusOK)
	rv = updated.Header().Get("ETag")
	h.do(http.MethodPut, path, "writer", integrationDoc("edge", map[string]any{"apiToken": map[string]any{"env": "OTHER_TOKEN"}}, true), "If-Match", rv).want(http.StatusForbidden)
	h.do(http.MethodPut, path, "writer", integrationDoc("edge", nil, true), "If-Match", rv).want(http.StatusForbidden)
	// Without a license, an existing Integration is still changed.
	h.srv.opts.License = licenseManager(t, 0)
	rv = h.do(http.MethodPut, path, "writer", integrationDoc("edge", token, false), "If-Match", rv).want(http.StatusOK).Header().Get("ETag")
	h.do(http.MethodPost, "/api/v1/objects/integrations", "admin", integrationDoc("other", token, false)).want(http.StatusPaymentRequired)

	// Reads need integrations.read and return references, never values.
	h.do(http.MethodGet, path, "viewer", nil).want(http.StatusForbidden)
	var read public.Integration
	h.do(http.MethodGet, path, "admin", nil).want(http.StatusOK).json(&read)
	if read.UID != stored.UID || read.ResourceVersion != rv || read.Spec.Suspend {
		t.Fatalf("read Integration: %+v", read)
	}
	var list ObjectList
	h.do(http.MethodGet, "/api/v1/objects/integrations?namespace=default", "admin", nil).want(http.StatusOK).json(&list)
	if len(list.Items) != 1 {
		t.Fatalf("listed %d Integrations", len(list.Items))
	}

	// Delete binds the reviewed revision and needs integrations.delete.
	h.do(http.MethodDelete, path, "operator", nil).want(http.StatusForbidden)
	h.do(http.MethodDelete, path, "writer", nil, "If-Match", "stale").want(http.StatusPreconditionFailed)
	h.do(http.MethodDelete, path, "writer", nil, "If-Match", rv).want(http.StatusNoContent)
	if integrationExists(t, h, key) {
		t.Fatal("deleted Integration survived")
	}
}

// TestIntegrationObjectCommandAdmission proves the SQLite command path: the
// command authorizes integrations.secrets beside integrations.write exactly
// when the write binds or changes credentials, from its own read, and admits
// a new Integration only under a current license cut.
func TestIntegrationObjectCommandAdmission(t *testing.T) {
	var mu sync.Mutex
	var seen [][]rbac.Attributes
	h, st := objectCommandHarness(t, func(_ context.Context, attrs []rbac.Attributes) error {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, attrs)
		return nil
	})
	last := func() []rbac.Attributes {
		mu.Lock()
		defer mu.Unlock()
		return seen[len(seen)-1]
	}
	k, _ := kindByKind("Integration")
	token := map[string]any{"apiToken": map[string]any{"file": "/run/secrets/cloudflare"}}
	write := func(doc map[string]any, ifMatch string) (writeResult, error) {
		return h.srv.writeObject(t.Context(), writeRequest{kind: k, body: doc, ifMatch: ifMatch})
	}
	manager, err := license.New(license.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h.srv.opts.License = manager
	var e *Error
	if _, err := write(integrationDoc("edge", token, false), ""); !errors.As(err, &e) || e.Status != http.StatusPaymentRequired {
		t.Fatalf("unlicensed command create: %v", err)
	}
	h.srv.opts.License = license.Unrestricted()
	created, err := write(integrationDoc("edge", token, false), "")
	if err != nil || created.result != ResultCreated {
		t.Fatalf("command create: %+v %v", created, err)
	}
	want := []rbac.Attributes{{Resource: "integrations", Verb: "write", Scope: clusterScope, Namespace: "default", Name: "edge"}, integrationSecretsAttributes(store.Key{Namespace: "default", Name: "edge"})}
	if got := last(); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("create authorized %+v", got)
	}
	updated, err := write(integrationDoc("edge", token, true), created.obj.GetResourceVersion())
	if err != nil || updated.result != ResultUpdated {
		t.Fatalf("command replace: %v", err)
	}
	if got := last(); len(got) != 1 || got[0] != want[0] {
		t.Fatalf("replace with the same credentials authorized %+v", got)
	}
	if _, err := write(integrationDoc("edge", map[string]any{"apiToken": map[string]any{"env": "OTHER"}}, true), updated.obj.GetResourceVersion()); err != nil {
		t.Fatal(err)
	}
	if got := last(); len(got) != 2 || got[1].Verb != "secrets" {
		t.Fatalf("credential change authorized %+v", got)
	}
	if _, err := write(integrationDoc("edge", token, false), "stale"); !errors.As(err, &e) || e.Status != http.StatusPreconditionFailed {
		t.Fatalf("stale command replace: %v", err)
	}
	if _, err := write(integrationDoc("edge", map[string]any{"apiToken": map[string]any{"env": pastedValue + "!"}}, false), ""); err == nil || strings.Contains(err.Error(), pastedValue) {
		t.Fatalf("command refusal repeats a pasted value: %v", err)
	}
	var stored public.Integration
	if err := st.Get(t.Context(), store.Key{Kind: "Integration", Namespace: "default", Name: "edge"}, &stored); err != nil || stored.Spec.Credentials["apiToken"].Env != "OTHER" {
		t.Fatalf("command did not commit to SQLite: %v", err)
	}
}

func TestIntegrationCredentialsChanged(t *testing.T) {
	stored := &public.Integration{Spec: public.IntegrationSpec{Credentials: map[string]public.CredentialReference{"apiToken": {Env: "A"}}}}
	for name, tc := range map[string]struct {
		credentials any
		existing    store.Object
		want        bool
	}{
		"create without":  {nil, nil, false},
		"create with":     {map[string]any{"apiToken": map[string]any{"env": "A"}}, nil, true},
		"same":            {map[string]any{"apiToken": map[string]any{"env": "A"}}, stored, false},
		"changed":         {map[string]any{"apiToken": map[string]any{"env": "B"}}, stored, true},
		"form changed":    {map[string]any{"apiToken": map[string]any{"file": "A"}}, stored, true},
		"added":           {map[string]any{"apiToken": map[string]any{"env": "A"}, "other": map[string]any{"env": "C"}}, stored, true},
		"removed":         {nil, stored, true},
		"undecodable":     {map[string]any{"apiToken": map[string]any{"value": "x"}}, stored, true},
		"not a map":       {"x", stored, true},
		"other kind":      {nil, &public.Policy{}, true},
		"empty both ways": {map[string]any{}, &public.Integration{}, false},
	} {
		spec := map[string]any{}
		if tc.credentials != nil {
			spec["credentials"] = tc.credentials
		}
		if got := integrationCredentialsChanged(map[string]any{"spec": spec}, tc.existing); got != tc.want {
			t.Errorf("%s: changed=%t, want %t", name, got, tc.want)
		}
	}
}

// fakeIntegrationAdmin answers only manifests.
type fakeIntegrationAdmin struct {
	IntegrationAdministration
	manifests *IntegrationManifests
}

func (f fakeIntegrationAdmin) IntegrationManifests(context.Context) (*IntegrationManifests, error) {
	return f.manifests, nil
}

func TestIntegrationManifestsRoute(t *testing.T) {
	h := integrationHarness(t)
	h.do(http.MethodGet, "/api/v1/integrations/manifests", "admin", nil).want(http.StatusServiceUnavailable)
	h.srv.opts.Integrations = fakeIntegrationAdmin{manifests: &IntegrationManifests{Driver: "sqlite", CredentialSources: []string{"file"}, AllowedFiles: []string{"/run/secrets/cloudflare"},
		Items: []IntegrationManifest{{Type: "cloudflare", Capabilities: []string{"RecordSink"}, Credentials: []IntegrationCredentialKey{{Name: "apiToken", Required: true}}, Supported: true, Refused: []string{}}}}}
	h.do(http.MethodGet, "/api/v1/integrations/manifests", "viewer", nil).want(http.StatusForbidden)
	var full, partial IntegrationManifests
	r := h.do(http.MethodGet, "/api/v1/integrations/manifests", "admin", nil).want(http.StatusOK)
	r.json(&full)
	if r.Header().Get("Cache-Control") != "no-store" || len(full.Items) != 1 || len(full.AllowedFiles) != 1 {
		t.Fatalf("manifests for a holder of integrations.secrets: %s", r.Body.String())
	}
	// Without integrations.secrets the node's credential grants are withheld.
	h.do(http.MethodGet, "/api/v1/integrations/manifests", "writer", nil).want(http.StatusOK).json(&partial)
	if len(partial.Items) != 1 || partial.AllowedFiles != nil || len(partial.CredentialSources) != 1 {
		t.Fatalf("manifests without integrations.secrets: %+v", partial)
	}
}

func integrationExists(t *testing.T, h *harness, key store.Key) bool {
	t.Helper()
	err := h.srv.opts.Store.Get(context.Background(), key, &public.Integration{})
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	return err == nil
}
