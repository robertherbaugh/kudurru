package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGetTables(t *testing.T) {
	h := loggedIn(t)
	h.api.pageSize = 1 // every list needs several "continue" pages
	cases := []struct {
		golden string
		args   []string
	}{
		{"get-zones", []string{"get", "zones"}},
		{"get-zones-wide", []string{"get", "dnszone", "-o", "wide"}},
		{"get-rrsets-all-wide", []string{"get", "rrset", "-A", "-o", "wide"}},
		{"get-rrsets", []string{"get", "rrsets"}},
		{"get-blocklists", []string{"get", "list"}},
		{"get-blocklists-wide", []string{"get", "blocklists", "-o", "wide"}},
		{"get-clientgroups", []string{"get", "cg"}},
		{"get-policies", []string{"get", "pol"}},
		{"get-policies-wide", []string{"get", "policy", "-o", "wide"}},
		{"get-policies-name", []string{"get", "policies", "-o", "name"}},
		{"get-zone-single", []string{"get", "zone", "home-example-com"}},
		{"get-rrsets-selector", []string{"get", "rrset", "-A", "-l", "team=platform"}},
	}
	for _, tc := range cases {
		t.Run(tc.golden, func(t *testing.T) {
			golden(t, h, tc.golden, h.mustRunT(t, tc.args...))
		})
	}
	if n := len(h.api.requested("GET /api/v1/objects/dnszones?continue=")); n < 1 {
		t.Fatalf("pagination not followed: %v", h.api.requested("GET /api/v1/objects/dnszones"))
	}
}

func TestGetStructuredOutput(t *testing.T) {
	h := loggedIn(t)
	out := h.mustRun("get", "zone", "home-example-com", "-o", "json")
	var obj map[string]any
	if err := json.Unmarshal([]byte(out), &obj); err != nil || obj["kind"] != "DNSZone" {
		t.Fatalf("single object JSON: %v %q", err, out)
	}
	out = h.mustRun("get", "pol", "-o", "yaml")
	if !strings.HasPrefix(out, "apiVersion: v1\nitems:\n") || !strings.Contains(out, "kind: List") || !strings.Contains(out, "name: kids") {
		t.Fatalf("list YAML:\n%s", out)
	}
	out = h.mustRun("get", "cg", "-n", "empty", "-o", "json")
	if !strings.Contains(out, `"items": []`) {
		t.Fatalf("empty list JSON: %s", out)
	}
}

func TestGetErrors(t *testing.T) {
	h := loggedIn(t)
	_, errOut, err := h.run("get", "rrset", "-n", "nowhere")
	if err != nil || !strings.Contains(errOut, "No resources found in nowhere namespace.") {
		t.Fatalf("empty namespace: %v %q", err, errOut)
	}
	if _, _, err := h.run("get", "widgets"); err == nil || !strings.Contains(err.Error(), "unknown kind") {
		t.Fatalf("unknown kind: %v", err)
	}
	if _, _, err := h.run("get", "zone", "nope"); err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("missing object: %v", err)
	}
	if _, _, err := h.run("get", "zones", "-o", "toml"); err == nil || !strings.Contains(err.Error(), "unknown output format") {
		t.Fatalf("bad -o: %v", err)
	}
	if _, _, err := h.run("get", "rrset", "web", "-A"); err == nil || !strings.Contains(err.Error(), "across all namespaces") {
		t.Fatalf("-A with a name: %v", err)
	}
	if out := h.mustRun("get", "zone", "home-example-com", "-A", "-o", "name"); out != "dnszones/home-example-com\n" {
		t.Fatalf("-A with a cluster-scoped name: %q", out)
	}
}

func TestDescribe(t *testing.T) {
	h := loggedIn(t)
	golden(t, h, "describe-zone", h.mustRun("describe", "zone", "lab-example-com"))
	golden(t, h, "describe-policy", h.mustRun("describe", "pol", "kids"))
}

const applyManifest = `# zones and lists for the lab
apiVersion: kudurru.io/v1alpha1
kind: DNSZone
metadata:
  name: new-example-com
spec:
  zoneName: new.example.com.
  type: primary
---
# a record set without a namespace gets the default one
apiVersion: kudurru.io/v1alpha1
kind: DNSRecordSet
metadata:
  name: web
spec:
  zoneRef: {name: home-example-com}
  endpoints:
  - {dnsName: www.home.example.com, recordType: A, targets: [192.168.1.10]}
  - {dnsName: home.example.com, recordType: MX, targets: ["10 mail.home.example.com."]}
---
---
apiVersion: v1
kind: List
items:
- apiVersion: kudurru.io/v1alpha1
  kind: BlockList
  metadata: {name: ads, namespace: default}
  spec:
    mode: block
    sources: [{name: extra, url: "https://lists.example/more.txt"}]
- apiVersion: kudurru.io/v1alpha1
  kind: ClientGroup
  metadata: {name: iot, namespace: home}
  spec:
    clients: [{cidr: 192.168.50.0/24}]
`

func TestApplyMultiDocument(t *testing.T) {
	h := loggedIn(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "objects.yaml")
	if err := os.WriteFile(file, []byte(applyManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	out := h.mustRun("apply", "-f", file)
	golden(t, h, "apply", out)

	var docs []map[string]any
	if err := json.Unmarshal(h.api.lastBody("POST", "/api/v1/apply"), &docs); err != nil {
		t.Fatal(err)
	}
	if len(docs) != 4 {
		t.Fatalf("sent %d documents, want 4 (empty documents skipped, List expanded)", len(docs))
	}
	if ns := docs[1]["metadata"].(map[string]any)["namespace"]; ns != "default" {
		t.Fatalf("record set namespace = %v, want default", ns)
	}
	if _, ok := docs[0]["metadata"].(map[string]any)["namespace"]; ok {
		t.Fatal("a cluster-scoped DNSZone got a namespace")
	}

	// Applying again is unchanged; stdin works too.
	h.stdin = applyManifest
	out = h.mustRun("apply", "-f", "-")
	if strings.Count(out, "unchanged") != 4 {
		t.Fatalf("second apply:\n%s", out)
	}
	// JSON results with -o json.
	out = h.mustRun("apply", "-f", file, "-o", "json")
	var results []map[string]any
	if err := json.Unmarshal([]byte(out), &results); err != nil || len(results) != 4 {
		t.Fatalf("apply -o json: %v\n%s", err, out)
	}
}

func TestApplyDirectoryAndErrors(t *testing.T) {
	h := loggedIn(t)
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("a.yaml", "apiVersion: kudurru.io/v1alpha1\nkind: Policy\nmetadata: {name: guests}\nspec: {priority: 5}\n")
	write("b.json", `[{"apiVersion":"kudurru.io/v1alpha1","kind":"DNSZone","metadata":{"name":"lab-example-com"},"spec":{"zoneName":"lab.example.com.","type":"primary"}}]`)
	write("notes.txt", "ignored")
	write("sub/c.yaml", "apiVersion: kudurru.io/v1alpha1\nkind: ClientGroup\nmetadata: {name: sub}\nspec: {}\n")

	out, errOut, err := h.run("apply", "-f", dir)
	if err == nil || !strings.Contains(err.Error(), "1 of 2 documents failed") {
		t.Fatalf("err = %v\n%s\n%s", err, out, errOut)
	}
	if !strings.Contains(out, "policy/default/guests created") {
		t.Fatalf("stdout:\n%s", out)
	}
	if !strings.Contains(errOut, "dnszone/lab-example-com: error: managed_by: object is managed by another manager (managed by file:objects.d/zones.yaml) (") || !strings.Contains(errOut, "b.json, document 0") {
		t.Fatalf("stderr:\n%s", errOut)
	}
	// -R descends into sub/.
	out, _, _ = h.run("apply", "-f", dir, "-R")
	if !strings.Contains(out, "clientgroup/default/sub created") {
		t.Fatalf("-R did not apply sub/c.yaml:\n%s", out)
	}

	bad := []struct{ name, body, want string }{
		{"nokind.yaml", "metadata: {name: x}\n", "no kind"},
		{"noname.yaml", "kind: Policy\nmetadata: {}\n", "no metadata.name"},
		{"badyaml.yaml", "kind: [\n", "badyaml.yaml"},
		{"unknown.yaml", "kind: Widget\nmetadata: {name: w}\n", `unknown kind "Widget"`},
	}
	for _, b := range bad {
		write("bad/"+b.name, b.body)
		if _, _, err := h.run("apply", "-f", filepath.Join(dir, "bad", b.name)); err == nil || !strings.Contains(err.Error(), b.want) {
			t.Errorf("%s: err = %v, want %q", b.name, err, b.want)
		}
	}
	if _, _, err := h.run("apply"); err == nil {
		t.Error("apply without -f must fail")
	}
}

func TestDelete(t *testing.T) {
	h := loggedIn(t)
	out := h.mustRun("delete", "cg", "kids")
	if out != "clientgroup/default/kids deleted\n" {
		t.Fatalf("delete output %q", out)
	}
	if len(h.api.requested("DELETE /api/v1/objects/clientgroups/default/kids")) != 1 {
		t.Fatal("wrong delete path")
	}
	out = h.mustRun("delete", "zone", "home-example-com")
	if out != "dnszone/home-example-com deleted\n" || len(h.api.requested("DELETE /api/v1/objects/dnszones/home-example-com")) != 1 {
		t.Fatalf("cluster-scoped delete: %q", out)
	}
	_, errOut, err := h.run("delete", "zone", "lab-example-com", "missing")
	if err == nil || !strings.Contains(errOut, "managed_by: object is managed by a file (managed by file:objects.d/zones.yaml)") || !strings.Contains(errOut, "not_found") {
		t.Fatalf("delete errors: %v\n%s", err, errOut)
	}
	if _, _, err := h.run("delete", "zone", "missing", "--ignore-not-found"); err != nil {
		t.Fatalf("--ignore-not-found: %v", err)
	}
	file := filepath.Join(t.TempDir(), "del.yaml")
	_ = os.WriteFile(file, []byte("apiVersion: kudurru.io/v1alpha1\nkind: Policy\nmetadata: {name: default}\n"), 0o600)
	if out := h.mustRun("delete", "-f", file); out != "policy/default/default deleted\n" {
		t.Fatalf("delete -f: %q", out)
	}
	if _, _, err := h.run("delete", "zone"); err == nil {
		t.Fatal("delete without a name must fail")
	}
}
