package main

import (
	"encoding/json"
	"net/http"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/store"
)

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

	// A sync writes the host's status again. The operator's earlier review
	// is now one status-only write behind.
	settled()
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
