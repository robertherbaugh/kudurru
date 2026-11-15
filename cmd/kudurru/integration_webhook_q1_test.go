package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/netip"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/ee/integration/webhook"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// webhookInventoryExpiry asserts the original observation: present now,
// timed by the peer's LastSeen, and gone LastSeen plus three minutes.
func webhookInventoryExpiry(t *testing.T, s *servingRuntime, p *webhookMountPeer) bool {
	t.Helper()
	snapshot := integrationInventory(t, s)
	if snapshot == nil {
		return false
	}
	ip := netip.MustParseAddr("192.0.2.80")
	c := snapshot.Lookup(ip, time.Now())
	return c != nil && c.ObservedAt.Equal(p.at) && c.ExpiresAt.Equal(p.at.Add(3*time.Minute)) && snapshot.Lookup(ip, p.at.Add(3*time.Minute)) == nil
}

// A process restart of a clients webhook Integration keeps the accepted
// inventory only until the original LastSeen plus three minutes, and the
// restarted runtime negotiates again and polls (F60c-Q1).
func TestIntegrationWebhookMountedClientsRestart(t *testing.T) {
	p := newWebhookMountPeer(t, true)
	f := webhookMountFixture(t, p, false)
	if !webhookInventoryExpiry(t, f.s, p) {
		t.Fatal("original observation missing before restart")
	}
	reads := p.reads.Load()
	webhookRecordsRestart(t, f)
	// The restored image serves the original observation unrenewed.
	awaitServingRuntime(t, f.s, func() bool { return webhookInventoryExpiry(t, f.s, p) })
	// The restarted instance negotiates and polls the peer again.
	awaitServingRuntime(t, f.s, func() bool { return p.reads.Load() > reads })
	integrationReadyCommand(t, f.s, integrationOwner(f.s, f.d))
	if p.unexpected.Load() != 0 {
		t.Fatal("the peer saw an unexpected request", p.unexpected.Load())
	}
	awaitServingRuntime(t, f.s, func() bool { return webhookInventoryExpiry(t, f.s, p) })
}

// A follower of a clients webhook Integration builds no provider, serves the
// replicated inventory unrenewed and refuses integration operations; after
// promotion it negotiates and polls (F60c-Q1).
func TestIntegrationWebhookMountedClientsFollowerAndPromotion(t *testing.T) {
	p := newWebhookMountPeer(t, true)
	f := webhookMountFixture(t, p, false)
	primary := f.s
	owner := integrationOwner(primary, f.d)
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "follower.db"), sqlite.Options{Scheme: scheme, Validator: primary.opts.Validator, InternalKinds: privateStoreKinds(), InitialMode: replication.Follower, Identity: "clients-successor", ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err = st.Enroll(t.Context(), primary.opts.Store.Authority().Term); err != nil {
		t.Fatal(err)
	}
	installCtx := captureServingReplica(t, st)
	image, err := primary.opts.Store.ReadGeneration(installCtx)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := st.ValidateImage(installCtx, *image)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := st.InstallReplica(installCtx, validated, replication.InstallOptions{ExpectedTerm: image.Term})
	if err != nil {
		t.Fatal(err)
	}
	opts := primary.opts
	opts.Store = st
	follower, err := newServingRuntime(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := follower.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	var provisions atomic.Int32
	follower.integrations.factory = func(typ string) (integration.Integration, error) {
		provisions.Add(1)
		if typ != "webhook" {
			t.Error("unexpected integration type", typ)
		}
		return webhook.New(), nil
	}
	runServingFixture(t, follower)
	awaitServingRuntime(t, follower, func() bool { return follower.publisher.Status().ProtectedReady })
	if !webhookInventoryExpiry(t, follower, p) {
		t.Fatal("the follower does not serve the replicated observation unrenewed")
	}
	// The follower refuses an integration operation through the real router.
	g := &integrationOperationFixture{s: follower, path: f.path, token: f.token, tokenID: f.tokenID}
	if g.adapter, err = servinghttp.New(servinghttp.Options{Publisher: follower.publisher, Now: follower.opts.Now}); err != nil {
		t.Fatal(err)
	}
	// The router as serve mounts it on a follower: the cluster role comes
	// from the source the request's lease captured.
	srv, err := api.New(api.Options{Store: follower.opts.Store, Validator: follower.opts.Validator, Config: follower.opts.Config, CanonicalSettings: follower, Integrations: follower.integrations, Authenticate: g.adapter.Middleware, Authorizer: g.adapter, ClusterRole: follower.ClusterRole, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	var router http.Handler = srv
	review := api.IntegrationReview{UID: string(f.d.UID), Revision: f.d.ResourceVersion}
	w := integrationAdminCall(t, router, g, http.MethodPost, "/integrations/default/webhook/check", review, g.token)
	var refusal struct {
		Error struct {
			Key string `json:"key"`
		} `json:"error"`
	}
	// Integration operations run under the primary's authority (D368): the
	// follower answers 503 primary_required and makes no provider call.
	if w.Code != http.StatusServiceUnavailable || json.Unmarshal(w.Body.Bytes(), &refusal) != nil || refusal.Error.Key != api.KeyPrimaryRequired {
		t.Fatal("a follower ran an integration operation", w.Code, w.Body.String())
	}
	t.Logf("follower refusal: %d %s", w.Code, refusal.Error.Key)
	time.Sleep(3 * time.Second)
	if provisions.Load() != 0 {
		t.Fatal("a follower built a provider", provisions.Load())
	}
	reads := p.reads.Load()
	promoted, err := st.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: image.Term, ExpectedInstalled: installed, RequestID: "clients-failover"})
	if err != nil {
		t.Fatal(err)
	}
	f.stop()
	if _, err = primary.opts.Store.ObserveAuthority(t.Context(), promoted.Term); err != nil {
		t.Fatal(err)
	}
	integrationWriter(t, follower)
	awaitServingRuntime(t, follower, func() bool {
		var head kinds.IntegrationSourceHead
		if st.Get(t.Context(), store.Key{Kind: kinds.KindIntegrationSourceHead, Name: kinds.SourceHeadName(owner)}, &head) != nil {
			return false
		}
		return provisions.Load() > 0 && p.reads.Load() > reads && head.Spec.Sequence >= 2
	})
	if p.unexpected.Load() != 0 {
		t.Fatal("the peer saw an unexpected request", p.unexpected.Load())
	}
	awaitServingRuntime(t, follower, func() bool { return webhookInventoryExpiry(t, follower, p) })
}
