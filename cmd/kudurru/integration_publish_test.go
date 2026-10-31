package main

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/ee/integration/plan"
	"github.com/robertherbaugh/kudurru/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The mount uses the real host/SQLite ledger and request budget with a bounded
// provider double; SDK wire behavior is covered separately by cloudflare tests.
type integrationPublisher struct {
	integration.Integration
	host   ih.OwnerHost
	mu     sync.Mutex
	remote []plan.RemoteRecord
	calls  int
}

func (p *integrationPublisher) Provision(ctx context.Context, h integration.Host, _ json.RawMessage, secrets integration.SecretResolver) error {
	p.host = h.(ih.OwnerHost)
	_, err := secrets.Resolve(ctx, "apiToken")
	return err
}
func (*integrationPublisher) Destinations(context.Context) ([]ih.Publication, error) {
	return []ih.Publication{{Destination: kinds.Destination{Provider: "cloudflare", Account: "account", Zone: "example."}, Domains: []string{"example."}}}, nil
}
func (p *integrationPublisher) Records(ctx context.Context, _ kinds.Destination) (ih.RemoteSnapshot, error) {
	var out ih.RemoteSnapshot
	err := p.host.Request(ctx, "cloudflare", "account", false, func(context.Context) error {
		p.mu.Lock()
		defer p.mu.Unlock()
		out = ih.RemoteSnapshot{Complete: true, Records: append([]plan.RemoteRecord(nil), p.remote...)}
		return nil
	})
	return out, err
}
func (*integrationPublisher) Adjust(_ context.Context, _ ih.Publication, endpoints []public.Endpoint) ([]public.Endpoint, error) {
	return endpoints, nil
}
func (p *integrationPublisher) Apply(ctx context.Context, op *kinds.IntegrationOperation) (kinds.Receipt, error) {
	var receipt kinds.Receipt
	err := p.host.Request(ctx, "cloudflare", "account", true, func(context.Context) error {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.calls++
		p.remote = []plan.RemoteRecord{{Endpoint: *op.Spec.After, IDs: []string{"provider-id"}, Version: "version-1", Owner: plan.RemoteOwner{ClusterID: op.Spec.Record.Owner.ClusterID, IntegrationUID: op.Spec.Record.Owner.UID}}}
		receipt = kinds.Receipt{Complete: true, ContentHash: op.Spec.AfterHash, ObservedAt: time.Now().UTC(), ProviderIDs: []string{"provider-id"}, Targets: []kinds.TargetResult{{Target: op.Spec.After.Targets[0], ProviderID: "provider-id", Applied: true}}}
		return nil
	})
	return receipt, err
}
func TestIntegrationRuntimeActualOutboundLedgerAndBudget(t *testing.T) {
	publisher := &integrationPublisher{Integration: &integrationProbe{}}
	s, path := integrationFixture(t, nil, func(string) (integration.Integration, error) { return publisher, nil })
	defer func() {
		if t.Failed() {
			for _, kind := range []store.Kind{kinds.KindIntegrationBudget, kinds.KindIntegrationOperation, kinds.KindIntegrationLedger} {
				rows, err := s.opts.Store.List(t.Context(), kind, store.ListOptions{})
				t.Log("publisher rows", kind, len(rows.Items), err != nil)
				for _, row := range rows.Items {
					if op, ok := row.(*kinds.IntegrationOperation); ok {
						t.Log("receipt saved", op.Spec.Receipt != nil)
					}
				}
			}
			publisher.mu.Lock()
			t.Log("mutation calls", publisher.calls)
			publisher.mu.Unlock()
			for _, status := range s.integrations.host.Statuses() {
				t.Log("publisher state", status.Phase, status.Reason, status.Result.Applied, status.Result.Pending)
			}
		}
	}()
	record := &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "published", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "home"}, Endpoints: []public.Endpoint{{DNSName: "published.example.", RecordType: "A", RecordTTL: 300, Targets: []string{"192.0.2.31"}}}}}
	if _, err := s.opts.Store.Apply(t.Context(), record, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	definition := &public.Integration{ObjectMeta: metav1.ObjectMeta{Name: "publish", Namespace: "default"}, Spec: public.IntegrationSpec{Type: "cloudflare", Credentials: map[string]public.CredentialReference{"apiToken": {File: path}}, Cloudflare: &public.CloudflareIntegration{Zones: []public.CloudflareZone{{Zone: "example."}}}, Resync: metav1.Duration{Duration: time.Hour}}}
	integrationApply(t, s, definition)
	integrationRun(t, s)
	awaitServingRuntime(t, s, func() bool {
		operations, err := s.opts.Store.List(t.Context(), kinds.KindIntegrationOperation, store.ListOptions{})
		if err != nil || len(operations.Items) != 1 {
			return false
		}
		receipt := operations.Items[0].(*kinds.IntegrationOperation).Spec.Receipt
		if receipt == nil || !receipt.Complete {
			return false
		}
		ledgers, err := s.opts.Store.List(t.Context(), kinds.KindIntegrationLedger, store.ListOptions{})
		return err == nil && len(ledgers.Items) == 1 && !ledgers.Items[0].(*kinds.IntegrationLedger).Spec.LastApplied.IsZero()
	})
	ledgers, err := s.opts.Store.List(t.Context(), kinds.KindIntegrationLedger, store.ListOptions{})
	if err != nil || len(ledgers.Items) != 1 {
		t.Fatal("outbound baseline missing", err != nil)
	}
	baseline := ledgers.Items[0].(*kinds.IntegrationLedger)
	if baseline.Spec.LastApplied.IsZero() || len(baseline.Spec.ProviderIDs) != 1 {
		t.Fatal("confirmed provider outcome did not advance durable baseline")
	}
	budgets, err := s.opts.Store.List(t.Context(), kinds.KindIntegrationBudget, store.ListOptions{})
	if err != nil || len(budgets.Items) != 1 {
		t.Fatal("actual provider request bypassed durable budget", err != nil)
	}
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if publisher.calls != 1 {
		t.Fatal("provider mutation replayed", publisher.calls)
	}
}
