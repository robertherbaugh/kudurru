package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	internal "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api/apitest"
	"github.com/robertherbaugh/kudurru/internal/importers"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func migrationPlan(names ...string) importers.Plan {
	p := importers.Plan{Source: &importers.SourceInfo{Type: "technitium", URL: "https://source.example/console"}, Objects: []map[string]any{}, Settings: map[string]any{}, Warnings: []string{}}
	for _, n := range names {
		doc := zoneDoc(n, n+".test.")
		doc["spec"].(map[string]any)["soa"] = map[string]any{"mname": "old.dns.test.", "rname": "hostmaster.test.", "refresh": 3600, "retry": 600, "expire": 86400, "minimum": 300}
		p.Objects = append(p.Objects, doc)
	}
	return p
}
func importRun(t *testing.T, h *harness, p importers.Plan) MigrationRun {
	t.Helper()
	var result MigrationResult
	h.do(http.MethodPost, "/api/v1/imports/apply", "admin", p).want(http.StatusOK).json(&result)
	if result.JournalError != "" {
		t.Fatal(result.JournalError)
	}
	for _, item := range result.Items {
		if item.Result == ResultError {
			t.Fatal(item.Error)
		}
	}
	var run MigrationRun
	h.do(http.MethodGet, "/api/v1/imports/runs/"+result.RunID, "admin", nil).want(http.StatusOK).json(&run)
	return run
}
func previewCutover(t *testing.T, h *harness, id string, names ...string) MigrationCutoverPlan {
	t.Helper()
	var plan MigrationCutoverPlan
	h.do(http.MethodPost, "/api/v1/imports/runs/"+id+"/decommission/preview", "admin", map[string]any{"nameservers": names}).want(http.StatusOK).json(&plan)
	return plan
}
func getMigrationZone(t *testing.T, h *harness, name string) *v1alpha1.DNSZone {
	t.Helper()
	var z v1alpha1.DNSZone
	if err := h.srv.opts.Store.Get(context.Background(), store.Key{Kind: "DNSZone", Name: name}, &z); err != nil {
		t.Fatal(err)
	}
	return &z
}
func applyCutover(t *testing.T, h *harness, id string, p MigrationCutoverPlan) MigrationResult {
	t.Helper()
	var result MigrationResult
	h.do(http.MethodPost, "/api/v1/imports/runs/"+id+"/decommission", "admin", p).want(http.StatusOK).json(&result)
	return result
}

func TestMigrationHistoryAndCutoverPreserveRecords(t *testing.T) {
	h := newHarness(t)
	p := migrationPlan("one")
	p.Source.URL = "https://ignored-user:secret@source.example/secret-path?token=secret#secret"
	run := importRun(t, h, p)
	if run.Data.Status != "completed" || run.Data.SourceURL != "https://source.example" || len(run.Data.Items) != 1 || len(run.Data.Zones) != 1 || run.Data.Zones[0].UID == "" {
		t.Fatalf("journal: %+v", run)
	}
	if strings.Contains(fmt.Sprint(run), "secret") {
		t.Fatal("credential URL leaked")
	}
	before := getMigrationZone(t, h, "one")
	if before.Spec.SOA.MName != "old.dns.test." {
		t.Fatal("source authority was not preserved")
	}
	h.seed(store.ManagerAPI, recordSetObj("default", "web", "one", ep("web.one.test.", "A", "192.0.2.10")))
	var records v1alpha1.DNSRecordSet
	if err := h.st.Get(context.Background(), store.Key{Kind: "DNSRecordSet", Namespace: "default", Name: "web"}, &records); err != nil {
		t.Fatal(err)
	}
	plan := previewCutover(t, h, run.ID, " DNS1.KUDURRU.TEST ", "dns2.kudurru.test.", "dns1.kudurru.test")
	if len(plan.Nameservers) != 2 || plan.Zones[0].Primary != "old.dns.test." {
		t.Fatalf("preview: %+v", plan)
	}
	result := applyCutover(t, h, run.ID, plan)
	if result.JournalError != "" || result.Items[0].Result != ResultUpdated {
		t.Fatalf("cutover: %+v", result)
	}
	after := getMigrationZone(t, h, "one")
	want := before.Spec
	want.Nameservers = plan.Nameservers
	want.SOA.MName = plan.Nameservers[0]
	if !reflect.DeepEqual(after.Spec, want) {
		t.Fatalf("cutover changed other zone settings: %+v", after.Spec)
	}
	var afterRecords v1alpha1.DNSRecordSet
	if err := h.st.Get(context.Background(), store.KeyOf(&records), &afterRecords); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(records, afterRecords) {
		t.Fatal("cutover rewrote records")
	}
	// A new API instance sees the same journal; history is not process state.
	h2 := newHarness(t, func(o *Options) { o.Store = h.st })
	var reread MigrationRun
	h2.do(http.MethodGet, "/api/v1/imports/runs/"+run.ID, "admin", nil).want(http.StatusOK).json(&reread)
	if len(reread.Data.Cutovers) != 1 || reread.Data.Cutovers[0].Status != "completed" || reread.Data.Cutovers[0].Items[0].Result != ResultUpdated {
		t.Fatalf("missing cutover log: %+v", reread)
	}
	// Repeating a fresh preview/apply is harmless; stale run revisions fail.
	h.do(http.MethodPost, "/api/v1/imports/runs/"+run.ID+"/decommission", "admin", plan).want(http.StatusPreconditionFailed)
	retry := applyCutover(t, h, run.ID, previewCutover(t, h, run.ID, plan.Nameservers...))
	if retry.Items[0].Result != ResultUnchanged {
		t.Fatalf("retry: %+v", retry)
	}
}
func TestMigrationCutoverRejectsStaleZonesAndReimports(t *testing.T) {
	h := newHarness(t)
	run := importRun(t, h, migrationPlan("one"))
	p := previewCutover(t, h, run.ID, "dns.kudurru.test")
	z := getMigrationZone(t, h, "one")
	z.Spec.DefaultTTL = 99
	h.seed(store.ManagerAPI, z)
	h.do(http.MethodPost, "/api/v1/imports/runs/"+run.ID+"/decommission", "admin", p).want(http.StatusConflict)
	if getMigrationZone(t, h, "one").Spec.SOA.MName != "old.dns.test." {
		t.Fatal("stale preview wrote authority")
	}
	// Unrelated edits are preserved after a fresh preview.
	result := applyCutover(t, h, run.ID, previewCutover(t, h, run.ID, "dns.kudurru.test"))
	if result.Items[0].Result == ResultError || getMigrationZone(t, h, "one").Spec.DefaultTTL != 99 {
		t.Fatalf("fresh preview: %+v", result)
	}
	importRun(t, h, migrationPlan("one"))
	p = previewCutover(t, h, run.ID, "dns.other.test")
	if p.Zones[0].Error == "" {
		t.Fatal("old run accepted reimported zone")
	}
	result = applyCutover(t, h, run.ID, p)
	if result.Items[0].Result != ResultError {
		t.Fatal("old run overwrote reimport")
	}
}
func TestMigrationCutoverRejectsEditedAuthorityAndRecreatedUID(t *testing.T) {
	for _, mutation := range []string{"authority", "uid", "owner"} {
		t.Run(mutation, func(t *testing.T) {
			h := newHarness(t)
			run := importRun(t, h, migrationPlan("one"))
			z := getMigrationZone(t, h, "one")
			switch mutation {
			case "authority":
				z.Spec.Nameservers = []string{"manual.test."}
			case "uid":
				if err := h.st.Delete(context.Background(), store.KeyOf(z), store.DeleteOptions{Manager: store.ManagerAPI}); err != nil {
					t.Fatal(err)
				}
				z.UID = ""
			case "owner":
				z.Annotations[migrationRunAnnotation] = "different-run"
			}
			h.seed(store.ManagerAPI, z)
			p := previewCutover(t, h, run.ID, "dns.kudurru.test")
			if p.Zones[0].Error == "" {
				t.Fatal("accepted replaced authority/identity")
			}
		})
	}
}
func TestMigrationCutoverPartialFailureAndRetry(t *testing.T) {
	h := newHarness(t)
	run := importRun(t, h, migrationPlan("one", "two"))
	z := getMigrationZone(t, h, "two")
	if _, err := h.st.Apply(context.Background(), z, store.ApplyOptions{Manager: store.ManagerFile("zones.yaml"), Force: true}); err != nil {
		t.Fatal(err)
	}
	result := applyCutover(t, h, run.ID, previewCutover(t, h, run.ID, "dns.kudurru.test"))
	if result.Items[0].Result != ResultUpdated || result.Items[1].Result != ResultError {
		t.Fatalf("partial cutover: %+v", result)
	}
	z = getMigrationZone(t, h, "two")
	if _, err := h.st.Apply(context.Background(), z, store.ApplyOptions{Manager: store.ManagerAPI, Force: true}); err != nil {
		t.Fatal(err)
	}
	result = applyCutover(t, h, run.ID, previewCutover(t, h, run.ID, "dns.kudurru.test"))
	if result.Items[0].Result != ResultUnchanged || result.Items[1].Result != ResultUpdated {
		t.Fatalf("partial retry: %+v", result)
	}
}
func TestMigrationCutoverPermissionsAndValidation(t *testing.T) {
	perms, err := rbac.LoadPermissions()
	if err != nil {
		t.Fatal(err)
	}
	perms.Roles["settings-writer"] = []string{"settings.write"}
	authz, err := rbac.NewBindingAuthorizer(perms, append(apitest.StandardBindings(), apitest.Grant("settings-writer", "settings-writer")))
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(o *Options) { o.Authorizer = authz })
	run := importRun(t, h, migrationPlan("one"))
	for _, user := range []string{"viewer", "operator", "zoneadmin"} {
		h.do(http.MethodGet, "/api/v1/imports/runs", user, nil).want(http.StatusForbidden)
	}
	var p MigrationCutoverPlan
	h.do(http.MethodPost, "/api/v1/imports/runs/"+run.ID+"/decommission/preview", "settings-writer", map[string]any{"nameservers": []string{"dns.test"}}).want(http.StatusOK).json(&p)
	if !strings.Contains(p.Zones[0].Error, "permission") {
		t.Fatal("settings permission bypassed zone authorization")
	}
	var result MigrationResult
	h.do(http.MethodPost, "/api/v1/imports/runs/"+run.ID+"/decommission", "settings-writer", p).want(http.StatusOK).json(&result)
	if result.Items[0].Result != ResultError {
		t.Fatal("settings-only cutover wrote zone")
	}
	for _, names := range [][]string{{}, {"192.0.2.1"}, {"bad_name.test"}} {
		h.do(http.MethodPost, "/api/v1/imports/runs/"+run.ID+"/decommission/preview", "admin", map[string]any{"nameservers": names}).want(http.StatusBadRequest)
	}
}
func TestMigrationInterruptedCutoverRecovers(t *testing.T) {
	now := time.Now()
	h := newHarness(t, func(o *Options) { o.Now = func() time.Time { return now } })
	run := importRun(t, h, migrationPlan("one"))
	stored, err := h.srv.getMigrationRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.Spec.Cutovers = append(stored.Spec.Cutovers, internal.MigrationCutover{StartedAt: metav1.NewTime(now), Status: "running", Nameservers: []string{"first.kudurru.test."}})
	if err := h.srv.saveMigrationRun(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	h.do(http.MethodPost, "/api/v1/imports/runs/"+run.ID+"/decommission/preview", "admin", map[string]any{"nameservers": []string{"second.kudurru.test"}}).want(http.StatusConflict)
	// Simulate a write committed before a process crash, without its progress save.
	z := getMigrationZone(t, h, "one")
	z.Spec.Nameservers = []string{"first.kudurru.test."}
	z.Spec.SOA.MName = "first.kudurru.test."
	h.seed(store.ManagerAPI, z)
	now = now.Add(11 * time.Minute)
	result := applyCutover(t, h, run.ID, previewCutover(t, h, run.ID, "second.kudurru.test"))
	if result.Items[0].Result == ResultError {
		t.Fatalf("recovery failed: %+v", result)
	}
}

type failingMigrationJournal struct {
	store.Store
	writes, failAt int
}

func (f *failingMigrationJournal) Apply(ctx context.Context, obj store.Object, opts store.ApplyOptions) (store.Object, error) {
	if store.KindOf(obj) == internal.KindMigrationRun {
		f.writes++
		if f.writes == f.failAt {
			return nil, store.ErrClosed
		}
	}
	return f.Store.Apply(ctx, obj, opts)
}
func TestMigrationJournalFailureStopsWrites(t *testing.T) {
	for _, failAt := range []int{1, 2, 3} {
		t.Run(fmt.Sprint(failAt), func(t *testing.T) {
			h := newHarness(t)
			f := &failingMigrationJournal{Store: h.st, failAt: failAt}
			h.srv.opts.Store = f
			r := h.do(http.MethodPost, "/api/v1/imports/apply", "admin", migrationPlan("one", "two"))
			if failAt == 1 {
				r.want(http.StatusServiceUnavailable)
			} else {
				r.want(http.StatusOK)
				var result MigrationResult
				r.json(&result)
				if result.JournalError == "" || len(result.Items) != failAt-2 {
					t.Fatalf("journal failure: %+v", result)
				}
				var run MigrationRun
				h.do(http.MethodGet, "/api/v1/imports/runs/"+result.RunID, "admin", nil).want(http.StatusOK).json(&run)
				if run.Data.Status != "partial" {
					t.Fatal("incomplete run marked completed")
				}
			}
			var z v1alpha1.DNSZone
			if err := h.st.Get(context.Background(), store.Key{Kind: "DNSZone", Name: "two"}, &z); err == nil {
				t.Fatal("writes continued without journal")
			}
		})
	}
}

func TestMigrationTracksPreviousImportsWithoutReplacingRecords(t *testing.T) {
	h := newHarness(t)
	// Model an API apply made before journals were available.
	p := migrationPlan("legacy")
	// Stable IDs hash the source zone spelling without its trailing dot.
	sum := sha256.Sum256([]byte("legacy.test"))
	name := fmt.Sprintf("technitium-legacy-test-%x", sum[:4])
	p.Objects[0]["metadata"].(map[string]any)["name"] = name
	h.do(http.MethodPost, "/api/v1/apply", "admin", p.Objects).want(http.StatusOK)
	h.seed(store.ManagerAPI, recordSetObj("default", "legacy-records", name, ep("web.legacy.test.", "A", "192.0.2.50")))
	before := getMigrationZone(t, h, name)
	var candidates List[MigrationAuthorityChange]
	h.do(http.MethodGet, "/api/v1/imports/existing", "admin", nil).want(http.StatusOK).json(&candidates)
	if len(candidates.Items) != 1 || candidates.Items[0].Name != name {
		t.Fatalf("candidates: %+v", candidates)
	}
	var result MigrationResult
	h.do(http.MethodPost, "/api/v1/imports/adopt", "admin", map[string]any{"zones": candidates.Items}).want(http.StatusOK).json(&result)
	if len(result.Items) != 1 || result.Items[0].Result == ResultError {
		t.Fatalf("adopt: %+v", result)
	}
	after := getMigrationZone(t, h, name)
	if !reflect.DeepEqual(before.Spec, after.Spec) {
		t.Fatal("adoption changed zone data")
	}
	var run MigrationRun
	h.do(http.MethodGet, "/api/v1/imports/runs/"+result.RunID, "admin", nil).want(http.StatusOK).json(&run)
	if run.Data.Source != "existing" || run.Data.Zones[0].UID != string(before.UID) {
		t.Fatalf("adopted run: %+v", run)
	}
	h.do(http.MethodPost, "/api/v1/imports/adopt", "admin", map[string]any{"zones": candidates.Items}).want(http.StatusConflict)
	cutover := applyCutover(t, h, run.ID, previewCutover(t, h, run.ID, "dns.kudurru.test"))
	if cutover.Items[0].Result != ResultUpdated {
		t.Fatalf("adopt cutover: %+v", cutover)
	}
	var records v1alpha1.DNSRecordSet
	if err := h.st.Get(context.Background(), store.Key{Kind: "DNSRecordSet", Namespace: "default", Name: "legacy-records"}, &records); err != nil || records.Spec.Endpoints[0].Targets[0] != "192.0.2.50" {
		t.Fatal("lost previous records")
	}
}

func TestMigrationCutoverCreatesMissingSOA(t *testing.T) {
	h := newHarness(t)
	p := migrationPlan("one")
	delete(p.Objects[0]["spec"].(map[string]any), "soa")
	run := importRun(t, h, p)
	result := applyCutover(t, h, run.ID, previewCutover(t, h, run.ID, "dns.kudurru.test"))
	if result.Items[0].Result != ResultUpdated || getMigrationZone(t, h, "one").Spec.SOA.MName != "dns.kudurru.test." {
		t.Fatalf("cutover without explicit source SOA: %+v", result)
	}
}

func TestMigrationVIPCutover(t *testing.T) {
	h := newHarness(t)
	run := importRun(t, h, migrationPlan("one", "two"))
	var p MigrationCutoverPlan
	h.do(http.MethodPost, "/api/v1/imports/runs/"+run.ID+"/decommission/preview", "admin", map[string]any{"nameservers": []string{}, "addresses": []string{"192.0.2.53/24", "2001:db8::53"}}).want(http.StatusOK).json(&p)
	if len(p.Zones) != 2 || len(p.Zones[0].TargetNameservers) != 2 || p.Zones[0].AddressRecordSet == nil || p.Zones[0].AddressRecordSet.Spec.Endpoints[0].Targets[0] != "192.0.2.53" {
		t.Fatalf("preview: %+v", p)
	}
	result := applyCutover(t, h, run.ID, p)
	if len(result.Items) != 4 || result.JournalError != "" {
		t.Fatalf("result: %+v", result)
	}
	for _, item := range result.Items {
		if item.Result == ResultError {
			t.Fatal(item.Error)
		}
	}
	for i, name := range []string{"one", "two"} {
		z := getMigrationZone(t, h, name)
		if !reflect.DeepEqual(z.Spec.Nameservers, p.Zones[i].TargetNameservers) || z.Spec.SOA.MName != p.Zones[i].TargetNameservers[0] {
			t.Fatalf("authority: %+v", z.Spec)
		}
		var rs v1alpha1.DNSRecordSet
		reviewed := p.Zones[i].AddressRecordSet
		if err := h.st.Get(context.Background(), store.Key{Kind: "DNSRecordSet", Namespace: reviewed.Namespace, Name: reviewed.Name}, &rs); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(rs.Spec, reviewed.Spec) {
			t.Fatalf("records: %+v", rs.Spec)
		}
	}
	h.do(http.MethodGet, "/api/v1/imports/runs/"+run.ID, "admin", nil).want(http.StatusOK).json(&run)
	if run.Data.Cutovers[0].Status != "completed" || len(run.Data.Cutovers[0].Addresses) != 2 || len(run.Data.Cutovers[0].Items) != 4 {
		t.Fatalf("journal: %+v", run.Data)
	}
	var retry MigrationCutoverPlan
	h.do(http.MethodPost, "/api/v1/imports/runs/"+run.ID+"/decommission/preview", "admin", map[string]any{"nameservers": []string{}, "addresses": p.Addresses}).want(http.StatusOK).json(&retry)
	result = applyCutover(t, h, run.ID, retry)
	for _, item := range result.Items {
		if item.Result != ResultUnchanged {
			t.Fatalf("retry changed objects: %+v", result)
		}
	}
}

func TestMigrationVIPCutoverRefusesConflictsAndStaleRecords(t *testing.T) {
	h := newHarness(t)
	run := importRun(t, h, migrationPlan("one"))
	preview := func() MigrationCutoverPlan {
		var p MigrationCutoverPlan
		h.do(http.MethodPost, "/api/v1/imports/runs/"+run.ID+"/decommission/preview", "admin", map[string]any{"nameservers": []string{}, "addresses": []string{"192.0.2.53"}}).want(http.StatusOK).json(&p)
		return p
	}
	p := preview()
	h.seed(store.ManagerAPI, recordSetObj("other", "conflict", "one", ep("ns-c0000235.one.test.", "CNAME", "old.test.")))
	h.do(http.MethodPost, "/api/v1/imports/runs/"+run.ID+"/decommission", "admin", p).want(http.StatusConflict)
	if p := preview(); p.Zones[0].Error == "" {
		t.Fatal("ignored existing alias")
	}
	if getMigrationZone(t, h, "one").Spec.SOA.MName != "old.dns.test." {
		t.Fatal("changed authority despite conflict")
	}
}

type countingMigrationRecords struct {
	store.Store
	scans, pages int
}

func (s *countingMigrationRecords) List(ctx context.Context, kind store.Kind, opts store.ListOptions) (store.ObjectList, error) {
	if kind == "DNSRecordSet" {
		if opts.Continue == "" {
			s.scans++
		}
		s.pages++
		// Exercise pagination as well as grouping records across namespaces.
		opts.Limit = 2
	}
	return s.Store.List(ctx, kind, opts)
}

func TestMigrationVIPCutoverScansRecordsOncePerRequest(t *testing.T) {
	h := newHarness(t)
	run := importRun(t, h, migrationPlan("one", "two", "three"))
	for _, name := range []string{"one", "two", "three"} {
		h.seed(store.ManagerAPI, recordSetObj("other", name, name, ep("www."+name+".test.", "A", "192.0.2.10")))
	}
	counter := &countingMigrationRecords{Store: h.st}
	h.srv.opts.Store = counter
	var p MigrationCutoverPlan
	h.do(http.MethodPost, "/api/v1/imports/runs/"+run.ID+"/decommission/preview", "admin", map[string]any{"nameservers": []string{}, "addresses": []string{"192.0.2.53"}}).want(http.StatusOK).json(&p)
	if counter.scans != 1 || counter.pages != 2 {
		t.Fatalf("preview scans=%d pages=%d; want 1 paginated scan", counter.scans, counter.pages)
	}
	counter.scans, counter.pages = 0, 0
	result := applyCutover(t, h, run.ID, p)
	if len(result.Items) != 6 || result.JournalError != "" {
		t.Fatalf("cutover: %+v", result)
	}
	for _, item := range result.Items {
		if item.Result == ResultError {
			t.Fatal(item.Error)
		}
	}
	if counter.scans != 1 || counter.pages != 2 {
		t.Fatalf("apply scans=%d pages=%d; want 1 paginated scan", counter.scans, counter.pages)
	}
}

type countingMigrationWrites struct {
	store.Store
	writes int
}

func (s *countingMigrationWrites) Apply(ctx context.Context, obj store.Object, opts store.ApplyOptions) (store.Object, error) {
	s.writes++
	return s.Store.Apply(ctx, obj, opts)
}

func TestMigrationCutoverRejectsFullJournalBeforeWrites(t *testing.T) {
	for _, addresses := range []bool{false, true} {
		t.Run(fmt.Sprintf("addresses=%t", addresses), func(t *testing.T) {
			h := newHarness(t)
			run := importRun(t, h, migrationPlan("one"))
			stored, err := h.srv.getMigrationRun(context.Background(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			// Simulate accumulated retry history close to the object size cap.
			stored.Spec.Cutovers = []internal.MigrationCutover{{StartedAt: stored.Spec.StartedAt, Status: "partial", Items: []internal.MigrationItem{{Kind: "DNSZone", Name: "one", Result: ResultError}}}}
			b, err := json.Marshal(stored)
			if err != nil {
				t.Fatal(err)
			}
			stored.Spec.Cutovers[0].Items[0].Error = strings.Repeat("x", migrationJournalMaxBytes-len(b)-2048)
			if err := h.srv.saveMigrationRun(context.Background(), stored); err != nil {
				t.Fatal(err)
			}
			body := map[string]any{"nameservers": []string{"dns.kudurru.test"}}
			if addresses {
				body = map[string]any{"nameservers": []string{}, "addresses": []string{"192.0.2.53"}}
			}
			var p MigrationCutoverPlan
			h.do(http.MethodPost, "/api/v1/imports/runs/"+run.ID+"/decommission/preview", "admin", body).want(http.StatusOK).json(&p)
			counter := &countingMigrationWrites{Store: h.st}
			h.srv.opts.Store = counter
			response := h.do(http.MethodPost, "/api/v1/imports/runs/"+run.ID+"/decommission", "admin", p).want(http.StatusConflict)
			if !strings.Contains(response.Body.String(), "insufficient space") || counter.writes != 0 {
				t.Fatalf("expected capacity error before any write: %s; writes=%d", response.Body, counter.writes)
			}
			if getMigrationZone(t, h, "one").Spec.SOA.MName != "old.dns.test." {
				t.Fatal("capacity rejection changed zone authority")
			}
		})
	}
}

type rejectingMigrationRecords struct{ store.Store }

func (s rejectingMigrationRecords) Apply(ctx context.Context, obj store.Object, opts store.ApplyOptions) (store.Object, error) {
	if store.KindOf(obj) == "DNSRecordSet" {
		return nil, fmt.Errorf("record writes unavailable")
	}
	return s.Store.Apply(ctx, obj, opts)
}

func TestMigrationAddressFailureLeavesAuthorityAndCanRetry(t *testing.T) {
	h := newHarness(t)
	run := importRun(t, h, migrationPlan("one"))
	preview := func() MigrationCutoverPlan {
		var p MigrationCutoverPlan
		h.do(http.MethodPost, "/api/v1/imports/runs/"+run.ID+"/decommission/preview", "admin", map[string]any{"nameservers": []string{}, "addresses": []string{"192.0.2.53"}}).want(http.StatusOK).json(&p)
		return p
	}
	p := preview()
	h.srv.opts.Store = rejectingMigrationRecords{h.st}
	result := applyCutover(t, h, run.ID, p)
	if len(result.Items) != 2 || result.Items[0].Result != ResultError || result.Items[1].Result != ResultError || getMigrationZone(t, h, "one").Spec.SOA.MName != "old.dns.test." {
		t.Fatalf("failed address write: %+v", result)
	}
	h.srv.opts.Store = h.st
	result = applyCutover(t, h, run.ID, preview())
	if result.Items[0].Result != ResultCreated || result.Items[1].Result != ResultUpdated {
		t.Fatalf("retry failed: %+v", result)
	}
	stale := preview()
	rs := stale.Zones[0].AddressRecordSet
	rs.Labels = map[string]string{"review": "edited"}
	h.seed(store.ManagerAPI, rs)
	h.do(http.MethodPost, "/api/v1/imports/runs/"+run.ID+"/decommission", "admin", stale).want(http.StatusConflict)
}

func TestMigrationCutoverReusesImportedVIPMapping(t *testing.T) {
	h := newHarness(t)
	p := migrationPlan("one")
	rs := recordSetObj("default", "imported-addresses", "one", v1alpha1.Endpoint{DNSName: "ns-c0000235.one.test.", RecordType: "A", Targets: []string{"192.0.2.53"}, RecordTTL: 300})
	k, _ := kindByKind("DNSRecordSet")
	doc, err := objectMap(k, rs)
	if err != nil {
		t.Fatal(err)
	}
	p.Objects = append(p.Objects, doc)
	run := importRun(t, h, p)
	var preview MigrationCutoverPlan
	h.do(http.MethodPost, "/api/v1/imports/runs/"+run.ID+"/decommission/preview", "admin", map[string]any{"nameservers": []string{}, "addresses": []string{"192.0.2.53"}}).want(http.StatusOK).json(&preview)
	if preview.Zones[0].Error != "" {
		t.Fatal(preview.Zones[0].Error)
	}
	result := applyCutover(t, h, run.ID, preview)
	for _, item := range result.Items {
		if item.Result == ResultError {
			t.Fatal(item.Error)
		}
	}
}
