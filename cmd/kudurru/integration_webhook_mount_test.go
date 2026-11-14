package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/inventory"
	inventorypersistence "github.com/robertherbaugh/kudurru/internal/inventory/persistence"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const webhookMountMedia = "application/kudurru.integration+json;version=1"

type webhookMountPeer struct {
	server                   *httptest.Server
	clients                  bool
	at                       time.Time
	posts, reads, unexpected atomic.Int32
	uncertain                atomic.Bool
	clientAction             atomic.Pointer[func()]
}

func newWebhookMountPeer(t *testing.T, clients bool) *webhookMountPeer {
	t.Helper()
	p := &webhookMountPeer{clients: clients, at: time.Now().UTC().Truncate(time.Second)}
	network := integration.Network{Name: "lan", VLAN: 10, CIDR: "192.0.2.0/24", Domain: "example.test"}
	client := integration.Client{ID: "device", Name: "Device", Hostname: "device.example.test", IPs: []string{"192.0.2.80"}, MACs: []string{"02:00:00:00:00:80"}, Network: network, FixedIP: "192.0.2.99", LastSeen: p.at}
	p.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-only-private-credential" {
			p.unexpected.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", webhookMountMedia)
		switch r.URL.Path {
		case "/provider/":
			capability := "actions"
			manifestCapability := integration.CapActions
			if clients {
				capability = "clients"
				manifestCapability = integration.CapClientSource
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"filters": []string{}, "capabilities": []string{capability}, "manifest": integration.Manifest{Type: "fixture", DisplayName: "Fixture", Version: "1", IOClass: "local_polling", Capabilities: []integration.Capability{manifestCapability}, ConfigSchema: json.RawMessage(`{"type":"object"}`)}})
		case "/provider/actions":
			if clients {
				p.unexpected.Add(1)
			}
			_ = json.NewEncoder(w).Encode([]integration.ActionSpec{{Name: "refresh", Verb: "integrations.act", Params: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["confirm"],"properties":{"confirm":{"type":"boolean"}}}`)}})
		case "/provider/actions/refresh":
			p.posts.Add(1)
			if r.Method != http.MethodPost || clients {
				p.unexpected.Add(1)
			}
			if p.uncertain.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case "/provider/clients":
			p.reads.Add(1)
			if !clients {
				p.unexpected.Add(1)
			}
			if action := p.clientAction.Load(); action != nil {
				(*action)()
			}
			_ = json.NewEncoder(w).Encode([]integration.Client{client})
		case "/provider/networks":
			p.reads.Add(1)
			if !clients {
				p.unexpected.Add(1)
			}
			_ = json.NewEncoder(w).Encode([]integration.Network{network})
		default:
			p.unexpected.Add(1)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(p.server.Close)
	return p
}

func webhookMountFixture(t *testing.T, p *webhookMountPeer, viewer bool) *integrationOperationFixture {
	t.Helper()
	ca := filepath.Join(t.TempDir(), "peer-ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	f := &integrationOperationFixture{}
	f.s, f.path = integrationFixture(t, func(o *servingRuntimeOptions) {
		o.Config.Credentials.AllowedFiles = append(o.Config.Credentials.AllowedFiles, ca)
		// These clients/actions fixtures have no DNS record destination. Keep
		// the real writer and publisher without the inherited sample zone's
		// independent startup status/source transactions.
		if err := o.Store.Delete(t.Context(), store.Key{Kind: "DNSZone", Name: "home"}, store.DeleteOptions{Manager: store.ManagerSystem, Force: true}); err != nil {
			t.Fatal(err)
		}
	}, nil)
	integrationOperationCaller(t, f)
	if viewer {
		if err := f.s.opts.Store.Command(t.Context(), f.s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			var token internalv1.APIToken
			if err := tx.Get(ctx, store.Key{Kind: "APIToken", Name: f.tokenID}, &token); err != nil {
				return err
			}
			token.Spec.Scopes = []string{"viewer"}
			_, err := tx.Apply(ctx, &token, store.ApplyOptions{Manager: store.ManagerSystem, ExpectedResourceVersion: token.ResourceVersion})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	d := &public.Integration{ObjectMeta: metav1.ObjectMeta{Name: "webhook", Namespace: "default"}, Spec: public.IntegrationSpec{Type: "webhook", Credentials: map[string]public.CredentialReference{"bearerToken": {File: f.path}, "ca": {File: ca}}, Webhook: &public.WebhookIntegration{Endpoint: p.server.URL + "/provider"}, Resync: metav1.Duration{Duration: 10 * time.Minute}}}
	f.d = integrationApply(t, f.s, d)
	f.stop = integrationRun(t, f.s)
	if p.clients {
		integrationReadyCommand(t, f.s, integrationOwner(f.s, f.d))
	} else {
		awaitServingRuntime(t, f.s, func() bool {
			for _, status := range f.s.integrations.host.Statuses() {
				if status.Owner == integrationOwner(f.s, f.d) && status.Phase == "Running" {
					// Join startup before reviewing the original action. This
					// check charges a budget, so subsequent publication waiting
					// must not repeat it after it succeeds.
					_, err := f.s.integrations.host.Check(t.Context(), status.Owner)
					return err == nil
				}
			}
			return false
		})
	}
	awaitServingRuntime(t, f.s, func() bool {
		phase := "Running"
		if p.clients {
			phase = "Ready"
		}
		ready := false
		for _, s := range f.s.integrations.host.Statuses() {
			ready = ready || s.Owner == integrationOwner(f.s, f.d) && s.Phase == phase
		}
		if !ready {
			return false
		}
		var current public.Integration
		if f.s.opts.Store.Get(t.Context(), store.KeyOf(f.d), &current) != nil {
			return false
		}
		if _, err := f.s.integrations.host.CaptureCommand(t.Context(), integrationOwner(f.s, &current), current.ResourceVersion); err != nil {
			return false
		}
		lease, err := f.s.publisher.CaptureHTTP(t.Context())
		if err != nil {
			return false
		}
		defer lease.Release()
		if lease.Bundle().Identity().Base.Revision != f.s.publisher.Status().Authority.Installed.Revision {
			return false
		}
		f.d = current.DeepCopy()
		return true
	})
	var err error
	f.adapter, err = servinghttp.New(servinghttp.Options{Publisher: f.s.publisher, Now: f.s.opts.Now})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestIntegrationWebhookMountedOriginalClientObservation(t *testing.T) {
	p := newWebhookMountPeer(t, true)
	f := webhookMountFixture(t, p, false)
	snapshot := integrationInventory(t, f.s)
	ip := netip.MustParseAddr("192.0.2.80")
	c := snapshot.Lookup(ip, time.Now())
	if c == nil || !c.ObservedAt.Equal(p.at) || !c.ExpiresAt.Equal(p.at.Add(3*time.Minute)) || snapshot.Lookup(netip.MustParseAddr("192.0.2.99"), time.Now()) != nil || snapshot.Lookup(ip, p.at.Add(3*time.Minute)) != nil {
		t.Fatal("mounted source renewed time or granted desired address")
	}
	var head kinds.IntegrationSourceHead
	if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: kinds.KindIntegrationSourceHead, Name: kinds.SourceHeadName(integrationOwner(f.s, f.d))}, &head); err != nil {
		t.Fatal(err)
	}
	if head.Spec.Sequence != 1 || p.reads.Load() != 2 || p.posts.Load() != 0 || p.unexpected.Load() != 0 {
		t.Fatal("initial source did not use one complete original poll")
	}
	before := p.reads.Load()
	review := api.IntegrationReview{UID: string(f.d.UID), Revision: f.d.ResourceVersion, Mode: "observe"}
	w := integrationAdminCall(t, integrationAdminRouter(t, f), f, http.MethodPost, "/integrations/default/webhook/sync", review, f.token)
	var result api.IntegrationExecutionResult
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || !result.SourceCommitted || !result.SourcePublished || result.Clients != 1 || p.reads.Load() != before+2 {
		var refusal struct {
			Error struct {
				Key  string                         `json:"key"`
				Hint api.IntegrationExecutionResult `json:"hint"`
			} `json:"error"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &refusal)
		t.Fatalf("mounted source sync: status=%d key=%q committed=%t published=%t uncertain=%t reads=%d", w.Code, refusal.Error.Key, result.SourceCommitted || refusal.Error.Hint.SourceCommitted, result.SourcePublished || refusal.Error.Hint.SourcePublished, refusal.Error.Hint.Uncertain, p.reads.Load()-before)
	}
	webhookMountActAudit(t, f, "integration.sync", sqlite.AuditSuccess)
	// Later operation/status commits may supersede the publication acknowledged
	// by Sync. Wait only for a read of the resulting complete serving snapshot.
	var nextSnapshot *inventory.Snapshot
	awaitServingRuntime(t, f.s, func() bool {
		nextSnapshot = integrationInventory(t, f.s)
		return nextSnapshot != nil
	})
	next := nextSnapshot.Lookup(ip, time.Now())
	if next == nil || !next.ExpiresAt.Equal(c.ExpiresAt) || !next.ObservedAt.Equal(c.ObservedAt) {
		t.Fatalf("later poll observation: publication=%t live=%t sameExpiry=%t sameObserved=%t", nextSnapshot != nil, next != nil, next != nil && next.ExpiresAt.Equal(c.ExpiresAt), next != nil && next.ObservedAt.Equal(c.ObservedAt))
	}
}

func TestIntegrationWebhookMountedActionsAndOriginalRefusals(t *testing.T) {
	for _, mode := range []string{"valid", "invalid-schema", "viewer", "uncertain"} {
		t.Run(mode, func(t *testing.T) {
			p := newWebhookMountPeer(t, false)
			p.uncertain.Store(mode == "uncertain")
			f := webhookMountFixture(t, p, mode == "viewer")
			params := json.RawMessage(`{"confirm":true}`)
			if mode == "invalid-schema" {
				params = json.RawMessage(`{"confirm":"invalid"}`)
			}
			review := api.IntegrationReview{UID: string(f.d.UID), Revision: f.d.ResourceVersion, Params: params}
			w := integrationAdminCall(t, integrationAdminRouter(t, f), f, http.MethodPost, "/integrations/default/webhook/actions/refresh", review, f.token)
			var outcome struct {
				Error struct {
					Key  string                         `json:"key"`
					Hint api.IntegrationExecutionResult `json:"hint"`
				} `json:"error"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &outcome)
			t.Cleanup(func() {
				if t.Failed() {
					t.Logf("original action: status=%d key=%q posts=%d uncertain=%t completed=%t", w.Code, outcome.Error.Key, p.posts.Load(), outcome.Error.Hint.Uncertain, outcome.Error.Hint.Completed)
				}
			})
			switch mode {
			case "valid":
				if w.Code != http.StatusOK || p.posts.Load() != 1 {
					t.Fatal("original action did not execute exactly once", w.Code)
				}
			case "invalid-schema":
				if w.Code != http.StatusBadRequest || p.posts.Load() != 0 {
					t.Fatal("invalid action reached peer", w.Code)
				}
			case "viewer":
				if w.Code != http.StatusForbidden || p.posts.Load() != 0 {
					t.Fatal("scoped token reached peer action", w.Code)
				}
			case "uncertain":
				var refusal struct {
					Error struct {
						Key  string                         `json:"key"`
						Hint api.IntegrationExecutionResult `json:"hint"`
					} `json:"error"`
				}
				if w.Code != http.StatusServiceUnavailable || p.posts.Load() != 1 || json.Unmarshal(w.Body.Bytes(), &refusal) != nil || refusal.Error.Key != "integration_uncertain" || !refusal.Error.Hint.Uncertain || refusal.Error.Hint.Completed {
					t.Fatal("uncertain action was replayed or acknowledged", w.Code)
				}
			}
			want := map[string]string{"valid": sqlite.AuditSuccess, "uncertain": sqlite.AuditError}[mode]
			webhookMountActAudit(t, f, "integration.act", want)
			integrationNoSource(t, f.s, integrationOwner(f.s, f.d))
			if p.reads.Load() != 0 || p.unexpected.Load() != 0 {
				t.Fatal("actions-only provider polled inventory")
			}
		})
	}
}

func TestIntegrationWebhookMountedOriginalCredentialRefusal(t *testing.T) {
	p := newWebhookMountPeer(t, true)
	f := webhookMountFixture(t, p, false)
	key := store.Key{Kind: kinds.KindIntegrationSourceHead, Name: kinds.SourceHeadName(integrationOwner(f.s, f.d))}
	var before kinds.IntegrationSourceHead
	if err := f.s.opts.Store.Get(t.Context(), key, &before); err != nil {
		t.Fatal(err)
	}
	change := func() {
		if err := os.WriteFile(f.path, []byte("rotated-test-only-credential"), 0600); err != nil {
			t.Error("credential rotation fixture failed")
		}
	}
	p.clientAction.Store(&change)
	calls := p.reads.Load()
	review := api.IntegrationReview{UID: string(f.d.UID), Revision: f.d.ResourceVersion, Mode: "observe"}
	w := integrationAdminCall(t, integrationAdminRouter(t, f), f, http.MethodPost, "/integrations/default/webhook/sync", review, f.token)
	if w.Code != http.StatusServiceUnavailable || p.reads.Load() != calls+1 {
		t.Fatal("rotated original credential reached next inventory request", w.Code)
	}
	var after kinds.IntegrationSourceHead
	if err := f.s.opts.Store.Get(t.Context(), key, &after); err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if string(a) != string(b) {
		t.Fatal("refused original poll replaced accepted source")
	}
	// Credential rotation invalidates serving admission. Inspect the durable
	// accepted owner without treating retained inventory as serving authority.
	image, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	owner, err := inventorypersistence.Decode(image.Payload.Runtime, image.Term.ClusterID, image.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	holder, err := inventory.RestoreState(owner.State, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := holder.Snapshot()
	c := snapshot.Lookup(netip.MustParseAddr("192.0.2.80"), time.Now())
	if c == nil || !c.ObservedAt.Equal(p.at) || !c.ExpiresAt.Equal(p.at.Add(3*time.Minute)) {
		t.Fatal("refused poll changed the durable original observation lifetime")
	}
}

// webhookMountActAudit requires exactly one audit row of a reviewed command
// with the given result, or none when result is empty (D379).
func webhookMountActAudit(t *testing.T, f *integrationOperationFixture, action, result string) {
	t.Helper()
	var rows []sqlite.AuditEntry
	for _, entry := range integrationAudit(t, f.s, f.d) {
		if entry.Action == action {
			rows = append(rows, entry)
		}
	}
	if result == "" {
		if len(rows) != 0 {
			t.Fatalf("a refused %s was audited: %+v", action, rows)
		}
		return
	}
	if len(rows) != 1 || rows[0].Result != result || rows[0].Actor != "User:operation-admin" || !strings.Contains(rows[0].Reason, string(f.d.UID)) {
		t.Fatalf("%s audit: %+v", action, rows)
	}
}
