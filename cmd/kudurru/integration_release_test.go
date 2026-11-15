package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// integrationDeleteAdmin gives a Delete fixture the real API router with an
// admin token and a viewer token of the same user.
func integrationDeleteAdmin(t *testing.T, f *integrationDeleteFixture) (*integrationOperationFixture, http.Handler, string) {
	t.Helper()
	admin := &integrationOperationFixture{s: f.s, d: f.d, path: f.path}
	integrationOperationCaller(t, admin)
	var err error
	if admin.adapter, err = servinghttp.New(servinghttp.Options{Publisher: f.s.publisher, Now: f.s.opts.Now}); err != nil {
		t.Fatal(err)
	}
	viewer, id := tokens.Generate()
	err = f.s.opts.Store.Command(t.Context(), f.s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var user internalv1.LocalUser
		if err := tx.Get(ctx, store.Key{Kind: "LocalUser", Name: "operation-admin"}, &user); err != nil {
			return err
		}
		expires := metav1.NewTime(time.Now().Add(time.Hour))
		_, err := tx.Apply(ctx, &internalv1.APIToken{ObjectMeta: metav1.ObjectMeta{Name: id}, Spec: internalv1.APITokenSpec{SecretHash: tokens.Hash(viewer), Owner: internalv1.TokenOwner{Kind: internalv1.OwnerUser, Name: user.Name, UID: string(user.UID), ID: "local:operation-admin", Provider: "local"}, Scopes: []string{"viewer"}, ExpiresAt: &expires}}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return admin, integrationAdminRouter(t, admin), viewer
}

// integrationSummaryOf reads one summary through the router once the
// publisher has caught up with the store, so it names the stored revision.
func integrationSummaryOf(t *testing.T, router http.Handler, admin *integrationOperationFixture, path string) api.IntegrationSummary {
	t.Helper()
	integrationAdminSettled(t, admin)
	w := integrationAdminCall(t, router, admin, http.MethodGet, path, nil, admin.token)
	var out api.IntegrationSummary
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatal("integration read", w.Code, w.Body.String())
	}
	return out
}

// integrationCLI runs the kudurru CLI against router over TLS.
func integrationCLI(t *testing.T, router http.Handler, token string) func(args ...string) (string, error) {
	t.Helper()
	server := httptest.NewTLSServer(router)
	t.Cleanup(server.Close)
	config := filepath.Join(t.TempDir(), "contexts.yaml")
	return func(args ...string) (string, error) {
		o := newCLIOpts()
		o.getenv = func(string) string { return "" }
		o.httpClient = server.Client()
		root := newRootWith(o)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(append([]string{"--cli-config", config, "--server", server.URL, "--token", token}, args...))
		err := root.ExecuteContext(t.Context())
		return out.String(), err
	}
}

// TestIntegrationAdminTerminatingStateAndReviewedRelease is the API and CLI
// surface of the Delete lifecycle (D367) on the mounted SQLite runtime: get
// and list show the policy and the terminating state with its reason and
// counts; check, preview, sync and act on a terminating instance are
// conflicts with no provider request; the reviewed release command refuses a
// live object, a stale review, a viewer and an anonymous caller, and then
// removes the exact reviewed object without any provider request, keeping
// the provider records and the ledger.
func TestIntegrationAdminTerminatingStateAndReviewedRelease(t *testing.T) {
	f := newIntegrationDeleteFixture(t, nil)
	admin, router, viewer := integrationDeleteAdmin(t, f)
	path := "/integrations/default/publisher"
	live := integrationSummaryOf(t, router, admin, path)
	if live.DeletionPolicy != public.IntegrationDeletionDelete || live.Terminating || live.DeletionRequestedAt != nil || live.Remaining != 0 {
		t.Fatalf("live summary: %+v", live)
	}
	// A live object is not released, whatever the review.
	if w := integrationAdminCall(t, router, admin, http.MethodPost, path+"/release", api.IntegrationReview{UID: live.UID, Revision: live.Revision}, admin.token); w.Code != http.StatusConflict {
		t.Fatal("a live object was released", w.Code)
	}
	if held := integrationStored(t, f.s, f.d); held == nil || held.ResourceVersion != live.Revision || held.DeletionTimestamp != nil {
		t.Fatal("a refused release changed the object")
	}

	// A record of another owner at the owned name blocks the cleanup, so the
	// object stays terminating for the rest of the test.
	f.seed(integrationForeignID(5), "web.example", "TXT", `"operator note"`, "")
	f.requestDelete(t)
	f.awaitStatus(t, ih.PhaseTerminating, ih.ReasonRecordsConflict)
	var held *public.Integration
	awaitServingRuntime(t, f.s, func() bool {
		held = integrationStored(t, f.s, f.d)
		for _, status := range f.s.integrations.host.Statuses() {
			if status.Owner == f.owner {
				return held != nil && status.Revision == held.ResourceVersion && status.Reason == ih.ReasonRecordsConflict && status.Result.Remaining == 1
			}
		}
		return false
	})
	// The job's own status write is a new revision; read the summary once
	// it and the stored object agree.
	var terminating api.IntegrationSummary
	awaitServingRuntime(t, f.s, func() bool {
		terminating = integrationSummaryOf(t, router, admin, path)
		held = integrationStored(t, f.s, f.d)
		return held != nil && terminating.Revision == held.ResourceVersion && terminating.StatusKnown
	})
	if !terminating.Terminating || terminating.DeletionRequestedAt == nil || !terminating.DeletionRequestedAt.Equal(held.DeletionTimestamp.Time) || terminating.Revision != held.ResourceVersion || terminating.UID != string(f.d.UID) {
		t.Fatalf("terminating summary: %+v; stored revision %s, deletion requested at %v", terminating, held.ResourceVersion, held.DeletionTimestamp)
	}
	if !terminating.StatusKnown || terminating.Phase != ih.PhaseTerminating || terminating.Reason != ih.ReasonRecordsConflict || terminating.Remaining != 1 || terminating.Blocked != 1 || terminating.Uncertain != 0 || terminating.DeletionPolicy != public.IntegrationDeletionDelete {
		t.Fatalf("terminating summary status: %+v", terminating)
	}
	w := integrationAdminCall(t, router, admin, http.MethodGet, "/integrations?namespace=default", nil, admin.token)
	var list struct {
		Items []api.IntegrationSummary `json:"items"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.Items) != 1 || !list.Items[0].Terminating || list.Items[0].Reason != ih.ReasonRecordsConflict {
		t.Fatal("list lost the terminating state", w.Code, w.Body.String())
	}
	run := integrationCLI(t, router, admin.token)
	out, err := run("integrations", "list")
	if err != nil || !strings.Contains(out, "POLICY") || !strings.Contains(out, "DELETION REQUESTED") || !strings.Contains(out, public.IntegrationDeletionDelete) || !strings.Contains(out, ih.ReasonRecordsConflict) || !strings.Contains(out, held.DeletionTimestamp.UTC().Format(time.RFC3339)) {
		t.Fatalf("CLI list: %v\n%s", err, out)
	}

	// Every operator command on a terminating instance is a conflict, and
	// none reaches the provider.
	requests, writes, _ := f.traffic()
	review := api.IntegrationReview{UID: terminating.UID, Revision: terminating.Revision}
	for _, command := range []struct {
		path string
		body api.IntegrationReview
	}{
		{"/check", review},
		{"/preview", review},
		{"/sync", api.IntegrationReview{UID: review.UID, Revision: review.Revision, Mode: "publish", PreviewDigest: strings.Repeat("a", 64)}},
		{"/actions/purge", review},
	} {
		if w := integrationAdminCall(t, router, admin, http.MethodPost, path+command.path, command.body, admin.token); w.Code != http.StatusConflict {
			t.Fatalf("%s on a terminating instance: %d %s", command.path, w.Code, w.Body.String())
		}
	}
	// The release needs its permission, a caller, the exact review and
	// nothing else in the body.
	for _, refused := range []struct {
		name   string
		body   api.IntegrationReview
		token  string
		status int
	}{
		{"anonymous", review, "", http.StatusUnauthorized},
		{"viewer", review, viewer, http.StatusForbidden},
		{"stale uid", api.IntegrationReview{UID: "another-uid", Revision: review.Revision}, admin.token, http.StatusConflict},
		{"stale revision", api.IntegrationReview{UID: review.UID, Revision: live.Revision}, admin.token, http.StatusConflict},
		{"sync fields", api.IntegrationReview{UID: review.UID, Revision: review.Revision, Mode: "publish"}, admin.token, http.StatusBadRequest},
	} {
		if w := integrationAdminCall(t, router, admin, http.MethodPost, path+"/release", refused.body, refused.token); w.Code != refused.status {
			t.Fatalf("release (%s): %d, want %d: %s", refused.name, w.Code, refused.status, w.Body.String())
		}
	}
	if current := integrationStored(t, f.s, f.d); current == nil || current.ResourceVersion != review.Revision || !store.HasFinalizer(current, store.IntegrationCleanupFinalizer) {
		t.Fatal("a refused command changed the terminating object")
	}
	if _, err := run("integrations", "release", "publisher"); err == nil {
		t.Fatal("the CLI released without a review")
	}
	if after, afterWrites, _ := f.traffic(); after != requests || afterWrites != writes {
		t.Fatal("a refused command reached the provider", after-requests, afterWrites-writes)
	}

	out, err = run("-o", "json", "integrations", "release", "publisher", "--uid", review.UID, "--revision", review.Revision)
	if err != nil {
		t.Fatalf("CLI release: %v\n%s", err, out)
	}
	var released api.IntegrationReleaseResult
	if err := json.Unmarshal([]byte(out), &released); err != nil || !released.Released || released.LedgerRows != 1 || released.Operations < 1 {
		t.Fatalf("release acknowledgment: %v %s", err, out)
	}
	if integrationStored(t, f.s, f.d) != nil {
		t.Fatal("the released object remains")
	}
	integrationAdminSettled(t, admin)
	if w := integrationAdminCall(t, router, admin, http.MethodGet, path, nil, admin.token); w.Code != http.StatusNotFound {
		t.Fatal("a released object is still served", w.Code)
	}
	awaitServingRuntime(t, f.s, func() bool { return f.phase() == "" })
	time.Sleep(200 * time.Millisecond)
	if after, afterWrites, deleted := f.traffic(); after != requests || afterWrites != writes || len(deleted) != 0 {
		t.Fatal("the release reached the provider", after-requests, afterWrites-writes, deleted)
	}
	// What the instance published and its ledger stay, as under Retain.
	f.peer.mu.Lock()
	for _, id := range f.published {
		if _, ok := f.peer.rows[id]; !ok {
			t.Error("the release removed a published provider record", id)
		}
	}
	f.peer.mu.Unlock()
	if rows := integrationRows(t, f.s, kinds.KindIntegrationLedger); len(rows) != 1 {
		t.Fatal("the release did not retain the ledger", len(rows))
	}
	entries := integrationAudit(t, f.s, f.d)
	var actions []string
	reason := ""
	for _, entry := range entries {
		actions = append(actions, entry.Action)
		if entry.Action == "integration.release" {
			reason = entry.Reason
		}
	}
	if slices.Contains(actions, "integration.cleanup") || !strings.HasPrefix(reason, "released without cleanup by the reviewed release command: deletionPolicy is Delete; removed "+store.IntegrationCleanupFinalizer) || !strings.Contains(reason, "1 ledger rows") || !strings.Contains(reason, string(f.d.UID)) {
		t.Fatalf("release audit: %v %q", actions, reason)
	}
}
