package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/externaldns"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// webhookRecordsPeer is the first-party external-dns listener over its own
// memory store, served over TLS: an independent record peer (D380).
type webhookRecordsPeer struct {
	server *httptest.Server
	ed     *externaldns.Server
	ca     string
	mu     sync.Mutex
	posts  int
	// unreadable makes every record list after the next POST answer 503.
	unreadable, failing bool
	// hold, when set, holds the next POST before it reaches the listener:
	// the peer closes entered and waits until hold is closed. Used once.
	hold, entered chan struct{}
}

func newWebhookRecordsPeer(t *testing.T, token string) *webhookRecordsPeer {
	t.Helper()
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	st := memory.New()
	zone := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "example"}, Spec: public.DNSZoneSpec{ZoneName: "example.", Type: public.ZoneTypePrimary, Nameservers: []string{"ns.example."}}}
	if _, err := st.Apply(t.Context(), zone, store.ApplyOptions{Manager: "api"}); err != nil {
		t.Fatal(err)
	}
	return newWebhookRecordsPeerOver(t, token, st, v)
}

// newWebhookRecordsPeerOver serves the first-party external-dns listener
// over st, which may be another runtime's store.
func newWebhookRecordsPeerOver(t *testing.T, token string, st store.Store, v *validator.Validator) *webhookRecordsPeer {
	t.Helper()
	ed, err := externaldns.New(externaldns.Options{Store: st, Validator: v, Namespace: "external-dns", TokenFile: token, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	p := &webhookRecordsPeer{ed: ed}
	p.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		var hold chan struct{}
		if r.Method == http.MethodPost && r.URL.Path == "/records" {
			p.posts++
			p.failing = p.unreadable
			if p.hold != nil {
				hold = p.hold
				close(p.entered)
				p.hold = nil
			}
		}
		failing := p.failing && r.Method == http.MethodGet && r.URL.Path == "/records"
		p.mu.Unlock()
		if hold != nil {
			// The held body is applied even if the client went away: the
			// peer took the change although no answer arrived.
			var ch externaldns.Changes
			if err := json.NewDecoder(r.Body).Decode(&ch); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			<-hold
			if err := ed.ApplyChanges(context.Background(), &ch); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if failing {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		ed.ServeHTTP(w, r)
	}))
	t.Cleanup(p.server.Close)
	p.ca = filepath.Join(t.TempDir(), "records-peer-ca.pem")
	if err := os.WriteFile(p.ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func (p *webhookRecordsPeer) postCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.posts
}

func (p *webhookRecordsPeer) setUnreadable(v bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unreadable, p.failing = v, false
}

// rows returns the peer's rows as name/type -> targets.
func (p *webhookRecordsPeer) rows(t *testing.T) map[string][]string {
	t.Helper()
	eps, err := p.ed.Records(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, e := range eps {
		out[e.DNSName+"/"+e.RecordType] = slices.Clone(e.Targets)
	}
	return out
}

// wipe removes every row from the peer, behind the instance's back.
func (p *webhookRecordsPeer) wipe(t *testing.T) {
	t.Helper()
	eps, err := p.ed.Records(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) == 0 {
		return
	}
	if err := p.ed.ApplyChanges(t.Context(), &externaldns.Changes{Delete: eps}); err != nil {
		t.Fatal(err)
	}
}

// put writes rows to the peer as another writer would.
func (p *webhookRecordsPeer) put(t *testing.T, eps ...*externaldns.Endpoint) {
	t.Helper()
	if err := p.ed.ApplyChanges(t.Context(), &externaldns.Changes{Create: eps}); err != nil {
		t.Fatal(err)
	}
}

func webhookRecordsDefinition(f *integrationOperationFixture, p *webhookRecordsPeer) *public.Integration {
	return &public.Integration{ObjectMeta: metav1.ObjectMeta{Name: "records", Namespace: "default"}, Spec: public.IntegrationSpec{Type: "webhook", Resync: metav1.Duration{Duration: time.Hour},
		Credentials: map[string]public.CredentialReference{"bearerToken": {File: f.path}, "ca": {File: p.ca}},
		Webhook:     &public.WebhookIntegration{Endpoint: p.server.URL, Domains: []string{"example"}}}}
}

// webhookRecordsFixture starts a serving runtime whose webhook Integration
// publishes the sample zone's record sets to a first-party external-dns
// peer, after one source record set is added.
func webhookRecordsFixture(t *testing.T) (*integrationOperationFixture, *webhookRecordsPeer) {
	t.Helper()
	return webhookRecordsFixtureWith(t, func(token string) *webhookRecordsPeer { return newWebhookRecordsPeer(t, token) })
}

// webhookRecordsFixtureWith is webhookRecordsFixture with a chosen peer;
// newPeer receives the bearer credential file the instance presents.
func webhookRecordsFixtureWith(t *testing.T, newPeer func(token string) *webhookRecordsPeer) (*integrationOperationFixture, *webhookRecordsPeer) {
	t.Helper()
	f := &integrationOperationFixture{}
	var peer *webhookRecordsPeer
	f.s, f.path = integrationFixture(t, func(o *servingRuntimeOptions) {
		peer = newPeer(o.Config.Credentials.AllowedFiles[0])
		o.Config.Credentials.AllowedFiles = append(o.Config.Credentials.AllowedFiles, peer.ca)
	}, nil)
	integrationOperationCaller(t, f)
	record := &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "published", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "home"}, Endpoints: []public.Endpoint{{DNSName: "web.example.", RecordType: "A", RecordTTL: 300, Targets: []string{"192.0.2.80"}}}}}
	if _, err := f.s.opts.Store.Apply(t.Context(), record, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	integrationOperationStart(t, f, webhookRecordsDefinition(f, peer))
	return f, peer
}

func webhookRecordsReview(t *testing.T, f *integrationOperationFixture, router http.Handler, path string) api.IntegrationReview {
	t.Helper()
	integrationAdminSettled(t, f)
	w := integrationAdminCall(t, router, f, http.MethodGet, path, nil, f.token)
	var summary api.IntegrationSummary
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &summary) != nil {
		t.Fatal("review unavailable", w.Code)
	}
	awaitServingRuntime(t, f.s, func() bool {
		_, err := f.s.integrations.host.CaptureCommand(t.Context(), integrationOwner(f.s, f.d), summary.Revision)
		return err == nil
	})
	return api.IntegrationReview{UID: summary.UID, Revision: summary.Revision}
}

func webhookRecordsPreview(t *testing.T, f *integrationOperationFixture, router http.Handler, path string, review api.IntegrationReview) api.IntegrationExecutionResult {
	t.Helper()
	w := integrationAdminCall(t, router, f, http.MethodPost, path+"/preview", review, f.token)
	var result api.IntegrationExecutionResult
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Preview == nil {
		t.Fatal("webhook preview refused", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "kudurru/owner") {
		t.Fatal("preview disclosed the registry marker")
	}
	integrationAdminSettled(t, f)
	return result
}

func webhookRecordsPending(t *testing.T, f *integrationOperationFixture) int {
	t.Helper()
	list, err := f.s.opts.Store.List(t.Context(), kinds.KindIntegrationLedger, store.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pending := 0
	for _, o := range list.Items {
		if l := o.(*kinds.IntegrationLedger); l.Spec.Pending != nil {
			pending++
		}
	}
	return pending
}

// A webhook Integration with domains is a publisher mounted on the real
// SQLite writer, router and a TLS peer (D380): its background sync and a
// reviewed preview/sync write the data and registry rows at the peer, a
// stale preview is a conflict with no POST, an applied but unreadable
// result is 503 integration_uncertain with the ledger pending and no
// replay, and the next sync confirms it from a reread.
func TestIntegrationWebhookMountedRecordPublication(t *testing.T) {
	f, peer := webhookRecordsFixture(t)
	marker := `"heritage=kudurru,kudurru/owner=`
	rows := peer.rows(t)
	if !slices.Equal(rows["web.example/A"], []string{"192.0.2.80"}) || len(rows["_kudurru-a.web.example/TXT"]) != 1 || !strings.HasPrefix(rows["_kudurru-a.web.example/TXT"][0], marker) {
		t.Fatal("background publication did not write the data and registry rows", rows)
	}
	router := integrationAdminRouter(t, f)
	path := "/integrations/default/records"

	// Remote drift needs a new operator preview.
	peer.wipe(t)
	before := peer.postCount()
	review := webhookRecordsReview(t, f, router, path)
	result := webhookRecordsPreview(t, f, router, path, review)
	if len(result.Preview.Operations) == 0 {
		t.Fatal("remote drift did not produce reviewed operations")
	}
	review.Mode = "observe"
	if w := integrationAdminCall(t, router, f, http.MethodPost, path+"/sync", review, f.token); w.Code != http.StatusBadRequest && w.Code != http.StatusUnprocessableEntity {
		t.Fatal("a publisher accepted an observe review", w.Code)
	}
	integrationAdminSettled(t, f)
	review.Mode, review.PreviewDigest = "publish", strings.Repeat("a", 64)
	if w := integrationAdminCall(t, router, f, http.MethodPost, path+"/sync", review, f.token); w.Code != http.StatusConflict || peer.postCount() != before {
		t.Fatal("stale preview reached the peer", w.Code)
	}
	integrationAdminSettled(t, f)
	review.PreviewDigest = result.Preview.Digest
	w := integrationAdminCall(t, router, f, http.MethodPost, path+"/sync", review, f.token)
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || !result.Completed || result.Uncertain || result.Applied == 0 || peer.postCount() <= before {
		t.Fatal("reviewed webhook sync", w.Code, w.Body.String())
	}
	if rows := peer.rows(t); !slices.Equal(rows["web.example/A"], []string{"192.0.2.80"}) || len(rows["_kudurru-a.web.example/TXT"]) != 1 {
		t.Fatal("reviewed sync did not restore the rows", rows)
	}

	// Applied but unreadable: uncertain, pending, sent once.
	peer.wipe(t)
	review = webhookRecordsReview(t, f, router, path)
	result = webhookRecordsPreview(t, f, router, path, review)
	review.Mode, review.PreviewDigest = "publish", result.Preview.Digest
	peer.setUnreadable(true)
	before = peer.postCount()
	w = integrationAdminCall(t, router, f, http.MethodPost, path+"/sync", review, f.token)
	var refusal struct {
		Error struct {
			Key  string                         `json:"key"`
			Hint api.IntegrationExecutionResult `json:"hint"`
		} `json:"error"`
	}
	if w.Code != http.StatusServiceUnavailable || json.Unmarshal(w.Body.Bytes(), &refusal) != nil || refusal.Error.Key != "integration_uncertain" || !refusal.Error.Hint.Uncertain || refusal.Error.Hint.Pending != 1 {
		t.Fatal("unreadable effect lost its uncertain refusal", w.Code, w.Body.String())
	}
	if peer.postCount() != before+1 || webhookRecordsPending(t, f) != 1 {
		t.Fatal("uncertain effect was replayed or not left pending", peer.postCount()-before, webhookRecordsPending(t, f))
	}
	peer.setUnreadable(false)
	integrationAdminSettled(t, f)
	// The next sync (the scheduler's, with no review) settles the pending
	// intent from a reread and sends nothing.
	before = peer.postCount()
	awaitServingRuntime(t, f.s, func() bool {
		res, err := f.s.integrations.host.Sync(t.Context(), integrationOwner(f.s, f.d), "")
		return err == nil && res.Pending == 0
	})
	if peer.postCount() != before || webhookRecordsPending(t, f) != 0 {
		t.Fatal("recovery replayed the effect or left it pending", peer.postCount()-before)
	}
}
