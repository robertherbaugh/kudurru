package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	internal "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api/apitest"
	"github.com/robertherbaugh/kudurru/internal/importers"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type blockingMigrationStore struct {
	store.Store
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingMigrationStore) Apply(ctx context.Context, obj store.Object, opts store.ApplyOptions) (store.Object, error) {
	if store.KeyOf(obj).Kind == "DNSZone" {
		s.once.Do(func() { close(s.entered) })
		select {
		case <-s.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.Store.Apply(ctx, obj, opts)
}
func startTestMigration(t *testing.T, h *harness, p importers.Plan) MigrationRun {
	t.Helper()
	var run MigrationRun
	response := h.do(http.MethodPost, "/api/v1/imports/start", "admin", p).want(http.StatusAccepted)
	response.json(&run)
	if response.Header().Get("Location") != "/api/v1/imports/runs/"+run.ID || run.Data.Status != "running" {
		t.Fatalf("start: %+v", run)
	}
	return run
}
func awaitTestMigration(t *testing.T, h *harness, id string) MigrationRun {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var run MigrationRun
		h.do(http.MethodGet, "/api/v1/imports/runs/"+id, "admin", nil).want(http.StatusOK).json(&run)
		if run.Data.Status != "running" {
			return run
		}
		time.Sleep(time.Millisecond * 5)
	}
	t.Fatal("migration did not finish")
	return MigrationRun{}
}
func TestMigrationBackgroundSurvivesDisconnectAndReportsProgress(t *testing.T) {
	h := newHarness(t)
	blocked := &blockingMigrationStore{Store: h.st, entered: make(chan struct{}), release: make(chan struct{})}
	h.srv.opts.Store = blocked
	ctx, cancel := context.WithCancel(context.Background())
	req := h.req(http.MethodPost, "/api/v1/imports/start", "admin", migrationPlan("one", "two")).WithContext(ctx)
	var run MigrationRun
	h.serve(req).want(http.StatusAccepted).json(&run)
	cancel()
	select {
	case <-blocked.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("worker never started")
	}
	var progress MigrationRun
	h.do(http.MethodGet, "/api/v1/imports/runs/"+run.ID, "admin", nil).want(http.StatusOK).json(&progress)
	if progress.Data.Items[0].Result != "running" || progress.Data.Items[1].Result != "pending" {
		t.Fatalf("progress: %+v", progress.Data.Items)
	}
	close(blocked.release)
	done := awaitTestMigration(t, h, run.ID)
	if done.Data.Status != "completed" || done.Data.Actor != "User:admin" || done.Data.Items[1].Result != ResultCreated {
		t.Fatalf("completion: %+v", done)
	}
	// The start response is a snapshot, never mutated by worker updates.
	if run.Data.Items[0].Result != "pending" {
		t.Fatal("start response mutated")
	}
}
func TestMigrationBackgroundPartialSuccessAndSelection(t *testing.T) {
	h := newHarness(t)
	p := migrationPlan("good", "bad", "skip")
	p.Objects[1]["spec"].(map[string]any)["type"] = "invalid"
	p.Settings = map[string]any{"blocking": map[string]any{"enabled": false}, "upstreams": map[string]any{}}
	p.Selection = &importers.Selection{Objects: []string{"DNSZone/good", "DNSZone/bad"}, Settings: []string{"blocking"}}
	run := awaitTestMigration(t, h, startTestMigration(t, h, p).ID)
	if run.Data.Status != "partial" || len(run.Data.Zones) != 1 || len(run.Data.Items) != 5 {
		t.Fatalf("report: %+v", run)
	}
	want := []string{ResultCreated, ResultError, "skipped", "not_applied", "skipped"}
	for i, w := range want {
		if run.Data.Items[i].Result != w {
			t.Fatalf("item %d: %+v", i, run.Data.Items[i])
		}
	}
	if run.Data.Items[1].Error == "" || run.Data.Items[1].ErrorKey == "" || run.Data.Items[0].ResourceVersion == "" {
		t.Fatal("missing item details")
	}
	if !h.srv.config().Blocking.Enabled {
		t.Fatal("settings applied despite object failure")
	}
	h.do(http.MethodGet, "/api/v1/objects/dnszones/skip", "admin", nil).want(http.StatusNotFound)
	if getMigrationZone(t, h, "good").Annotations[migrationRunAnnotation] != run.ID {
		t.Fatal("successful object was not retained")
	}
}
func TestMigrationBackgroundSelectedSettingsOnly(t *testing.T) {
	h := newHarness(t)
	p := migrationPlan("skip")
	p.Settings = map[string]any{"blocking": map[string]any{"enabled": false}, "unknownSetting": true}
	p.Selection = &importers.Selection{Objects: []string{}, Settings: []string{"blocking"}}
	run := awaitTestMigration(t, h, startTestMigration(t, h, p).ID)
	if run.Data.Status != "completed" || h.srv.config().Blocking.Enabled || len(run.Data.Zones) != 0 || run.Data.Items[0].Result != "skipped" || run.Data.Items[1].Result != ResultUpdated || run.Data.Items[2].Result != "skipped" {
		t.Fatalf("selected settings: %+v", run)
	}
}
func TestMigrationBackgroundRejectsInvalidSelectionBeforeWrites(t *testing.T) {
	for _, selection := range []*importers.Selection{
		{Objects: []string{}, Settings: []string{}},
		{Objects: []string{"DNSZone/missing"}, Settings: []string{}},
		{Objects: []string{"DNSZone/one", "DNSZone/one"}, Settings: []string{}},
		{Objects: []string{}, Settings: []string{"missing"}},
		{Objects: []string{}, Settings: []string{"blocking", "blocking"}},
	} {
		h := newHarness(t)
		p := migrationPlan("one")
		p.Selection = selection
		p.Settings = map[string]any{"blocking": map[string]any{"enabled": false}}
		h.do(http.MethodPost, "/api/v1/imports/start", "admin", p).want(http.StatusBadRequest)
		runs, err := h.st.List(context.Background(), internal.KindMigrationRun, store.ListOptions{})
		if err != nil || len(runs.Items) != 0 {
			t.Fatal("invalid selection created journal")
		}
	}
	h := newHarness(t)
	h.do(http.MethodPost, "/api/v1/imports/start", "viewer", migrationPlan("one")).want(http.StatusForbidden)
	h.do(http.MethodPost, "/api/v1/imports/start", "admin", migrationPlan()).want(http.StatusBadRequest)
}
func TestMigrationBackgroundConcurrencyAndShutdown(t *testing.T) {
	h := newHarness(t)
	blocked := &blockingMigrationStore{Store: h.st, entered: make(chan struct{}), release: make(chan struct{})}
	h.srv.opts.Store = blocked
	a := startTestMigration(t, h, migrationPlan("one", "two"))
	b := startTestMigration(t, h, migrationPlan("three"))
	h.do(http.MethodPost, "/api/v1/imports/start", "admin", migrationPlan("four")).want(http.StatusServiceUnavailable)
	<-blocked.entered
	h.srv.Close()
	for _, id := range []string{a.ID, b.ID} {
		run := awaitTestMigration(t, h, id)
		if run.Data.Status != "partial" || run.Data.Failure == "" || run.Data.FinishedAt == nil {
			t.Fatalf("shutdown: %+v", run)
		}
	}
	h.do(http.MethodPost, "/api/v1/imports/start", "admin", migrationPlan("five")).want(http.StatusServiceUnavailable)
}
func TestMigrationBackgroundInterruptedView(t *testing.T) {
	h := newHarness(t)
	run := &internal.MigrationRun{ObjectMeta: metav1.ObjectMeta{Name: "interrupted"}, Spec: internal.MigrationRunSpec{Status: "running", Deadline: &metav1.Time{Time: time.Now().Add(-time.Minute)}, Items: []internal.MigrationItem{{Kind: "DNSZone", Name: "one", Result: "running"}, {Kind: "DNSZone", Name: "two", Result: ResultCreated}}}}
	if err := h.srv.saveMigrationRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	var view MigrationRun
	h.do(http.MethodGet, "/api/v1/imports/runs/interrupted", "admin", nil).want(http.StatusOK).json(&view)
	if view.Data.Status != "interrupted" || view.Data.Items[0].Result != "unconfirmed" || view.Data.Items[1].Result != ResultCreated || view.Data.Failure == "" {
		t.Fatalf("interruption: %+v", view)
	}
	if run.Spec.Items[0].Result != "running" {
		t.Fatal("view mutated stored state")
	}
}
func TestMigrationErrorFitsJournal(t *testing.T) {
	for _, value := range []string{strings.Repeat("界", 1000), strings.Repeat("\x00", 1000), strings.Repeat("<", 1000), "short"} {
		truncated := truncateMigrationError(value)
		encoded, _ := json.Marshal(truncated)
		if len(encoded) > 1024 || !utf8.ValidString(truncated) {
			t.Fatalf("invalid bounded error: %d bytes", len(encoded))
		}
	}
}

func TestMigrationBackgroundKeepsObjectAuthorization(t *testing.T) {
	permissions, err := rbac.LoadPermissions()
	if err != nil {
		t.Fatal(err)
	}
	permissions.Roles["settings-writer"] = []string{"settings.write"}
	authorizer, err := rbac.NewBindingAuthorizer(permissions, append(apitest.StandardBindings(), apitest.Grant("settings-writer", "settings-writer")))
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(o *Options) { o.Authorizer = authorizer })
	var run MigrationRun
	h.do(http.MethodPost, "/api/v1/imports/start", "settings-writer", migrationPlan("one")).want(http.StatusAccepted).json(&run)
	done := awaitTestMigration(t, h, run.ID)
	if done.Data.Status != "partial" || done.Data.Items[0].ErrorKey != KeyForbidden {
		t.Fatalf("object authorization bypassed: %+v", done)
	}
}
func TestMigrationBackgroundCapacityCheckedBeforeWrites(t *testing.T) {
	h := newHarness(t)
	names := []string{}
	for i := 0; i < 600; i++ {
		names = append(names, fmt.Sprintf("zone-%d", i))
	}
	h.do(http.MethodPost, "/api/v1/imports/start", "admin", migrationPlan(names...)).want(http.StatusBadRequest)
	zones, err := h.st.List(context.Background(), "DNSZone", store.ListOptions{})
	if err != nil || len(zones.Items) > 0 {
		t.Fatal("capacity rejection wrote objects")
	}
}

type panickingMigrationStore struct{ store.Store }

func (s panickingMigrationStore) Apply(ctx context.Context, obj store.Object, opts store.ApplyOptions) (store.Object, error) {
	if store.KeyOf(obj).Kind == "DNSZone" {
		panic("simulated writer panic")
	}
	return s.Store.Apply(ctx, obj, opts)
}
func TestMigrationBackgroundUnexpectedStopIsUnconfirmed(t *testing.T) {
	h := newHarness(t)
	h.srv.opts.Store = panickingMigrationStore{Store: h.st}
	run := awaitTestMigration(t, h, startTestMigration(t, h, migrationPlan("one", "two")).ID)
	if run.Data.Status != "partial" || run.Data.Failure == "" || run.Data.Items[0].Result != "unconfirmed" || run.Data.Items[1].Result != "not_applied" {
		t.Fatalf("unexpected stop: %+v", run)
	}
}
