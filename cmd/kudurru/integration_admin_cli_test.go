package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/api"
)

// seedIntegration adds one Integration to the fake API.
func seedIntegration(h *harness, name string) {
	h.api.mu.Lock()
	defer h.api.mu.Unlock()
	if h.api.objects["integrations"] == nil {
		h.api.objects["integrations"] = map[string]json.RawMessage{}
	}
	h.api.objects["integrations"]["default/"+name] = json.RawMessage(`{"apiVersion":"kudurru.io/v1alpha1","kind":"Integration","metadata":{"name":"` + name + `","namespace":"default","creationTimestamp":"2026-10-11T11:00:00Z","annotations":{"kudurru.io/managed-by":"api"}},
		"spec":{"type":"cloudflare","suspend":true,"deletionPolicy":"Delete","credentials":{"apiToken":{"file":"/run/secrets/cloudflare"}},"cloudflare":{"zones":[{"zone":"example.com"}]}},
		"status":{"conditions":[{"type":"Ready","status":"True","lastTransitionTime":"2026-10-11T11:00:00Z"}]}}`)
}

// The Integration kind of the CLI (D400): get, the intg alias, apply and
// delete take the generic object routes, and a backup or an export that
// names no kinds leaves Integrations out while `export integrations`
// includes them.
func TestIntegrationsObjectCLI(t *testing.T) {
	h := loggedIn(t)
	seedIntegration(h, "edge")

	out := h.mustRun("get", "integrations")
	for _, want := range []string{"TYPE", "SUSPENDED", "POLICY", "READY", "edge", "cloudflare", "true", "Delete"} {
		if !strings.Contains(out, want) {
			t.Fatalf("get integrations lacks %q:\n%s", want, out)
		}
	}
	if alias := h.mustRun("get", "intg"); alias != out {
		t.Fatalf("get intg differs from get integrations:\n%s\n%s", alias, out)
	}
	if wide := h.mustRun("get", "integrations", "-o", "wide"); !strings.Contains(wide, "TERMINATING") || !strings.Contains(wide, "MANAGED-BY") {
		t.Fatalf("get integrations -o wide:\n%s", wide)
	}

	// A kind-less export and backup do not read Integrations.
	var objs []map[string]any
	if err := json.Unmarshal([]byte(h.mustRun("export", "-o", "json")), &objs); err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if o["kind"] == "Integration" {
			t.Fatalf("kind-less export includes an Integration: %v", o)
		}
	}
	if got := h.api.requested("GET /api/v1/objects/integrations"); len(got) != 3 {
		t.Fatalf("Integrations were listed by the kind-less export: %v", got)
	}
	file := filepath.Join(t.TempDir(), "backup.yaml")
	if _, errOut, err := h.run("backup", "--out", file, "--safe"); err != nil || !strings.Contains(errOut, "9 objects written") {
		t.Fatalf("backup: %v %q", err, errOut)
	}
	if b, err := os.ReadFile(file); err != nil || strings.Contains(string(b), "kind: Integration") || strings.Contains(string(b), "integrations") { //nolint:gosec // G304: the test's temp file
		t.Fatalf("backup includes Integrations: %v", err)
	}
	if got := h.api.requested("GET /api/v1/objects/integrations"); len(got) != 3 {
		t.Fatalf("Integrations were listed by the backup: %v", got)
	}
	// Named, they are exported, references and all.
	if err := json.Unmarshal([]byte(h.mustRun("export", "integrations", "-o", "json")), &objs); err != nil || len(objs) != 1 || objs[0]["kind"] != "Integration" {
		t.Fatalf("export integrations: %v %v", err, objs)
	}

	// apply -f and delete use /apply and the object route.
	doc := filepath.Join(t.TempDir(), "integration.yaml")
	if err := os.WriteFile(doc, []byte("apiVersion: kudurru.io/v1alpha1\nkind: Integration\nmetadata: {name: second, namespace: default}\nspec:\n  type: cloudflare\n  credentials: {apiToken: {file: /run/secrets/cloudflare}}\n  cloudflare: {zones: [{zone: example.org}]}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := h.mustRun("apply", "-f", doc); !strings.Contains(out, "integration/default/second created") {
		t.Fatalf("apply integration: %q", out)
	}
	if body := string(h.api.lastBody("POST", "/api/v1/apply")); !strings.Contains(body, `"kind":"Integration"`) {
		t.Fatalf("apply body %s", body)
	}
	h.mustRun("delete", "integrations", "second", "-n", "default")
	if got := h.api.requested("DELETE /api/v1/objects/integrations/default/second"); len(got) != 1 {
		t.Fatalf("delete requests %v", got)
	}
}

// `kudurru integrations manifests` renders the provider manifests of the
// node as a table and passes the JSON through with -o json.
func TestIntegrationsManifestsCLI(t *testing.T) {
	h := loggedIn(t)
	manifests := api.IntegrationManifests{Driver: "kube", Namespace: "kudurru-system", Items: []api.IntegrationManifest{
		{Type: "cloudflare", DisplayName: "Cloudflare", Capabilities: []string{"RecordSink"}, Credentials: []api.IntegrationCredentialKey{{Name: "apiToken", Required: true}}, Supported: true, Refused: []string{}},
		{Type: "webhook", DisplayName: "Webhook", Capabilities: []string{"RecordSink"}, Supported: false, Reason: "unsupported on this driver", Refused: []string{}},
		{Type: "unifi", DisplayName: "UniFi", Capabilities: []string{"RecordSource", "Inventory"}, Credentials: []api.IntegrationCredentialKey{{Name: "apiKey", Required: true}, {Name: "ca"}}, Supported: true, Refused: []string{"spec.unifi.inventory", "spec.unifi.push"}},
	}}
	h.api.mu.Lock()
	h.api.manifests = manifests
	h.api.mu.Unlock()
	out := h.mustRun("integrations", "manifests")
	for _, want := range []string{
		"TYPE", "CAPABILITIES", "CREDENTIALS", "SUPPORTED", "REFUSED",
		"apiToken (required)", "no: unsupported on this driver", "apiKey (required), ca", "spec.unifi.inventory, spec.unifi.push", "RecordSource,Inventory",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("integrations manifests lacks %q:\n%s", want, out)
		}
	}
	var decoded api.IntegrationManifests
	if err := json.Unmarshal([]byte(h.mustRun("integrations", "manifests", "-o", "json")), &decoded); err != nil || decoded.Driver != "kube" || len(decoded.Items) != 3 {
		t.Fatalf("integrations manifests -o json: %v %+v", err, decoded)
	}
}
