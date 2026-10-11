package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConvenienceGolden(t *testing.T) {
	h := loggedIn(t)
	cases := []struct {
		golden string
		args   []string
	}{
		{"zones-list", []string{"zones", "list"}},
		{"zones-get", []string{"zones", "get", "home-example-com"}},
		{"records-list", []string{"records", "list", "home-example-com"}},
		{"records-list-wide", []string{"records", "list", "home-example-com", "-o", "wide"}},
		{"lists-list", []string{"lists", "list", "-A"}},
		{"lists-search", []string{"lists", "search", "ads.example.net"}},
		{"query-forwarded", []string{"query", "www.example.com"}},
		{"query-blocked", []string{"query", "ads.example.net", "a", "--client", "192.168.1.23"}},
		{"stats-summary", []string{"stats", "summary"}},
		{"stats-top", []string{"stats", "top", "--dim", "domains", "--blocked"}},
		{"stats-top-wide", []string{"stats", "top", "--dim", "domains", "-o", "wide"}},
		{"nodes", []string{"nodes"}},
		{"audit", []string{"audit", "--since", "24h"}},
		{"settings-get", []string{"settings", "get"}},
		{"settings-get-key", []string{"settings", "get", "cache.maxEntries", "-o", "json"}},
		{"export", []string{"export", "blocklists", "--safe"}},
	}
	for _, tc := range cases {
		t.Run(tc.golden, func(t *testing.T) {
			golden(t, h, tc.golden, h.mustRunT(t, tc.args...))
		})
	}
	if got := h.api.requested("GET /api/v1/audit"); len(got) != 2 || !strings.Contains(got[0], "since=2026-10-10T12%3A00%3A00Z") {
		t.Fatalf("audit requests %v", got)
	}
	if got := h.api.requested("POST /api/v1/dns/query"); len(got) != 2 {
		t.Fatalf("query requests %v", got)
	}
	if body := string(h.api.lastBody("POST", "/api/v1/dns/query")); body != `{"client":"192.168.1.23","name":"ads.example.net","type":"A"}` {
		t.Fatalf("query body %s", body)
	}
}

func TestZonesImportExportNotify(t *testing.T) {
	h := loggedIn(t)
	zf := filepath.Join(t.TempDir(), "home.zone")
	zone := "$ORIGIN home.example.com.\nwww 300 IN A 192.168.1.10\n"
	if err := os.WriteFile(zf, []byte(zone), 0o600); err != nil {
		t.Fatal(err)
	}
	out := h.mustRun("zones", "import", "home-example-com", "-f", zf)
	if out != "zone home-example-com imported: recordSets 1, records 3\n" {
		t.Fatalf("import output %q", out)
	}
	if got := string(h.api.lastBody("POST", "/api/v1/zones/home-example-com/import")); got != zone {
		t.Fatalf("import body %q", got)
	}
	// `import bind` is the same request.
	h.stdin = zone
	h.mustRun("import", "bind", "home-example-com", "-f", "-")
	if n := len(h.api.requested("POST /api/v1/zones/home-example-com/import")); n != 2 {
		t.Fatalf("import requests %d", n)
	}
	out = h.mustRun("zones", "export", "home-example-com")
	if !strings.HasPrefix(out, "$ORIGIN home.example.com.\n") {
		t.Fatalf("export %q", out)
	}
	if accept := h.api.lastHeader("GET", "/api/v1/zones/home-example-com/export").Get("Accept"); accept != "text/dns" {
		t.Fatalf("export Accept %q", accept)
	}
	file := filepath.Join(t.TempDir(), "out.zone")
	h.mustRun("zones", "export", "home-example-com", "--out", file)
	if b, _ := os.ReadFile(file); !strings.Contains(string(b), "www\t300\tIN\tA") { //nolint:gosec // G304: the test's temp file
		t.Fatalf("export --out wrote %q", b)
	}
	if out := h.mustRun("zones", "notify", "home-example-com"); out != "NOTIFY queued for zone home-example-com\n" {
		t.Fatalf("notify %q", out)
	}
}

func TestRecordsAddRemove(t *testing.T) {
	h := loggedIn(t)
	out := h.mustRun("records", "add", "home-example-com", "nas.home.example.com", "a", "192.168.1.20", "192.168.1.21", "--ttl", "60", "--comment", "the NAS")
	if out != "nas.home.example.com A set in zone home-example-com (record set api-home-example-com)\n" {
		t.Fatalf("add output %q", out)
	}
	var ep map[string]any
	if err := json.Unmarshal(h.api.lastBody("POST", "/api/v1/zones/home-example-com/records"), &ep); err != nil {
		t.Fatal(err)
	}
	if ep["recordType"] != "A" || ep["recordTTL"] != 60.0 || len(ep["targets"].([]any)) != 2 || ep["kudurru"].(map[string]any)["comment"] != "the NAS" {
		t.Fatalf("endpoint body %v", ep)
	}
	h.mustRun("records", "add", "home-example-com", "mail.home.example.com", "MX", "10 mx.example.net.", "--record-set", "mail")
	if got := h.api.requested("POST /api/v1/zones/home-example-com/records?recordSet=mail"); len(got) != 1 {
		t.Fatalf("record-set query: %v", h.api.requested("POST"))
	}
	if out := h.mustRun("records", "rm", "home-example-com", "nas.home.example.com", "a"); out != "nas.home.example.com A removed from zone home-example-com\n" {
		t.Fatalf("rm output %q", out)
	}
	if len(h.api.requested("DELETE /api/v1/zones/home-example-com/records/nas.home.example.com/A")) != 1 {
		t.Fatal("rm path")
	}
	if _, _, err := h.run("records", "rm", "home-example-com", "missing.home.example.com", "A"); err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("rm missing: %v", err)
	}
}

func TestListsBlockingCache(t *testing.T) {
	h := loggedIn(t)
	if out := h.mustRun("lists", "refresh", "ads"); out != "blocklist/default/ads refresh requested\n" {
		t.Fatalf("refresh %q", out)
	}
	if _, _, err := h.run("lists", "refresh", "nope"); err == nil || !strings.Contains(err.Error(), "refresh default/nope: not_found") {
		t.Fatalf("refresh nope: %v", err)
	}
	out, errOut, err := h.run("lists", "deny", "bad.example.com", "not a domain")
	if err == nil || out != "1 added to ui-deny\n" || !strings.Contains(errOut, "invalid domain") {
		t.Fatalf("deny: %v %q %q", err, out, errOut)
	}
	h.mustRun("lists", "allow", `^cdn\d+\.example\.com$`, "--regex", "--list", "cdn-allow")
	if got := string(h.api.lastBody("POST", "/api/v1/domains/allow/regex")); !strings.Contains(got, `"list":"cdn-allow"`) || !strings.Contains(got, `"namespace":"default"`) {
		t.Fatalf("allow body %s", got)
	}

	if out := h.mustRun("blocking", "disable", "5m"); out != "blocking: disabled until 2026-10-11T12:05:00Z (5m0s left)\n" {
		t.Fatalf("disable %q", out)
	}
	if got := string(h.api.lastBody("POST", "/api/v1/dns/blocking")); got != `{"blocking":false,"timer":300}` {
		t.Fatalf("disable body %s", got)
	}
	if out := h.mustRun("blocking", "status"); !strings.Contains(out, "disabled until") {
		t.Fatalf("status %q", out)
	}
	if out := h.mustRun("blocking", "disable"); out != "blocking: disabled\n" {
		t.Fatalf("disable indefinitely %q", out)
	}
	if out := h.mustRun("blocking", "enable"); out != "blocking: enabled\n" {
		t.Fatalf("enable %q", out)
	}
	if out := h.mustRun("blocking", "status", "-o", "json"); !strings.Contains(out, `"blocking": true`) {
		t.Fatalf("status json %q", out)
	}
	if _, _, err := h.run("blocking", "disable", "soon"); err == nil {
		t.Fatal("bad duration accepted")
	}

	if out := h.mustRun("cache", "flush"); out != "cache flushed (4211 entries)\n" {
		t.Fatalf("flush %q", out)
	}
	if out := h.mustRun("cache", "flush", "example.com"); out != "cache entries for example.com flushed (3 entries)\n" {
		t.Fatalf("flush name %q", out)
	}
	if len(h.api.requested("POST /api/v1/cache/flush?name=example.com")) != 1 {
		t.Fatal("flush name is a query parameter")
	}
}

func TestSettingsSet(t *testing.T) {
	h := loggedIn(t)
	out, errOut, err := h.run("settings", "set", "cache.maxEntries=200000", "log.level=debug", "upstreams.bootstrap=9.9.9.9, 1.1.1.1", "cache.minTTL-")
	if err != nil {
		t.Fatalf("set: %v\n%s", err, errOut)
	}
	if out != "cluster settings updated\nnode settings updated\n" {
		t.Fatalf("set output %q", out)
	}
	if got := string(h.api.lastBody("PATCH", "/api/v1/settings")); got != `{"cache":{"maxEntries":200000,"minTTL":null},"upstreams":{"bootstrap":["9.9.9.9","1.1.1.1"]}}` {
		t.Fatalf("cluster patch %s", got)
	}
	if got := string(h.api.lastBody("PATCH", "/api/v1/node/config")); got != `{"log":{"level":"debug"}}` {
		t.Fatalf("node patch %s", got)
	}
	cases := []struct{ arg, want string }{
		{"node.dataDir=/data", "node.dataDir is hostOnly"},
		{"listeners.api.address=:9090", "KUDURRU_LISTENERS__API__ADDRESS"},
		{"nope.key=1", `unknown settings key "nope.key"`},
		{"cache.maxEntries=lots", "wants an integer"},
		{"cache.minTTL=soon", "wants a duration"},
		{"cache.maxEntries", "not key=value"},
	}
	for _, tc := range cases {
		if _, _, err := h.run("settings", "set", tc.arg); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("settings set %s: err = %v, want %q", tc.arg, err, tc.want)
		}
	}
	if n := len(h.api.requested("PATCH")); n != 2 {
		t.Fatalf("rejected keys reached the server: %v", h.api.requested("PATCH"))
	}
}

func TestBackupAndRestore(t *testing.T) {
	h := loggedIn(t)
	file := filepath.Join(t.TempDir(), "backup.yaml")
	_, errOut, err := h.run("backup", "--out", file, "--safe")
	if err != nil || !strings.Contains(errOut, "9 objects written") {
		t.Fatalf("backup: %v %q", err, errOut)
	}
	st, _ := os.Stat(file)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode %v", st.Mode().Perm())
	}
	b, _ := os.ReadFile(file) //nolint:gosec // G304: the test's temp file
	text := string(b)
	for _, want := range []string{"# objects: 9 (dnszones 2, dnsrecordsets 2, blocklists 2, clientgroups 1, policies 2)", "# safe: credentials", "token=REDACTED", "kind: DNSZone", "namespace: apps"} {
		if !strings.Contains(text, want) {
			t.Errorf("backup lacks %q", want)
		}
	}
	for _, unwanted := range []string{"status:", "resourceVersion", "uid:", "creationTimestamp", "kudurru.io/managed-by", "abc123"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("backup keeps %q", unwanted)
		}
	}
	out, errOut, err := h.run("restore", "-f", file)
	if err == nil || !strings.Contains(errOut, "REDACTED values") || !strings.Contains(err.Error(), "1 of 9 documents failed") {
		t.Fatalf("restore: %v\n%s\n%s", err, out, errOut)
	}
	if !strings.Contains(out, "dnszone/home-example-com unchanged") || !strings.Contains(out, "blocklist/default/ads updated") {
		t.Fatalf("restore output:\n%s", out)
	}
	// --exclude-managed leaves file and integration objects out.
	out = h.mustRun("export", "--exclude-managed", "-o", "json")
	var objs []map[string]any
	if err := json.Unmarshal([]byte(out), &objs); err != nil || len(objs) != 7 {
		t.Fatalf("export --exclude-managed: %v, %d objects", err, len(objs))
	}
}

func TestRawAPI(t *testing.T) {
	h := loggedIn(t)
	out := h.mustRun("api", "/version")
	if !strings.Contains(out, `"version": "v0.1.0-test"`) {
		t.Fatalf("api /version: %q", out)
	}
	h.mustRun("api", "PATCH", "/settings", "--data", `{"cache":{"maxEntries":1}}`, "--content-type", "application/merge-patch+json")
	if got := string(h.api.lastBody("PATCH", "/api/v1/settings")); got != `{"cache":{"maxEntries":1}}` {
		t.Fatalf("api body %s", got)
	}
	data := filepath.Join(t.TempDir(), "q.json")
	_ = os.WriteFile(data, []byte(`{"name":"example.com","type":"A"}`), 0o600)
	out = h.mustRun("api", "/dns/query", "--data", "@"+data)
	if !strings.Contains(out, `"status": "forwarded"`) {
		t.Fatalf("api @file (POST implied): %q", out)
	}
	_, errOut, err := h.run("api", "GET", "/objects/dnszones/nope")
	if err == nil || !strings.Contains(err.Error(), "404") || !strings.Contains(errOut, `"key": "not_found"`) {
		t.Fatalf("api 404: %v %q", err, errOut)
	}
}

func TestCompletion(t *testing.T) {
	h := newHarness(t)
	for shell, want := range map[string]string{
		"bash":       "__start_kudurru",
		"zsh":        "#compdef kudurru",
		"fish":       "complete -c kudurru",
		"powershell": "Register-ArgumentCompleter",
	} {
		out := h.mustRun("completion", shell)
		if !strings.Contains(out, want) {
			t.Errorf("completion %s lacks %q", shell, want)
		}
	}
	if _, _, err := h.run("completion", "tcsh"); err == nil {
		t.Error("unsupported shell accepted")
	}
	// Dynamic completion of kinds and of object names from the API.
	out := h.mustRun("__complete", "get", "")
	for _, want := range []string{"dnszones", "rrset", "cg", "pol"} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("kind completion lacks %s:\n%s", want, out)
		}
	}
	h2 := loggedIn(t)
	out = h2.mustRun("__complete", "describe", "zone", "")
	if !strings.Contains(out, "home-example-com\n") || !strings.Contains(out, "lab-example-com\n") {
		t.Errorf("name completion:\n%s", out)
	}
	out = h2.mustRun("__complete", "zones", "export", "")
	if !strings.Contains(out, "home-example-com\n") {
		t.Errorf("zone completion:\n%s", out)
	}
	out = h2.mustRun("__complete", "config", "use-context", "")
	if !strings.Contains(out, "test\n") {
		t.Errorf("context completion:\n%s", out)
	}
}

func TestExistingCommandsStillWork(t *testing.T) {
	// The server-side verbs ignore the client flags and contexts.
	h := newHarness(t)
	if out := h.mustRun("version"); !strings.HasPrefix(out, "kudurru ") {
		t.Fatalf("version %q", out)
	}
	if out := h.mustRun("config", "schema"); !strings.Contains(out, "node.name") {
		t.Fatal("config schema")
	}
}
