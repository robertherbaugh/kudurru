package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/cli"
	"github.com/robertherbaugh/kudurru/internal/importers"
	"github.com/robertherbaugh/kudurru/internal/store"
)

const corednsCLIInput = `# secret-comment-marker
. {
    errors
    forward . 192.0.2.53 {
        policy sequential
    }
    hosts /etc/secret-hosts-marker {
        192.0.2.20 host.migrate.test
        fallthrough
    }
    acl {
        block type ANY
        allow net 192.0.2.0/24
        block
    }
    cache 600
}
corp.test {
    forward . 192.0.2.54
    file /etc/coredns/secret-db-marker
}
`

func TestCoreDNSCLIPreviewAndSelection(t *testing.T) {
	h := newHarness(t)
	calls := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(500) }))
	defer target.Close()
	h.env[cli.EnvServer] = target.URL
	h.env[cli.EnvToken] = "destination-token"
	h.stdin = corednsCLIInput
	var first string
	for _, flags := range [][]string{nil, {"--dry-run"}, {"--skip-settings"}, {"--select-setting", "upstreams"}, {"--select-object", "Policy/default/default", "--select-setting", "control"}} {
		out, stderr, err := h.run(append([]string{"import", "coredns", "-f", "-", "-n", "default", "-o", "json"}, flags...)...)
		if err != nil {
			t.Fatal(err)
		}
		if calls != 0 || strings.Contains(out+stderr, "secret-") {
			t.Fatal("preview contacted destination or leaked a secret/path")
		}
		var plan importers.Plan
		if err := json.Unmarshal([]byte(out), &plan); err != nil {
			t.Fatal(err)
		}
		if plan.Source.Type != "coredns" || plan.Source.URL != "" || len(plan.Objects) != 1 || len(plan.Settings) != 3 {
			t.Fatalf("plan shape: %s", out)
		}
		if flags == nil {
			first = out
		}
	}
	again, _, err := h.run("import", "coredns", "-f", "-", "-n", "default", "-o", "json")
	if err != nil || again != first {
		t.Fatal("CLI preview is not deterministic", err)
	}
	for _, flags := range [][]string{{"--wait"}, {"--apply", "--dry-run"}, {"--apply", "--select-object", "Policy/missing"}, {"--skip-settings", "--select-setting", "upstreams"}} {
		_, _, err := h.run(append([]string{"import", "coredns", "-f", "-"}, flags...)...)
		if err == nil || calls != 0 {
			t.Fatal("invalid flags/selection reached destination")
		}
	}
	h.stdin = "not a corefile {"
	if _, _, err := h.run("import", "coredns", "-f", "-"); err == nil || calls != 0 {
		t.Fatal("malformed Corefile accepted or reached destination")
	}
}

// The converted plan applies through the mounted tracked migration with
// normal validation: the policy rewrites and every settings section persist.
func TestCoreDNSCLIActualApply(t *testing.T) {
	f := newServingCommandFixture(t)
	probe := newOfflineStepProbe()
	s := migrationServingAPI(t, f, probe.wrap(f.s.MigrationCommand))
	h, calls := offlineAcceptanceHarness(t, "coredns", f, s)
	h.stdin = corednsCLIInput
	args := []string{"import", "coredns", "-f", "-", "-n", "default", "-o", "json"}
	before, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	out, stderr, err := h.run(args...)
	if err != nil {
		t.Fatal(err)
	}
	after, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil || !before.Generation().Same(after.Generation()) || calls.Load() != 0 || strings.Contains(out+stderr, "secret-") {
		t.Fatal("offline preview wrote, connected or leaked secrets")
	}
	var plan importers.Plan
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatal(err)
	}
	out, _, err = h.run(append(args, "--apply", "--wait")...)
	if err != nil {
		t.Fatal(err, out)
	}
	var run api.MigrationRun
	if err := json.Unmarshal([]byte(out), &run); err != nil {
		t.Fatal(err)
	}
	if run.Data.Status != "completed" || run.Data.SettingsPersisted == nil || !*run.Data.SettingsPersisted {
		t.Fatal("converted plan was not applied", out)
	}
	offlineAcceptanceJournal(t, f, run.ID, "coredns")
	var pol public.Policy
	if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "Policy", Namespace: "default", Name: "default"}, &pol); err != nil {
		t.Fatal(err)
	}
	if len(pol.Spec.Rewrites) != 2 || pol.Spec.Rewrites[1].Name != "host.migrate.test" || pol.Spec.Rewrites[1].Targets[0] != "192.0.2.20" || pol.Spec.Rewrites[0].Type != "PTR" {
		t.Fatalf("policy rewrites: %+v", pol.Spec.Rewrites)
	}
	out, _, err = h.run("settings", "get", "-o", "json")
	if err != nil {
		t.Fatal(err, out)
	}
	for _, want := range []string{`"192.0.2.53"`, `"192.0.2.54"`, `"corp.test."`, `"192.0.2.0/24"`, `"ANY"`, `"10m0s"`} {
		if !strings.Contains(out, want) {
			t.Errorf("persisted settings lack %s", want)
		}
	}
}
