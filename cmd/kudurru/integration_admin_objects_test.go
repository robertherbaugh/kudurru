package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// integrationObjectRouter mounts the API as serve does on the complete
// SQLite runtime: object commands, captured object reads, the integration
// host's administration and the node's license.
func integrationObjectRouter(t *testing.T, f *integrationOperationFixture, mods ...func(*api.Options)) http.Handler {
	t.Helper()
	opts := api.Options{Store: f.s.opts.Store, Validator: f.s.opts.Validator, Config: f.s.opts.Config, CanonicalSettings: f.s,
		Integrations: f.s.integrations, ObjectCommand: f.s.ObjectCommand, ObjectItems: f.s.ObjectItems, ObjectReads: servingbuild.NewObjectReads(),
		License: f.s.opts.License, Authenticate: f.adapter.Middleware, Authorizer: f.adapter, Logger: slog.New(slog.DiscardHandler)}
	for _, mod := range mods {
		mod(&opts)
	}
	srv, err := api.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	return srv
}

func integrationObjectCall(t *testing.T, router http.Handler, method, path string, body any, token string, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest(method, "https://kudurru.test/api/v1"+path, bytes.NewReader(payload))
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", "Bearer "+token)
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	// The credential file's value never reaches an answer; its path, the
	// reference, is the object's own field.
	if strings.Contains(w.Body.String(), "test-only-private-credential") {
		t.Fatalf("%s %s: credential value reached the answer", method, path)
	}
	return w
}

// integrationScopedToken adds an API token of the fixture's admin user
// narrowed to role.
func integrationScopedToken(t *testing.T, f *integrationOperationFixture, role string) string {
	t.Helper()
	token, id := tokens.Generate()
	err := f.s.opts.Store.Command(t.Context(), f.s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var user internalv1.LocalUser
		if err := tx.Get(ctx, store.Key{Kind: "LocalUser", Name: "operation-admin"}, &user); err != nil {
			return err
		}
		expires := metav1.NewTime(time.Now().Add(time.Hour))
		_, err := tx.Apply(ctx, &internalv1.APIToken{ObjectMeta: metav1.ObjectMeta{Name: id}, Spec: internalv1.APITokenSpec{SecretHash: tokens.Hash(token), Owner: internalv1.TokenOwner{Kind: internalv1.OwnerUser, Name: user.GetName(), UID: string(user.GetUID()), ID: "local:operation-admin", Provider: "local"}, Scopes: []string{role}, ExpiresAt: &expires}}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	integrationAwaitPublished(t, f)
	return token
}

// integrationAwaitPublished waits until HTTP requests capture the installed
// generation, so that the next request reads what was just committed.
func integrationAwaitPublished(t *testing.T, f *integrationOperationFixture) {
	t.Helper()
	awaitServingRuntime(t, f.s, func() bool {
		lease, err := f.s.publisher.CaptureHTTP(t.Context())
		if err != nil {
			return false
		}
		defer lease.Release()
		base, current := lease.Bundle().Identity().Base, f.s.opts.Store.Authority().Installed
		return base.Term.Same(current.Term) && base.Revision == current.Revision
	})
}

// An Integration written through the mounted object routes on the complete
// SQLite runtime is admitted by the current command (permission, license,
// exact resourceVersion), committed with its audit row, run by the host and
// checked through the administration API with the identity the write
// returned; the credential value appears in no answer and no audit row (D400).
func TestIntegrationAdminObjectWritesOnCompleteSQLite(t *testing.T) {
	f := newIntegrationOperationFixture(t)
	router := integrationObjectRouter(t, f)
	operator := integrationScopedToken(t, f, "operator")

	var manifests api.IntegrationManifests
	w := integrationObjectCall(t, router, http.MethodGet, "/integrations/manifests", nil, f.token)
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &manifests) != nil {
		t.Fatalf("manifests: %d %s", w.Code, w.Body)
	}
	types := make([]string, 0, len(manifests.Items))
	for _, m := range manifests.Items {
		types = append(types, m.Type)
	}
	if manifests.Driver != "sqlite" || !manifests.Licensed || !slices.Equal(types, []string{"cloudflare", "unifi", "webhook"}) ||
		!slices.Equal(manifests.CredentialSources, []string{"file"}) || !slices.Equal(manifests.AllowedFiles, []string{f.path}) {
		t.Fatalf("manifests: %+v", manifests)
	}

	definition := integrationDefinition("second", f.path)
	definition.TypeMeta = metav1.TypeMeta{APIVersion: "kudurru.io/v1alpha1", Kind: "Integration"}
	path := "/objects/integrations/default/second"
	if w := integrationObjectCall(t, router, http.MethodPut, path, definition, operator); w.Code != http.StatusForbidden {
		t.Fatalf("operator create: %d %s", w.Code, w.Body)
	}
	w = integrationObjectCall(t, router, http.MethodPut, path, definition, f.token)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var created public.Integration
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.UID == "" || w.Header().Get("ETag") != created.ResourceVersion {
		t.Fatalf("created: %v %s", err, w.Body)
	}
	integrationAwaitPublished(t, f)
	entries, err := f.s.opts.Store.ReadAudit(t.Context(), sqlite.AuditQuery{Kind: "Integration", Namespace: "default", Name: "second"})
	if err != nil {
		t.Fatal(err)
	}
	audited := slices.ContainsFunc(entries, func(e sqlite.AuditEntry) bool {
		return e.Action == "integration.create" && e.Result == sqlite.AuditSuccess && e.Manager == store.ManagerAPI && e.ResourceVersion == created.ResourceVersion
	})
	for _, e := range entries {
		if bytes.Contains(e.After, []byte("test-only-private-credential")) || bytes.Contains(e.Before, []byte("test-only-private-credential")) {
			t.Fatal("audit row holds the credential value")
		}
	}
	if !audited {
		t.Fatalf("create has no audit row in its transaction: %+v", entries)
	}

	// The host runs the new instance. Review its settled identity, which
	// must be the UID the write returned, and check it once.
	owner := integrationOwner(f.s, &created)
	integrationReadyCommand(t, f.s, owner)
	summary := integrationSettled(t, f, router, owner, "second")
	if summary.UID != string(created.UID) {
		t.Fatalf("summary UID %s, created %s", summary.UID, created.UID)
	}
	before := f.calls.Load()
	w = integrationObjectCall(t, router, http.MethodPost, "/integrations/default/second/check", api.IntegrationReview{UID: summary.UID, Revision: summary.Revision}, f.token)
	if w.Code != http.StatusOK || f.calls.Load() <= before {
		t.Fatalf("check of the API-created Integration: %d %s (provider requests %d before, %d after)", w.Code, w.Body, before, f.calls.Load())
	}

	// Replace and delete bind the exact stored resourceVersion.
	summary = integrationSettled(t, f, router, owner, "second")
	definition.Spec.Suspend = true
	if w := integrationObjectCall(t, router, http.MethodPut, path, definition, f.token, "If-Match", "stale"); w.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale replace: %d %s", w.Code, w.Body)
	}
	w = integrationObjectCall(t, router, http.MethodPut, path, definition, f.token, "If-Match", summary.Revision)
	if w.Code != http.StatusOK {
		t.Fatalf("replace: %d %s", w.Code, w.Body)
	}
	if w.Header().Get("ETag") == "" || w.Header().Get("ETag") == summary.Revision {
		t.Fatalf("replace returned no new revision: %q", w.Header().Get("ETag"))
	}
	// The host settles the suspended revision (its status write may move it).
	revision := integrationSettled(t, f, router, owner, "second").Revision
	if w := integrationObjectCall(t, router, http.MethodDelete, path, nil, operator); w.Code != http.StatusForbidden {
		t.Fatalf("operator delete: %d %s", w.Code, w.Body)
	}
	if w := integrationObjectCall(t, router, http.MethodDelete, path, nil, f.token, "If-Match", "stale"); w.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale delete: %d %s", w.Code, w.Body)
	}
	if w := integrationObjectCall(t, router, http.MethodDelete, path, nil, f.token, "If-Match", revision); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if err := f.s.opts.Store.Get(t.Context(), store.KeyOf(&created), &public.Integration{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted Integration: %v", err)
	}
	// A file-managed Integration is not the API's to change.
	filed := integrationDefinition("filed", f.path)
	filed.Spec.Suspend = true
	if _, err := f.s.opts.Store.Apply(t.Context(), filed, store.ApplyOptions{Manager: "file:/etc/kudurru/objects/filed.yaml"}); err != nil {
		t.Fatal(err)
	}
	integrationAwaitPublished(t, f)
	filed.Spec.Suspend = false
	filed.TypeMeta = definition.TypeMeta
	w = integrationObjectCall(t, router, http.MethodPut, "/objects/integrations/default/filed", filed, f.token)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "managed_by") {
		t.Fatalf("file-managed Integration replace: %d %s", w.Code, w.Body)
	}

	// Without a license that covers integrations, the command refuses a new
	// Integration with 402 and stores nothing.
	unlicensed, err := license.New(license.Options{})
	if err != nil {
		t.Fatal(err)
	}
	noLicense := integrationObjectRouter(t, f, func(o *api.Options) { o.License = unlicensed })
	refused := integrationDefinition("unlicensed", f.path)
	refused.TypeMeta = definition.TypeMeta
	if w := integrationObjectCall(t, noLicense, http.MethodPost, "/objects/integrations", refused, f.token); w.Code != http.StatusPaymentRequired || !strings.Contains(w.Body.String(), api.KeyLicense) {
		t.Fatalf("unlicensed create: %d %s", w.Code, w.Body)
	}
	if err := f.s.opts.Store.Get(t.Context(), store.KeyOf(refused), &public.Integration{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("refused create was stored: %v", err)
	}

	// deletionPolicy Delete: the command arms the cleanup finalizer in the
	// write's transaction.
	cleanup := integrationDefinition("cleanup", f.path)
	cleanup.TypeMeta = definition.TypeMeta
	cleanup.Spec.Suspend = true
	cleanup.Spec.DeletionPolicy = public.IntegrationDeletionDelete
	if w := integrationObjectCall(t, router, http.MethodPut, "/objects/integrations/default/cleanup", cleanup, f.token); w.Code != http.StatusCreated {
		t.Fatalf("create with deletionPolicy Delete: %d %s", w.Code, w.Body)
	}
	var armed public.Integration
	if err := f.s.opts.Store.Get(t.Context(), store.KeyOf(cleanup), &armed); err != nil || !slices.Contains(armed.Finalizers, store.IntegrationCleanupFinalizer) {
		t.Fatalf("cleanup finalizer not armed: %v %v", err, armed.Finalizers)
	}
	integrationAwaitPublished(t, f)

	// POST /apply and POST /integrations:batchDelete take the same command.
	applied := integrationDefinition("applied", f.path)
	applied.TypeMeta = definition.TypeMeta
	applied.Spec.Suspend = true
	var results struct {
		Items []api.ApplyResult `json:"items"`
	}
	w = integrationObjectCall(t, router, http.MethodPost, "/apply", []any{applied}, operator)
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &results) != nil || len(results.Items) != 1 || results.Items[0].ErrorKey != api.KeyForbidden {
		t.Fatalf("operator apply: %d %s", w.Code, w.Body)
	}
	w = integrationObjectCall(t, router, http.MethodPost, "/apply", []any{applied}, f.token)
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &results) != nil || len(results.Items) != 1 || results.Items[0].Result != api.ResultCreated {
		t.Fatalf("apply: %d %s", w.Code, w.Body)
	}
	if err := f.s.opts.Store.Get(t.Context(), store.KeyOf(applied), &public.Integration{}); err != nil {
		t.Fatalf("applied Integration: %v", err)
	}
	integrationAwaitPublished(t, f)
	var processed api.Processed
	w = integrationObjectCall(t, router, http.MethodPost, "/integrations:batchDelete", map[string]any{"names": []string{"applied", "missing"}, "namespace": "default"}, f.token)
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &processed) != nil || len(processed.Processed.Success) != 1 || len(processed.Processed.Errors) != 1 {
		t.Fatalf("batch delete: %d %s", w.Code, w.Body)
	}
	if err := f.s.opts.Store.Get(t.Context(), store.KeyOf(applied), &public.Integration{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("batch-deleted Integration: %v", err)
	}
}

// integrationSettled waits until the host captures the stored revision of
// name (or it is suspended) and HTTP requests capture that generation, and
// returns the summary the API answers for it.
func integrationSettled(t *testing.T, f *integrationOperationFixture, router http.Handler, owner kinds.Owner, name string) api.IntegrationSummary {
	t.Helper()
	var summary api.IntegrationSummary
	awaitServingRuntime(t, f.s, func() bool {
		var current public.Integration
		if f.s.opts.Store.Get(t.Context(), store.Key{Kind: "Integration", Namespace: "default", Name: name}, &current) != nil {
			return false
		}
		if _, err := f.s.integrations.host.CaptureCommand(t.Context(), owner, current.ResourceVersion); err != nil && !current.Spec.Suspend {
			return false
		}
		lease, err := f.s.publisher.CaptureHTTP(t.Context())
		if err != nil {
			return false
		}
		base := lease.Bundle().Identity().Base
		lease.Release()
		if installed := f.s.opts.Store.Authority().Installed; !base.Term.Same(installed.Term) || base.Revision != installed.Revision {
			return false
		}
		w := integrationObjectCall(t, router, http.MethodGet, "/integrations/default/"+name, nil, f.token)
		return w.Code == http.StatusOK && json.Unmarshal(w.Body.Bytes(), &summary) == nil && summary.Revision == current.ResourceVersion
	})
	return summary
}

// The Kubernetes limits the manifests report are the refusals the
// Kubernetes root makes (D384).
func TestIntegrationManifestsMatchKubernetesRefusals(t *testing.T) {
	m := integrationManifests("kube", "kudurru-system", kubeIntegrationOptions{}.Credentials, nil, kubeIntegrationLimits)
	if m.Licensed || len(m.CredentialSources) != 0 || m.Namespace != "kudurru-system" {
		t.Fatalf("manifests: %+v", m)
	}
	base := func(typ string) *public.Integration {
		d := &public.Integration{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "kudurru-system"}, Spec: public.IntegrationSpec{Type: typ}}
		switch typ {
		case "unifi":
			d.Spec.UniFi = &public.UniFiIntegration{Endpoint: "https://controller.invalid"}
		case "cloudflare":
			d.Spec.Cloudflare = &public.CloudflareIntegration{}
		}
		return d
	}
	for _, item := range m.Items {
		reason := kubeIntegrationRefusal("kudurru-system", base(item.Type))
		if item.Supported != (reason == "") || item.Reason != reason {
			t.Errorf("%s: supported=%t reason=%q, root refuses with %q", item.Type, item.Supported, item.Reason, reason)
		}
		if item.Type == "unifi" {
			d := base("unifi")
			d.Spec.UniFi.Inventory = &public.UniFiInventory{}
			if kubeIntegrationRefusal("kudurru-system", d) == "" || !slices.Contains(item.Refused, "spec.unifi.inventory") {
				t.Errorf("unifi inventory: refused=%v", item.Refused)
			}
			// Each UniFi push is refused on Kubernetes and named (D401).
			for field, set := range map[string]func(*public.UniFiPush){
				"spec.unifi.push.dhcpDNSServers": func(p *public.UniFiPush) { p.DHCPDNSServers = true },
				"spec.unifi.push.mirrorRecords":  func(p *public.UniFiPush) { p.MirrorRecords.Enabled = true },
				"spec.unifi.push.clientActions":  func(p *public.UniFiPush) { p.ClientActions = true },
			} {
				d := base("unifi")
				set(&d.Spec.UniFi.Push)
				if kubeIntegrationRefusal("kudurru-system", d) != reasonPushUnsupported || !slices.Contains(item.Refused, field) {
					t.Errorf("%s: refused=%v reason=%q", field, item.Refused, kubeIntegrationRefusal("kudurru-system", d))
				}
			}
		}
		if len(item.Credentials) == 0 && item.Type != "webhook" || item.ConfigField != item.Type || !json.Valid(item.ConfigSchema) {
			t.Errorf("%s manifest: %+v", item.Type, item)
		}
		for _, c := range item.Credentials {
			if c.Required != (item.Type == "cloudflare" && c.Name == "apiToken" || item.Type == "unifi" && c.Name == "apiKey") {
				t.Errorf("%s credential %s required=%t", item.Type, c.Name, c.Required)
			}
		}
	}
	if d := base("cloudflare"); kubeIntegrationRefusal("other", d) != reasonNamespaceNotAllowed {
		t.Error("another namespace is not refused")
	}
}

// The SQLite root runs every UniFi push (D401): its manifest refuses no
// UniFi field and advertises the record sink and actions the adapter serves.
func TestIntegrationManifestsSQLiteRunsUniFiPush(t *testing.T) {
	m := integrationManifests("sqlite", "", config.CredentialGrants{}, nil, sqliteIntegrationLimits)
	found := false
	for _, item := range m.Items {
		if item.Type != "unifi" {
			continue
		}
		found = true
		if !item.Supported || len(item.Refused) != 0 || !slices.Contains(item.Capabilities, "RecordSink") || !slices.Contains(item.Capabilities, "Actions") {
			t.Fatalf("SQLite UniFi manifest: %+v", item)
		}
	}
	if !found {
		t.Fatal("unifi manifest missing")
	}
}

// The current command admits integrations.secrets only beside the write of
// the same Integration (D400).
func TestObjectCommandAttributesIntegrationSecrets(t *testing.T) {
	key := store.Key{Kind: "Integration", Namespace: "default", Name: "edge"}
	cluster := rbac.Scope{Kind: rbac.ScopeCluster}
	write := rbac.Attributes{Resource: "integrations", Verb: "write", Scope: cluster, Namespace: "default", Name: "edge"}
	secrets := rbac.Attributes{Resource: "integrations", Verb: "secrets", Scope: cluster, Namespace: "default", Name: "edge"}
	other := secrets
	other.Name = "other"
	remove := write
	remove.Verb = "delete"
	for name, tc := range map[string]struct {
		key   store.Key
		attrs []rbac.Attributes
		want  bool
	}{
		"write":             {key, []rbac.Attributes{write}, true},
		"write and secrets": {key, []rbac.Attributes{write, secrets}, true},
		"delete":            {key, []rbac.Attributes{remove}, true},
		"secrets alone":     {key, []rbac.Attributes{secrets}, false},
		"secrets first":     {key, []rbac.Attributes{secrets, write}, false},
		"twice write":       {key, []rbac.Attributes{write, write}, false},
		"delete and secret": {key, []rbac.Attributes{remove, secrets}, false},
		"other object":      {key, []rbac.Attributes{write, other}, false},
		"policy pair":       {store.Key{Kind: "Policy", Namespace: "default", Name: "edge"}, []rbac.Attributes{write, secrets}, false},
	} {
		resource := objectCommandResource(tc.key)
		if got := objectCommandAttributes(tc.key, resource, tc.attrs); got != tc.want {
			t.Errorf("%s: admitted=%t, want %t", name, got, tc.want)
		}
	}
}
