package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	internal "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/cli"
	"github.com/robertherbaugh/kudurru/internal/importers"
	"github.com/robertherbaugh/kudurru/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func offlineAcceptanceInput(source string) string {
	if source == "adguard" {
		return `dns: {upstream_dns: [192.0.2.53]}
filtering:
  blocking_mode: nxdomain
  blocked_response_ttl: 30
  rewrites: [{domain: host.migrate.test, answer: 192.0.2.20}]
user_rules: ['||ads.example.test^', '||private.example.test^$client=secret-rule-marker']
users: [{name: admin, password: secret-auth-marker}]
tls: {private_key_path: /secret-key-marker}
`
	}
	return `upstreams: {groups: {default: [192.0.2.53]}, strategy: strict}
blocking:
  blockType: nxDomain
  denylists:
    ads:
      - |
        ads.example.test
  clientGroupsBlock: {default: [ads]}
customDNS: {mapping: {migrate.test: 192.0.2.20}}
keyFile: /secret-key-marker
`
}
func offlineAcceptanceArgs(source string) []string {
	args := []string{"import", source, "-f", "-", "-n", "default", "--nameserver-address", "192.0.2.99", "-o", "json"}
	if source == "adguard" {
		args = append(args, "--local-zone", "migrate.test")
	}
	return args
}
func offlineAcceptanceHarness(t *testing.T, source string, f *servingCommandFixture, s *api.Server) (*harness, *atomic.Int32) {
	t.Helper()
	adoptionWait(t, f)
	var calls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); servingMutationRoutes(s).ServeHTTP(w, r) }))
	t.Cleanup(target.Close)
	h := newHarness(t)
	h.env[cli.EnvServer] = target.URL
	h.env[cli.EnvToken] = f.token
	h.stdin = offlineAcceptanceInput(source)
	return h, &calls
}
func offlineAcceptanceJournal(t *testing.T, f *servingCommandFixture, id, source string) *internal.MigrationRun {
	t.Helper()
	var run internal.MigrationRun
	if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: internal.KindMigrationRun, Name: id}, &run); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(&run)
	if run.Spec.Source != source || run.Spec.SourceURL != "" || strings.Contains(string(raw), "secret-") {
		t.Fatal("source identity, source secrets or paths leaked into journal")
	}
	return &run
}

func TestOfflineCLIActualCurrentApplyAndSelection(t *testing.T) {
	for _, source := range []string{"adguard", "blocky"} {
		t.Run(source, func(t *testing.T) {
			f := newServingCommandFixture(t)
			s := migrationServingAPI(t, f, nil)
			h, calls := offlineAcceptanceHarness(t, source, f, s)
			before, err := f.s.opts.Store.ReadGeneration(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			out, stderr, err := h.run(offlineAcceptanceArgs(source)...)
			if err != nil {
				t.Fatal(err)
			}
			var plan importers.Plan
			if err := json.Unmarshal([]byte(out), &plan); err != nil {
				t.Fatal(err)
			}
			after, err := f.s.opts.Store.ReadGeneration(t.Context())
			if err != nil || !before.Generation().Same(after.Generation()) || calls.Load() != 0 || strings.Contains(out+stderr, "secret-") {
				t.Fatal("offline preview wrote, connected or leaked secrets")
			}
			if _, _, err := h.run(append(offlineAcceptanceArgs(source), "--apply", "--select-object", "Policy/missing")...); err == nil || calls.Load() != 0 {
				t.Fatal("bad selection reached API")
			}
			out, _, err = h.run(append(offlineAcceptanceArgs(source), "--apply", "--wait", "--select-object", "Policy/default/default", "--select-setting", "upstreams")...)
			if err != nil {
				t.Fatal(err, out)
			}
			var selected api.MigrationRun
			if err := json.Unmarshal([]byte(out), &selected); err != nil {
				t.Fatal(err)
			}
			if selected.Data.Status != "completed" || selected.Data.SettingsPersisted == nil || !*selected.Data.SettingsPersisted {
				t.Fatal("selected settings were not applied", out)
			}
			skipped := 0
			for _, item := range selected.Data.Items {
				if item.Result == "skipped" {
					skipped++
				}
			}
			if skipped != len(plan.Objects)-1 {
				t.Fatal("selection was broadened", out)
			}
			for _, z := range plan.Zones {
				var absent public.DNSZone
				if !errors.Is(f.s.opts.Store.Get(t.Context(), store.Key{Kind: "DNSZone", Name: z.Name}, &absent), store.ErrNotFound) {
					t.Fatal("unselected zone was written")
				}
			}
			offlineAcceptanceJournal(t, f, selected.ID, source)
			adoptionWait(t, f)
			// Full imports race several desired-object and primary-head publications.
			// Submit once, then observe durable completion and serving independently
			// of the CLI's deliberately bounded consecutive report-read failures.
			out, _, err = h.run(append(offlineAcceptanceArgs(source), "--apply")...)
			if err != nil {
				t.Fatal(err, out)
			}
			var applied api.MigrationRun
			if err := json.Unmarshal([]byte(out), &applied); err != nil {
				t.Fatal(err)
			}
			offlineAcceptanceWaitRun(t, f, applied.ID, source)
			offlineAcceptanceWaitRecords(t, f, plan)
			out, _, err = h.run("import", "runs", applied.ID, "-o", "json")
			if err != nil {
				t.Fatal(err, out)
			}
			if err := json.Unmarshal([]byte(out), &applied); err != nil {
				t.Fatal(err)
			}
			if applied.Data.Status != "completed" {
				t.Fatal(out)
			}
			offlineAcceptanceJournal(t, f, applied.ID, source)
			for _, z := range plan.Zones {
				var zone public.DNSZone
				if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "DNSZone", Name: z.Name}, &zone); err != nil || len(zone.Spec.Nameservers) != 1 || zone.Spec.SOA.MName != zone.Spec.Nameservers[0] {
					t.Fatal("local records/authority not applied", err, zone.Spec)
				}
			}
			migrationServingDNS(t, f, "host.migrate.test.", 1, "192.0.2.20")
		})
	}
}
func TestOfflineCLIActualOwnershipPartialReport(t *testing.T) {
	for _, source := range []string{"adguard", "blocky"} {
		t.Run(source, func(t *testing.T) {
			f := newServingCommandFixture(t)
			s := migrationServingAPI(t, f, nil)
			h, _ := offlineAcceptanceHarness(t, source, f, s)
			out, _, err := h.run(offlineAcceptanceArgs(source)...)
			if err != nil {
				t.Fatal(err)
			}
			var plan importers.Plan
			if err := json.Unmarshal([]byte(out), &plan); err != nil {
				t.Fatal(err)
			}
			name := ""
			for _, doc := range plan.Objects {
				if doc["kind"] == "BlockList" {
					name = doc["metadata"].(map[string]any)["name"].(string)
					break
				}
			}
			managed := &public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: public.BlockListSpec{Mode: "block", Sources: []public.ListSource{{Name: "file", Inline: []string{"existing.test"}}}}}
			if _, err := f.s.opts.Store.Apply(t.Context(), managed, store.ApplyOptions{Manager: store.ManagerFile("owned.yaml")}); err != nil {
				t.Fatal(err)
			}
			adoptionWait(t, f)
			// Exercise --wait's partial-result exit with filtering/settings work;
			// primary publication is checked separately by the full-import test.
			args := append(offlineAcceptanceArgs(source), "--apply", "--wait")
			for _, doc := range plan.Objects {
				if doc["kind"] != "DNSZone" && doc["kind"] != "DNSRecordSet" {
					args = append(args, "--select-object", importers.ObjectID(doc))
				}
			}
			settingKeys := make([]string, 0, len(plan.Settings))
			for key := range plan.Settings {
				settingKeys = append(settingKeys, key)
			}
			sort.Strings(settingKeys)
			for _, key := range settingKeys {
				args = append(args, "--select-setting", key)
			}
			out, _, err = h.run(args...)
			if err == nil {
				t.Fatal("partial import exited successfully")
			}
			var run api.MigrationRun
			if err := json.Unmarshal([]byte(out), &run); err != nil {
				t.Fatal(err)
			}
			if run.Data.Status != "partial" || run.Data.Items[0].ErrorKey != api.KeyManagedBy || run.Data.Items[len(run.Data.Items)-1].Result != "not_applied" {
				t.Fatal("partial object/settings outcome lost", out)
			}
			successes := 0
			for _, item := range run.Data.Items {
				if item.Result == api.ResultCreated || item.Result == api.ResultUpdated {
					successes++
				}
			}
			if successes == 0 {
				t.Fatal("successful objects were not retained")
			}
			offlineAcceptanceJournal(t, f, run.ID, source)
		})
	}
}
func TestOfflineCLIWaitCancellationKeepsTrackedJob(t *testing.T) {
	for _, source := range []string{"adguard", "blocky"} {
		t.Run(source, func(t *testing.T) {
			f := newServingCommandFixture(t)
			s := migrationServingAPI(t, f, nil)
			h, _ := offlineAcceptanceHarness(t, source, f, s)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var out, stderr syncBuffer
			done := make(chan error, 1)
			go func() {
				done <- h.runCtx(ctx, &out, &stderr, append(offlineAcceptanceArgs(source), "--apply", "--wait")...)
			}()
			until := time.Now().Add(10 * time.Second)
			for !strings.Contains(stderr.String(), "Import started:") {
				if time.Now().After(until) {
					t.Fatal("CLI never reported accepted import", stderr.String())
				}
				time.Sleep(time.Millisecond)
			}
			cancel()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "background") || !errors.Is(err, context.Canceled) {
					t.Fatal("wait cancellation lost job identity", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("canceled wait did not exit")
			}
			fields := strings.Fields(stderr.String())
			id := ""
			for i, field := range fields {
				if field == "started:" && i+1 < len(fields) {
					id = strings.TrimSuffix(fields[i+1], ".")
					break
				}
			}
			journal := importSettingsJournal(t, f, s, id)
			if journal.Data.Status != "completed" {
				t.Fatal("caller canceled durable work", journal)
			}
			offlineAcceptanceJournal(t, f, id, source)
		})
	}
}

func TestOfflineCLIGeneratedPlanLimitBeforePreviewOrWrites(t *testing.T) {
	for _, apply := range []bool{false, true} {
		t.Run(map[bool]string{false: "preview", true: "apply"}[apply], func(t *testing.T) {
			h := newHarness(t)
			h.stdin = "small input"
			calls := 0
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(500) }))
			defer target.Close()
			h.env[cli.EnvServer] = target.URL
			h.env[cli.EnvToken] = "destination-token"
			opts := h.opts()
			root := newRootWith(opts)
			importCmd, _, err := root.Find([]string{"import"})
			if err != nil {
				t.Fatal(err)
			}
			oversized := &importers.Plan{Source: &importers.SourceInfo{Type: "adguard"}, Objects: []map[string]any{{"kind": "Policy", "metadata": map[string]any{"name": "oversized", "namespace": "default", "annotations": map[string]any{"padding": strings.Repeat("x", api.MaxRequestBodyBytes)}}}}, Settings: map[string]any{}, Warnings: []string{}}
			importCmd.AddCommand(newOfflineMigrationCmd(opts, "oversized", func(context.Context, importers.OfflineRequest, io.Reader) (*importers.Plan, error) {
				return oversized, nil
			}))
			args := []string{"--cli-config", h.cfgPath, "import", "oversized", "-f", "-", "--select-setting", "upstreams"}
			// Retain an enormous unselected object: selection must not hide its body size.
			oversized.Settings["upstreams"] = map[string]any{}
			if apply {
				args = append(args, "--apply")
			}
			var out, stderr syncBuffer
			root.SetOut(&out)
			root.SetErr(&stderr)
			root.SetArgs(args)
			err = root.ExecuteContext(t.Context())
			if err == nil || !strings.Contains(err.Error(), "32 MiB") || calls != 0 || out.String() != "" {
				t.Fatal("oversized generated plan was printed or submitted", err, calls)
			}
		})
	}
}

func TestOfflineCLIContextNamespaceWithoutDestinationAccess(t *testing.T) {
	for _, source := range []string{"adguard", "blocky"} {
		t.Run(source, func(t *testing.T) {
			h := newHarness(t)
			h.stdin = offlineAcceptanceInput(source)
			contents := `apiVersion: kudurru.io/v1alpha1
kind: Config
currentContext: offline
contexts:
  - name: offline
    server: http://127.0.0.1:1
    namespace: saved-namespace
`
			if err := os.MkdirAll(filepath.Dir(h.cfgPath), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(h.cfgPath, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			out, _, err := h.run("import", source, "-f", "-", "-o", "json")
			if err != nil {
				t.Fatal(err)
			}
			var p importers.Plan
			if err := json.Unmarshal([]byte(out), &p); err != nil {
				t.Fatal(err)
			}
			for _, doc := range p.Objects {
				if doc["kind"] != "DNSZone" && doc["metadata"].(map[string]any)["namespace"] != "saved-namespace" {
					t.Fatal("ignored local context namespace")
				}
			}
		})
	}
}

func TestOfflineCLIRefusedStartIsNeverReplayed(t *testing.T) {
	for _, source := range []string{"adguard", "blocky"} {
		for _, status := range []int{http.StatusForbidden, http.StatusConflict, http.StatusServiceUnavailable} {
			t.Run(fmt.Sprintf("%s/%d", source, status), func(t *testing.T) {
				h := newHarness(t)
				h.stdin = offlineAcceptanceInput(source)
				calls := 0
				target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					if r.Method != http.MethodPost || r.URL.Path != "/api/v1/imports/start" {
						t.Error("unexpected request")
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					_, _ = io.WriteString(w, `{"error":{"key":"unavailable","message":"refused"}}`)
				}))
				defer target.Close()
				h.env[cli.EnvServer] = target.URL
				h.env[cli.EnvToken] = "destination-token"
				_, _, err := h.run(append(offlineAcceptanceArgs(source), "--apply", "--wait")...)
				if err == nil || calls != 1 {
					t.Fatal("refused import-start request was replayed", err, calls)
				}
			})
		}
	}
}
