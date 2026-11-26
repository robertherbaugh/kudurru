package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func TestIntegrationAdminReportsHostCaptureFailure(t *testing.T) {
	f := &integrationOperationFixture{}
	f.s, f.path = integrationFixture(t, nil, func(string) (integration.Integration, error) {
		return &integrationProbe{observe: func(context.Context, ih.OwnerHost) (ih.SourceSnapshot, error) {
			return ih.SourceSnapshot{Complete: true}, nil
		}}, nil
	})
	integrationOperationCaller(t, f)
	options, err := f.s.integrations.hostOptions()
	if err != nil {
		t.Fatal(err)
	}
	var fail atomic.Bool
	capture := options.Capture
	options.Capture = func(ctx context.Context) (ih.Input, error) {
		if fail.Load() {
			return ih.Input{}, context.DeadlineExceeded
		}
		return capture(ctx)
	}
	f.s.integrations.host, err = ih.New(options)
	if err != nil {
		t.Fatal(err)
	}
	integrationOperationStart(t, f, integrationDefinition("manual", f.path))
	router := integrationAdminRouter(t, f)
	fail.Store(true)
	f.s.integrations.host.Wake()
	awaitServingRuntime(t, f.s, func() bool {
		_, reason := f.s.integrations.host.StatusSnapshot()
		return reason == "CaptureDeadlineExceeded"
	})
	read := func(path string, value any) {
		t.Helper()
		w := integrationAdminCall(t, router, f, http.MethodGet, path, nil, f.token)
		if w.Code != http.StatusOK {
			t.Fatalf("capture failure status: HTTP %d", w.Code)
		}
		if err := json.Unmarshal(w.Body.Bytes(), value); err != nil {
			t.Fatal(err)
		}
	}
	assertFailure := func(summary api.IntegrationSummary) {
		t.Helper()
		if summary.Reason != "CaptureDeadlineExceeded" || summary.Phase != "Unknown" || summary.StatusKnown || summary.Leader || summary.UID != string(f.d.UID) {
			t.Fatalf("unexpected capture failure summary: %+v", summary)
		}
	}
	var summary api.IntegrationSummary
	read("/integrations/default/manual", &summary)
	assertFailure(summary)
	var list struct {
		Items []api.IntegrationSummary `json:"items"`
	}
	read("/integrations?namespace=default", &list)
	if len(list.Items) != 1 {
		t.Fatalf("unexpected integration count: %d", len(list.Items))
	}
	assertFailure(list.Items[0])
}

// TestIntegrationAdminStatusOnlyWriteKeepsReviewCurrent proves D384 through
// the mounted admin API on the complete SQLite runtime: the host's own status
// write moves the object's revision, yet GET still knows the status of the
// running instance, and a command reviewed at the revision before that write
// still runs. Any other change still makes the earlier review stale.
func TestIntegrationAdminStatusOnlyWriteKeepsReviewCurrent(t *testing.T) {
	f := newIntegrationOperationFixture(t)
	router := integrationAdminRouter(t, f)
	path := "/integrations/default/manual"
	// A read while a publication is being prepared may answer 503; the
	// waits below only look at answered reads.
	read := func() api.IntegrationSummary {
		t.Helper()
		w := integrationAdminCall(t, router, f, http.MethodGet, path, nil, f.token)
		var out api.IntegrationSummary
		if w.Code != http.StatusOK {
			return out
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	settled := func() {
		t.Helper()
		awaitServingRuntime(t, f.s, func() bool {
			l, err := f.s.publisher.CaptureHTTP(t.Context())
			if err != nil {
				return false
			}
			defer l.Release()
			base, current := l.Bundle().Identity().Base, f.s.opts.Store.Authority().Installed
			return base.Term.Same(current.Term) && base.Revision == current.Revision
		})
	}
	stored := func() string {
		var current public.Integration
		if err := f.s.opts.Store.Get(t.Context(), store.KeyOf(f.d), &current); err != nil {
			t.Fatal(err)
		}
		return current.ResourceVersion
	}

	// The fixture's start already wrote a Ready status, which moved the
	// revision: the healthy running instance must still report a known status.
	var reviewed api.IntegrationSummary
	awaitServingRuntime(t, f.s, func() bool {
		reviewed = read()
		return reviewed.Revision != "" && reviewed.Revision == stored() && reviewed.StatusKnown
	})
	if reviewed.Phase == "Unknown" || reviewed.Reason == "NoMatchingObservation" {
		t.Fatalf("running instance reported no status: %+v", reviewed)
	}

	// The stored sync time has whole-second precision. Advance the clock so
	// this sync changes the status and leaves the review one revision behind.
	settled()
	f.offset.Add(int64(time.Second))
	w := integrationAdminCall(t, router, f, http.MethodPost, path+"/sync", api.IntegrationReview{UID: reviewed.UID, Revision: reviewed.Revision, Mode: "observe"}, f.token)
	if w.Code != http.StatusOK {
		t.Fatalf("observer sync: %d %s", w.Code, w.Body.String())
	}
	var after api.IntegrationSummary
	awaitServingRuntime(t, f.s, func() bool {
		after = read()
		return after.Revision != reviewed.Revision && after.Revision == stored() && after.StatusKnown
	})
	settled()
	before := f.calls.Load()
	w = integrationAdminCall(t, router, f, http.MethodPost, path+"/check", api.IntegrationReview{UID: reviewed.UID, Revision: reviewed.Revision}, f.token)
	if w.Code != http.StatusOK {
		t.Fatalf("check reviewed one status write before %s: %d %s", after.Revision, w.Code, w.Body.String())
	}
	var result api.IntegrationExecutionResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Completed || !result.Reachable || f.calls.Load() != before+1 {
		t.Fatal("status-only successor lost the check's outcome")
	}

	// A label change is not a status write: both earlier reviews are stale
	// and reach no provider.
	settled()
	var current public.Integration
	if err := f.s.opts.Store.Get(t.Context(), store.KeyOf(f.d), &current); err != nil {
		t.Fatal(err)
	}
	current.Labels = map[string]string{"team": "dns"}
	current.Status = public.IntegrationStatus{}
	labeled := integrationApply(t, f.s, &current)
	awaitServingRuntime(t, f.s, func() bool {
		s := read()
		return s.Revision == labeled.ResourceVersion && s.StatusKnown
	})
	settled()
	before = f.calls.Load()
	for _, stale := range []string{reviewed.Revision, after.Revision} {
		w = integrationAdminCall(t, router, f, http.MethodPost, path+"/check", api.IntegrationReview{UID: reviewed.UID, Revision: stale}, f.token)
		if w.Code != http.StatusConflict || f.calls.Load() != before {
			t.Fatalf("check reviewed at %s before a label change: %d", stale, w.Code)
		}
	}
}
