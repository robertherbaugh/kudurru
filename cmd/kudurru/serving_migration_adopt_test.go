package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	internal "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/providerhost"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// adoptionHTTP sends exactly one request through the production route gate and
// current authentication. A refused mutation is never retried by this helper.
func adoptionHTTP(t *testing.T, f *servingCommandFixture, server *httptest.Server, method, path string, body any, want ...int) []byte {
	t.Helper()
	adoptionWait(t, f)
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r, err := http.NewRequestWithContext(t.Context(), method, server.URL+"/api/v1"+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+f.token)
	r.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	out, err := io.ReadAll(response.Body)
	if err != nil || !slices.Contains(want, response.StatusCode) {
		t.Fatalf("%s %s: status=%d want=%v err=%v body=%s", method, path, response.StatusCode, want, err, out)
	}
	return out
}

func adoptionWait(t *testing.T, f *servingCommandFixture) {
	t.Helper()
	var stable replication.Generation
	var since time.Time
	settled := func() bool {
		image, err := f.s.opts.Store.ReadGeneration(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		status := f.s.publisher.Status()
		current := image.Generation()
		if !status.ProtectedReady || !status.Installed.Same(current) || !status.Served.Base.Same(current) {
			since = time.Time{}
			return false
		}
		// Complete publication can legitimately include desired-only primary
		// placeholders. Wait for every seeded zone's actual admission and
		// acknowledgment so its scheduler cannot invalidate the test request.
		lease, err := f.s.publisher.CaptureHTTP(t.Context())
		if err != nil {
			since = time.Time{}
			return false
		}
		defer lease.Release()
		component, ok := lease.Bundle().Component("runtime")
		owner, typed := component.(*servingbuild.RuntimeOwner)
		if !ok || !typed || !owner.Primary().Base().Same(current) {
			since = time.Time{}
			return false
		}
		acknowledged := map[string]bool{}
		for _, z := range f.s.primary.driver.Status().Zones {
			acknowledged[z.ObjectName] = z.Installed && z.Acknowledged && z.Published
		}
		for _, raw := range image.Payload.Objects {
			var header metav1.TypeMeta
			if err := json.Unmarshal(raw, &header); err != nil {
				t.Fatal(err)
			}
			if header.Kind != "DNSZone" {
				continue
			}
			var z public.DNSZone
			if err := json.Unmarshal(raw, &z); err != nil {
				t.Fatal(err)
			}
			if z.Spec.Type == public.ZoneTypePrimary && z.DeletionTimestamp == nil && (!acknowledged[z.Name] || !owner.Primary().Admitted(z.Spec.ZoneName, time.Now())) {
				since = time.Time{}
				return false
			}
		}
		if since.IsZero() || !stable.Same(current) {
			stable, since = current, time.Now()
			return false
		}
		return time.Since(since) >= 100*time.Millisecond
	}
	// Many primary zones produce several complete publications under -race.
	// Wait before issuing the single HTTP request, without extending any
	// credential, lease, writer or operation deadline and without replaying it.
	until := time.Now().Add(2 * time.Minute)
	for !settled() {
		if !time.Now().Before(until) {
			t.Fatal("adoption fixture did not reach a stable complete publication")
		}
		if err := t.Context().Err(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func adoptionJSON[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func adoptionZone(name string) *public.DNSZone {
	sum := sha256.Sum256([]byte(name + ".test"))
	return &public.DNSZone{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "DNSZone"}, ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("technitium-%s-test-%x", name, sum[:4]), Annotations: map[string]string{"retain": "annotation"}}, Spec: public.DNSZoneSpec{ZoneName: name + ".test.", Type: public.ZoneTypePrimary, Nameservers: []string{"old.test."}, SOA: &public.SOASpec{MName: "old.test.", RName: "hostmaster.test."}}}
}

func adoptionSeed(t *testing.T, f *servingCommandFixture, names ...string) {
	t.Helper()
	err := f.s.opts.Store.Command(t.Context(), f.s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		for _, name := range names {
			if _, err := tx.Apply(ctx, adoptionZone(name), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func adoptionSelection(t *testing.T, f *servingCommandFixture, server *httptest.Server) []api.MigrationAuthorityChange {
	t.Helper()
	return adoptionJSON[api.List[api.MigrationAuthorityChange]](t, adoptionHTTP(t, f, server, http.MethodGet, "/imports/existing", nil, http.StatusOK)).Items
}

func adoptionUnchangedImage(t *testing.T, st *sqlite.Store, before *replication.Image) {
	t.Helper()
	after, err := st.ReadGeneration(t.Context())
	if err != nil || !before.Generation().Same(after.Generation()) {
		t.Fatal("refused adoption changed durable image", err)
	}
}

func TestServingAdoptionActualHTTPPreservesDataRestartAndCutover(t *testing.T) {
	f := newServingCommandFixture(t)
	s := migrationServingAPI(t, f, nil)
	server := httptest.NewServer(servingMutationRoutes(s))
	defer server.Close()
	// Create the historical import using the public API, as an older converter
	// would have done before MigrationRun journals existed.
	z := adoptionZone("legacy")
	adoptionHTTP(t, f, server, http.MethodPost, "/apply", []any{z}, http.StatusOK)
	rs := &public.DNSRecordSet{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "DNSRecordSet"}, ObjectMeta: metav1.ObjectMeta{Name: "legacy-records", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: z.Name}, Endpoints: []public.Endpoint{{DNSName: "web.legacy.test.", RecordType: "A", Targets: []string{"192.0.2.10"}, RecordTTL: 60}}}}
	adoptionHTTP(t, f, server, http.MethodPost, "/apply", []any{rs}, http.StatusOK)
	var before public.DNSZone
	if err := f.s.opts.Store.Get(t.Context(), store.KeyOf(z), &before); err != nil {
		t.Fatal(err)
	}
	if err := f.s.opts.Store.Get(t.Context(), store.KeyOf(rs), rs); err != nil {
		t.Fatal(err)
	}
	reviewed := adoptionSelection(t, f, server)
	if len(reviewed) != 1 || reviewed[0].UID != string(before.UID) || reviewed[0].ResourceVersion != before.ResourceVersion {
		t.Fatal("listing lost reviewed identity", reviewed)
	}
	result := adoptionJSON[api.MigrationResult](t, adoptionHTTP(t, f, server, http.MethodPost, "/imports/adopt", map[string]any{"zones": reviewed}, http.StatusOK))
	if len(result.Items) != 1 || result.Items[0].Result != api.ResultUpdated || result.JournalError != "" {
		t.Fatal("adoption did not finish atomically", result)
	}
	var adopted public.DNSZone
	if err := f.s.opts.Store.Get(t.Context(), store.KeyOf(z), &adopted); err != nil || adopted.UID != before.UID || !reflect.DeepEqual(adopted.Spec, before.Spec) || adopted.Annotations["retain"] != "annotation" || adopted.Annotations["kudurru.io/migration-run"] != result.RunID || store.ManagedBy(&adopted) != store.ManagerAPI {
		t.Fatal("adoption changed authority or provenance", err, adopted)
	}
	if len(adoptionSelection(t, f, server)) != 0 {
		t.Fatal("adopted zone still a candidate")
	}
	path := "/imports/runs/" + result.RunID
	journal := adoptionJSON[api.MigrationRun](t, adoptionHTTP(t, f, server, http.MethodGet, path, nil, http.StatusOK))
	if journal.Data.Source != "existing" || journal.Data.Status != "completed" || journal.Data.Zones[0].UID != string(before.UID) || journal.Data.Items[0].ResourceVersion != adopted.ResourceVersion {
		t.Fatal("lost completed history", journal)
	}
	image, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Install the full image, restart the store, and promote it. The restarted
	// current runtime must still read this run and execute its reviewed cutover.
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "adopted-restart.db")
	open := func() *sqlite.Store {
		st, err := sqlite.Open(dbPath, sqlite.Options{Scheme: scheme, Validator: f.s.opts.Validator, InternalKinds: privateStoreKinds(), InitialMode: replication.Follower, Identity: "adoption-restart", ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	restarted := open()
	if err := restarted.Enroll(t.Context(), image.Term); err != nil {
		t.Fatal(err)
	}
	validated, err := restarted.ValidateImage(t.Context(), *image)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := restarted.InstallReplica(t.Context(), validated, replication.InstallOptions{ExpectedTerm: image.Term})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Close(); err != nil {
		t.Fatal(err)
	}
	restarted = open()
	t.Cleanup(func() { _ = restarted.Close() })
	if _, err := restarted.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: image.Term, ExpectedInstalled: installed, RequestID: "adoption-promotion"}); err != nil {
		t.Fatal(err)
	}
	opts := f.s.opts
	opts.Store = restarted
	runtime, err := newServingRuntime(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	runServingFixture(t, runtime)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- restarted.Elector("adoption-restarted").Run(ctx, func(ctx context.Context) {
			if err := runtime.RunWriter(ctx); err != nil && ctx.Err() == nil {
				t.Error(err)
			}
		}, nil)
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	})
	awaitServingRuntime(t, runtime, func() bool { return runtime.primary.driver.Status().Ready && runtime.publisher.Status().ProtectedReady })
	httpAuth, err := servinghttp.New(servinghttp.Options{Publisher: runtime.publisher, Now: runtime.opts.Now})
	if err != nil {
		t.Fatal(err)
	}
	restartedFixture := &servingCommandFixture{s: runtime, http: httpAuth, token: f.token}
	restartedAPI := migrationServingAPI(t, restartedFixture, nil)
	restartedHTTP := httptest.NewServer(servingMutationRoutes(restartedAPI))
	defer restartedHTTP.Close()
	reloaded := adoptionJSON[api.MigrationRun](t, adoptionHTTP(t, restartedFixture, restartedHTTP, http.MethodGet, path, nil, http.StatusOK))
	if !reflect.DeepEqual(journal, reloaded) {
		t.Fatal("restart lost history", journal, reloaded)
	}
	preview := adoptionJSON[api.MigrationCutoverPlan](t, adoptionHTTP(t, restartedFixture, restartedHTTP, http.MethodPost, path+"/decommission/preview", map[string]any{"nameservers": []string{"dns.kudurru.test"}}, http.StatusOK))
	if len(preview.Zones) != 1 || preview.Zones[0].Error != "" {
		t.Fatal("adopted zone not available for cutover", preview)
	}
	out := adoptionJSON[api.MigrationResult](t, adoptionHTTP(t, restartedFixture, restartedHTTP, http.MethodPost, path+"/decommission", preview, http.StatusOK))
	if len(out.Items) != 1 || out.Items[0].Result != api.ResultUpdated || out.JournalError != "" {
		t.Fatal("restarted adoption could not cut over", out)
	}
	var after public.DNSZone
	if err := restarted.Get(t.Context(), store.KeyOf(z), &after); err != nil || after.Spec.SOA.MName != "dns.kudurru.test." || !reflect.DeepEqual(after.Spec.Nameservers, []string{"dns.kudurru.test."}) || after.UID != before.UID {
		t.Fatal("cutover lost adopted identity", err, after)
	}
	var records public.DNSRecordSet
	if err := restarted.Get(t.Context(), store.KeyOf(rs), &records); err != nil || !reflect.DeepEqual(records, *rs) {
		t.Fatal("adoption/restart/cutover changed imported records", err, records, rs)
	}
}

func TestServingAdoptionActualHTTPLargeSelectionAndConcurrentAdoption(t *testing.T) {
	f := newServingCommandFixture(t)
	names := make([]string, 16)
	for i := range names {
		names[i] = fmt.Sprintf("previous%d", i)
	}
	adoptionSeed(t, f, names...)
	s := migrationServingAPI(t, f, nil)
	server := httptest.NewServer(servingMutationRoutes(s))
	defer server.Close()
	reviewed := adoptionSelection(t, f, server)
	result := adoptionJSON[api.MigrationResult](t, adoptionHTTP(t, f, server, http.MethodPost, "/imports/adopt", map[string]any{"zones": reviewed}, http.StatusOK))
	if len(result.Items) != 16 {
		t.Fatal("large selection lost outcomes", result)
	}
	adoptionHTTP(t, f, server, http.MethodPost, "/imports/adopt", map[string]any{"zones": reviewed}, http.StatusConflict, http.StatusServiceUnavailable)
	// Native primary admission can advance its private runtime rows after the
	// successful annotation commit. Check the adoption's exact public versions
	// and journal outcome without attributing that background work to a refusal.
	for _, item := range result.Items {
		var z public.DNSZone
		if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "DNSZone", Name: item.Name}, &z); err != nil || z.ResourceVersion != item.ResourceVersion || z.Annotations["kudurru.io/migration-run"] != result.RunID {
			t.Fatal("competing adoption changed an adopted zone", err, item)
		}
	}
	runs, err := f.s.opts.Store.List(t.Context(), internal.KindMigrationRun, store.ListOptions{})
	if err != nil || len(runs.Items) != 1 || runs.Items[0].GetName() != result.RunID {
		t.Fatal("competing adoption created a second journal", err)
	}
}

func TestServingAdoptionActualHTTPFinalExpiryRollsBackAllZones(t *testing.T) {
	f := newServingCommandFixture(t)
	adoptionSeed(t, f, "first", "last")
	migrationShortCredential(t, f)
	var attempts atomic.Int32
	command := func(ctx context.Context, p *api.MigrationProgram) (api.MigrationNext, error) {
		next, err := f.s.MigrationCommand(ctx, p)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context) error {
			attempts.Add(1)
			var calls atomic.Int32
			f.s.opts.Now = func() time.Time {
				if calls.Add(1) <= 3 {
					return f.now
				}
				return f.expires
			}
			defer func() { f.s.opts.Now = func() time.Time { return f.now } }()
			return next(ctx)
		}, nil
	}
	s := migrationServingAPI(t, f, command)
	server := httptest.NewServer(servingMutationRoutes(s))
	defer server.Close()
	reviewed := adoptionSelection(t, f, server)
	adoptionWait(t, f)
	before, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	adoptionHTTP(t, f, server, http.MethodPost, "/imports/adopt", map[string]any{"zones": reviewed}, http.StatusUnauthorized)
	if attempts.Load() != 1 {
		t.Fatal("refused work was retried", attempts.Load())
	}
	adoptionUnchangedImage(t, f.s.opts.Store, before)
}

func TestServingAdoptionActualHTTPOriginalWriterProofAndLease(t *testing.T) {
	for _, mode := range []string{"stale-lease", "revoked-proof", "stopped-writer"} {
		t.Run(mode, func(t *testing.T) {
			f := newServingCommandFixture(t)
			adoptionSeed(t, f, "first", "last")
			var attempts atomic.Int32
			var expected *replication.Image
			command := func(ctx context.Context, p *api.MigrationProgram) (api.MigrationNext, error) {
				next, err := f.s.MigrationCommand(ctx, p)
				if err != nil {
					return nil, err
				}
				return func(ctx context.Context) error {
					attempts.Add(1)
					switch mode {
					case "stopped-writer":
						f.stop()
					case "stale-lease":
						z := adoptionZone("first")
						if err := f.s.opts.Store.Get(ctx, store.KeyOf(z), z); err != nil {
							return err
						}
						z.Labels = map[string]string{"concurrent": "edit"}
						if _, err := f.s.opts.Store.Apply(ctx, z, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
							return err
						}
					case "revoked-proof":
						id, _ := tokens.Parse(f.token)
						if err := f.s.opts.Store.Delete(ctx, store.Key{Kind: "APIToken", Name: id}, store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
							return err
						}
					}
					expected, err = f.s.opts.Store.ReadGeneration(ctx)
					if err != nil {
						return err
					}
					err = next(ctx)
					if err == nil || next(ctx) == nil {
						t.Error("refused program accepted/replayed against a successor")
					}
					return err
				}, nil
			}
			s := migrationServingAPI(t, f, command)
			server := httptest.NewServer(servingMutationRoutes(s))
			defer server.Close()
			reviewed := adoptionSelection(t, f, server)
			adoptionHTTP(t, f, server, http.MethodPost, "/imports/adopt", map[string]any{"zones": reviewed}, http.StatusServiceUnavailable)
			if attempts.Load() != 1 || expected == nil {
				t.Fatal("refused work retried or not reached", attempts.Load())
			}
			adoptionUnchangedImage(t, f.s.opts.Store, expected)
		})
	}
}

func TestServingAdoptionActualHTTPRequiresSettingsAndEveryZone(t *testing.T) {
	for _, missing := range []string{"settings", "last-zone"} {
		t.Run(missing, func(t *testing.T) {
			f := newServingCommandFixture(t)
			adoptionSeed(t, f, "first", "last")
			s := migrationServingAPI(t, f, nil)
			server := httptest.NewServer(servingMutationRoutes(s))
			defer server.Close()
			reviewed := adoptionSelection(t, f, server)
			input, err := loadProviderStatic(t.Context(), providerStaticOptions{Config: f.s.opts.Config, Keys: f.s.opts.AuthKeys, Namespace: "default"})
			if err != nil {
				t.Fatal(err)
			}
			// Add a narrow test role to the immutable host permissions. The
			// production authentication/authorization chain still resolves the
			// original token, checks its complete restriction and pins this host.
			input.Auth.Permissions.Roles["settings-writer"] = []string{"settings.write"}
			prepared, err := providerhost.PrepareStatic(t.Context(), input)
			if err != nil {
				input.Release()
				t.Fatal(err)
			}
			defer prepared.Release()
			if err := f.s.host.ReplaceStatic(t.Context(), prepared); err != nil {
				t.Fatal(err)
			}
			id, _ := tokens.Parse(f.token)
			var token internal.APIToken
			if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "APIToken", Name: id}, &token); err != nil {
				t.Fatal(err)
			}
			token.Spec.Scopes = []string{"zone-admin@zone:" + reviewed[0].Name}
			if missing == "last-zone" {
				token.Spec.Scopes = append(token.Spec.Scopes, "settings-writer")
			}
			if _, err := f.s.opts.Store.Apply(t.Context(), &token, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
				t.Fatal(err)
			}
			f.s.publisher.Wake()
			adoptionWait(t, f)
			before, err := f.s.opts.Store.ReadGeneration(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			adoptionHTTP(t, f, server, http.MethodPost, "/imports/adopt", map[string]any{"zones": reviewed}, http.StatusForbidden)
			adoptionUnchangedImage(t, f.s.opts.Store, before)
		})
	}
}

func TestServingAdoptionActualHTTPConcurrentRequestsHaveOneOutcome(t *testing.T) {
	f := newServingCommandFixture(t)
	adoptionSeed(t, f, "competing")
	s := migrationServingAPI(t, f, nil)
	server := httptest.NewServer(servingMutationRoutes(s))
	defer server.Close()
	reviewed := adoptionSelection(t, f, server)
	raw, err := json.Marshal(map[string]any{"zones": reviewed})
	if err != nil {
		t.Fatal(err)
	}
	adoptionWait(t, f)
	type outcome struct {
		status int
		body   []byte
		err    error
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	for range 2 {
		r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/api/v1/imports/adopt", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+f.token)
		r.Header.Set("Content-Type", "application/json")
		go func() {
			<-start
			response, err := server.Client().Do(r)
			if err != nil {
				results <- outcome{err: err}
				return
			}
			body, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			results <- outcome{status: response.StatusCode, body: body, err: err}
		}()
	}
	close(start)
	var winner api.MigrationResult
	successes, refused := 0, 0
	for range 2 {
		out := <-results
		if out.err != nil {
			t.Fatal(out.err)
		}
		switch out.status {
		case http.StatusOK:
			successes++
			winner = adoptionJSON[api.MigrationResult](t, out.body)
		case http.StatusConflict, http.StatusServiceUnavailable:
			refused++
		default:
			t.Fatalf("unexpected competing outcome %d: %s", out.status, out.body)
		}
	}
	if successes != 1 || refused != 1 || winner.RunID == "" {
		t.Fatal("competing adoption did not have exactly one winner", successes, refused)
	}
	runs := adoptionJSON[api.List[api.MigrationRun]](t, adoptionHTTP(t, f, server, http.MethodGet, "/imports/runs", nil, http.StatusOK))
	if len(runs.Items) != 1 || runs.Items[0].ID != winner.RunID || runs.Items[0].Data.Status != "completed" {
		t.Fatal("competing requests created inconsistent journals", runs)
	}
	var z public.DNSZone
	if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "DNSZone", Name: reviewed[0].Name}, &z); err != nil || z.Annotations["kudurru.io/migration-run"] != winner.RunID {
		t.Fatal("journal does not own adopted zone", err, z)
	}
}

func TestServingAdoptionActualHTTPInvalidLastCandidateCreatesNoJournal(t *testing.T) {
	for _, mode := range []string{"duplicate", "uid", "rv", "manager"} {
		t.Run(mode, func(t *testing.T) {
			f := newServingCommandFixture(t)
			adoptionSeed(t, f, "first", "last")
			s := migrationServingAPI(t, f, nil)
			server := httptest.NewServer(servingMutationRoutes(s))
			defer server.Close()
			reviewed := adoptionSelection(t, f, server)
			want := http.StatusConflict
			switch mode {
			case "duplicate":
				reviewed[1] = reviewed[0]
				want = http.StatusBadRequest
			case "uid":
				reviewed[1].UID = "replaced-after-review"
			case "rv":
				reviewed[1].ResourceVersion = "0"
				want = http.StatusPreconditionFailed
			case "manager":
				var z public.DNSZone
				if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "DNSZone", Name: reviewed[1].Name}, &z); err != nil {
					t.Fatal(err)
				}
				if _, err := f.s.opts.Store.Apply(t.Context(), &z, store.ApplyOptions{Manager: store.ManagerFile("zones.yaml"), Force: true}); err != nil {
					t.Fatal(err)
				}
			}
			adoptionWait(t, f)
			before, err := f.s.opts.Store.ReadGeneration(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			adoptionHTTP(t, f, server, http.MethodPost, "/imports/adopt", map[string]any{"zones": reviewed}, want)
			adoptionUnchangedImage(t, f.s.opts.Store, before)
		})
	}
}

func TestServingAdoptionActualHTTPFinalOriginalLicenseDeadline(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		t.Run(fmt.Sprintf("clock-rollback=%t", rollback), func(t *testing.T) {
			licensed := newBrandingHTTPFixture(t)
			f := &servingCommandFixture{s: licensed.s, http: licensed.adapter, token: licensed.token}
			adoptionSeed(t, f, "licensed")
			var calls atomic.Int32
			command := func(ctx context.Context, p *api.MigrationProgram) (api.MigrationNext, error) {
				next, err := f.s.MigrationCommand(ctx, p)
				if err != nil {
					return nil, err
				}
				return func(ctx context.Context) error {
					original := f.s.opts.Now
					f.s.opts.Now = func() time.Time {
						// The program deadline was fixed at admission; first is
						// the pre-execution deadline, second the writer check,
						// third the original proof's authentication clock. Move
						// only the licensed host clock before staging the effects.
						if calls.Add(1) == 3 {
							at := licensed.base.Add(time.Hour + license.Grace)
							if rollback {
								at = licensed.base.Add(-time.Second)
							}
							licensed.clock.Store(at.UnixNano())
						}
						return licensed.base
					}
					defer func() {
						f.s.opts.Now = original
						licensed.clock.Store(licensed.base.UnixNano())
					}()
					return next(ctx)
				}, nil
			}
			s := migrationServingAPI(t, f, command)
			server := httptest.NewServer(servingMutationRoutes(s))
			defer server.Close()
			reviewed := adoptionSelection(t, f, server)
			adoptionWait(t, f)
			before, err := f.s.opts.Store.ReadGeneration(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			adoptionHTTP(t, f, server, http.MethodPost, "/imports/adopt", map[string]any{"zones": reviewed}, http.StatusServiceUnavailable)
			if calls.Load() < 3 {
				t.Fatal("license final check was not reached")
			}
			adoptionUnchangedImage(t, f.s.opts.Store, before)
		})
	}
}
