package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/inventory"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/servingzones"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type integrationProbe struct {
	host    ih.OwnerHost
	observe func(context.Context, ih.OwnerHost) (ih.SourceSnapshot, error)
	stopped func()
}

func (*integrationProbe) Manifest() integration.Manifest { return integration.Manifest{Type: "unifi"} }
func (p *integrationProbe) Provision(ctx context.Context, h integration.Host, _ json.RawMessage, secrets integration.SecretResolver) error {
	p.host = h.(ih.OwnerHost)
	_, err := secrets.Resolve(ctx, "apiKey")
	return err
}
func (*integrationProbe) Validate(context.Context) error { return nil }
func (*integrationProbe) Check(context.Context) (integration.Status, error) {
	return integration.Status{Reachable: true, Authenticated: true}, nil
}
func (*integrationProbe) Start(ctx context.Context) error { <-ctx.Done(); return nil }
func (p *integrationProbe) Stop(context.Context) error {
	if p.stopped != nil {
		p.stopped()
	}
	return nil
}
func (p *integrationProbe) Observe(ctx context.Context) (ih.SourceSnapshot, error) {
	return p.observe(ctx, p.host)
}

func integrationDefinition(name, path string) *public.Integration {
	return &public.Integration{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: public.IntegrationSpec{Type: "unifi", Credentials: map[string]public.CredentialReference{"apiKey": {File: path}}, UniFi: &public.UniFiIntegration{Endpoint: "https://controller.invalid", Poll: metav1.Duration{Duration: time.Hour}}}}
}
func integrationFixture(t *testing.T, prepare func(*servingRuntimeOptions), factory func(string) (integration.Integration, error)) (*servingRuntime, string) {
	t.Helper()
	base := newServingRuntimeFixture(t)
	opts := base.opts
	if err := base.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(path, []byte("test-only-private-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	opts.Config.Auth.IdentityProviders = nil
	opts.Config.Credentials.AllowedFiles = []string{path}
	opts.License = license.Unrestricted()
	if prepare != nil {
		prepare(&opts)
	}
	s, err := newServingRuntime(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	s.integrations.factory = factory
	t.Cleanup(func() {
		if err := s.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return s, path
}
func integrationRun(t *testing.T, s *servingRuntime) func() {
	t.Helper()
	runServingFixture(t, s)
	return integrationWriter(t, s)
}
func integrationWriter(t *testing.T, s *servingRuntime) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- s.opts.Store.Elector("integration-test-writer").Run(ctx, func(ctx context.Context) {
			if err := s.RunWriter(ctx); err != nil && ctx.Err() == nil {
				t.Error(err)
			}
		}, nil)
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		})
	}
	t.Cleanup(stop)
	awaitServingRuntime(t, s, func() bool { return s.publisher.Status().ProtectedReady && s.primary.driver.Status().Ready })
	return stop
}
func integrationApply(t *testing.T, s *servingRuntime, d *public.Integration) *public.Integration {
	t.Helper()
	out, err := s.opts.Store.Apply(t.Context(), d, store.ApplyOptions{Manager: store.ManagerAPI, ExpectedResourceVersion: d.ResourceVersion})
	if err != nil {
		t.Fatal(err)
	}
	s.integrations.host.Wake()
	return out.(*public.Integration)
}
func integrationClient(o ih.InstanceID) inventory.Client {
	return inventory.Client{ID: "fixture-device", Source: ih.SourceID(o), IPs: []netip.Addr{netip.MustParseAddr("192.0.2.19")}, MACs: []string{"00:11:22:33:44:55"}, Name: "device", ObservedAt: time.Now().Add(-time.Second), ExpiresAt: time.Now().Add(time.Hour)}
}
func integrationInventory(t *testing.T, s *servingRuntime) *inventory.Snapshot {
	t.Helper()
	lease, err := s.publisher.CaptureHTTP(t.Context())
	if err != nil {
		return nil
	}
	defer lease.Release()
	value, _ := lease.Bundle().Component("inventory")
	return value.(*servingbuild.InventoryOwner).Snapshot
}
func TestIntegrationRuntimeActualSourceCommitAndWithdrawal(t *testing.T) {
	var polls atomic.Int32
	s, path := integrationFixture(t, nil, func(string) (integration.Integration, error) {
		return &integrationProbe{observe: func(_ context.Context, h ih.OwnerHost) (ih.SourceSnapshot, error) {
			polls.Add(1)
			return ih.SourceSnapshot{Complete: true, Clients: []inventory.Client{integrationClient(h.IntegrationOwner())}}, nil
		}}, nil
	})
	d := integrationApply(t, s, integrationDefinition("inventory", path))
	integrationRun(t, s)
	awaitServingRuntime(t, s, func() bool {
		snapshot := integrationInventory(t, s)
		return snapshot != nil && snapshot.Lookup(netip.MustParseAddr("192.0.2.19"), time.Now()) != nil
	})
	var head kinds.IntegrationSourceHead
	if err := s.opts.Store.Get(t.Context(), store.Key{Kind: kinds.KindIntegrationSourceHead, Name: kinds.SourceHeadName(kinds.Owner{ClusterID: s.opts.Store.Info().ClusterID, Namespace: d.Namespace, Name: d.Name, UID: string(d.UID)})}, &head); err != nil {
		t.Fatal(err)
	}
	if head.Spec.Sequence != 1 {
		t.Fatal("source self-triggered another observation", head.Spec.Sequence)
	}
	if err := s.opts.Store.Delete(t.Context(), store.KeyOf(d), store.DeleteOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	s.integrations.host.Wake()
	awaitServingRuntime(t, s, func() bool {
		snapshot := integrationInventory(t, s)
		return snapshot != nil && snapshot.Lookup(netip.MustParseAddr("192.0.2.19"), time.Now()) == nil
	})
	if polls.Load() != 1 {
		t.Fatal("withdrawal replayed observation", polls.Load())
	}
}

func integrationController(t *testing.T) (*httptest.Server, *atomic.Int32, *atomic.Bool) {
	t.Helper()
	var calls atomic.Int32
	var present atomic.Bool
	present.Store(true)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		page := func(rows []map[string]any) {
			_ = json.NewEncoder(w).Encode(map[string]any{"offset": 0, "limit": 200, "count": len(rows), "totalCount": len(rows), "data": rows})
		}
		base := "/proxy/network/integration/v1"
		switch r.URL.Path {
		case base + "/info":
			_ = json.NewEncoder(w).Encode(map[string]any{"applicationVersion": "10.6.106"})
		case base + "/sites":
			page([]map[string]any{{"id": "11111111-1111-4111-8111-111111111111", "internalReference": "default"}})
		case base + "/sites/11111111-1111-4111-8111-111111111111/clients":
			if !present.Load() {
				page([]map[string]any{})
				return
			}
			page([]map[string]any{{"id": "33333333-3333-4333-8333-333333333333", "name": "Laptop", "type": "WIRED", "ipAddress": "192.0.2.19", "macAddress": "00:11:22:33:44:55", "connectedAt": time.Now().Add(-time.Hour)}})
		case base + "/sites/11111111-1111-4111-8111-111111111111/networks":
			page([]map[string]any{{"id": "22222222-2222-4222-8222-222222222222", "name": "LAN", "management": "GATEWAY", "enabled": true, "vlanId": 10}})
		case base + "/sites/11111111-1111-4111-8111-111111111111/networks/22222222-2222-4222-8222-222222222222":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "22222222-2222-4222-8222-222222222222", "name": "LAN", "management": "GATEWAY", "enabled": true, "vlanId": 10, "ipv4Configuration": map[string]any{"hostIpAddress": "192.0.2.1", "prefixLength": 24}})
		default:
			w.WriteHeader(404)
		}
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, &calls, &present
}
func integrationPinnedDefinition(t *testing.T, server *httptest.Server, path string) *public.Integration {
	t.Helper()
	d := integrationDefinition("inventory", path)
	sum := sha256.Sum256(server.Certificate().Raw)
	d.Spec.UniFi.Endpoint = server.URL
	d.Spec.UniFi.TLSFingerprint = "sha256:" + hex.EncodeToString(sum[:])
	d.Spec.UniFi.Inventory = &public.UniFiInventory{ZoneRef: public.ObjectRef{Name: "home"}, NameTemplate: "{{ .Name | slug }}", ReverseZones: "disabled"}
	d.Spec.UniFi.Groups.ByNetwork = map[string]string{"LAN": "devices"}
	return d
}
func TestIntegrationRuntimeActualUniFiHTTPSDesiredAndManualGroups(t *testing.T) {
	server, calls, present := integrationController(t)
	s, path := integrationFixture(t, nil, nil)
	defer func() {
		if t.Failed() {
			for _, status := range s.integrations.host.Statuses() {
				t.Log("integration", status.Phase, status.Reason, status.Result.Clients, status.Result.Pending)
			}
			for _, kind := range []store.Kind{kinds.KindIntegrationBudget, kinds.KindIntegrationSourceHead} {
				rows, e := s.opts.Store.List(t.Context(), kind, store.ListOptions{})
				t.Log("private row count", kind, len(rows.Items), e != nil)
			}
			t.Log("HTTP calls", calls.Load())
		}
	}()
	group := &public.ClientGroup{ObjectMeta: metav1.ObjectMeta{Name: "devices", Namespace: "default"}, Spec: public.ClientGroupSpec{Clients: []public.ClientMatcher{{MAC: "00:00:00:00:00:09"}}, Sources: &public.ClientGroupSources{IntegrationRef: &public.ObjectRef{Name: "inventory"}}}}
	if _, err := s.opts.Store.Apply(t.Context(), group, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	d := integrationApply(t, s, integrationPinnedDefinition(t, server, path))
	integrationRun(t, s)
	awaitServingRuntime(t, s, func() bool {
		snapshot := integrationInventory(t, s)
		return snapshot != nil && snapshot.Lookup(netip.MustParseAddr("192.0.2.19"), time.Now()) != nil
	})
	if calls.Load() < 10 {
		t.Fatal("provider did not acquire complete authenticated observations", calls.Load())
	}
	list, err := s.opts.Store.List(t.Context(), "DNSRecordSet", store.ListOptions{Namespace: "default"})
	if err != nil || len(list.Items) != 1 {
		t.Fatal("source records not durable", err, len(list.Items))
	}
	record := list.Items[0].(*public.DNSRecordSet)
	if record.Spec.Endpoints[0].DNSName != "laptop.example." || record.Spec.Endpoints[0].Kudurru.ExpiresAt == nil || !sourceOwned(record, kinds.Owner{ClusterID: s.opts.Store.Info().ClusterID, Namespace: d.Namespace, Name: d.Name, UID: string(d.UID)}) {
		t.Fatal("record lost source or original expiry")
	}
	var manual public.ClientGroup
	if err = s.opts.Store.Get(t.Context(), store.KeyOf(group), &manual); err != nil || len(manual.Spec.Clients) != 1 {
		t.Fatal("private enrichment mutated manual group", err)
	}
	var lease *serving.Lease
	awaitServingRuntime(t, s, func() bool {
		var captureErr error
		lease, captureErr = s.publisher.CaptureHTTP(t.Context())
		return captureErr == nil
	})
	defer lease.Release()
	objects, _ := lease.Bundle().Component("objects")
	groups := objects.(*zonecache.Snapshot).Objects.ClientGroups()
	if len(groups) != 1 || len(groups[0].Spec.Clients) != 2 {
		t.Fatal("complete image lacks merged group contribution")
	}
	compiled, _ := lease.Bundle().Component("policy")
	client := pipeline.Client{Addr: netip.MustParseAddrPort("192.0.2.19:53"), IdentityCaptured: true, InventorySource: ih.SourceID(integrationOwner(s, d)), MACs: []string{"00:11:22:33:44:55"}}
	expires := record.Spec.Endpoints[0].Kudurru.ExpiresAt.Time
	ix := compiled.(*policy.Index)
	if !slices.Contains(ix.IdentifyAt(client, expires.Add(-time.Nanosecond)).GroupIDs, "default/devices") || slices.Contains(ix.IdentifyAt(client, expires).GroupIDs, "default/devices") {
		t.Fatal("private group did not retain its exact exclusive source expiry")
	}
	client.InventorySource = "another-source"
	if slices.Contains(ix.IdentifyAt(client, expires.Add(-time.Nanosecond)).GroupIDs, "default/devices") {
		t.Fatal("another source inherited private group membership")
	}
	client.MACs = []string{"00:00:00:00:00:09"}
	if !slices.Contains(ix.IdentifyAt(client, expires.Add(time.Hour)).GroupIDs, "default/devices") {
		t.Fatal("private source expiry removed manual membership")
	}
	value, _ := lease.Bundle().Component("zones")
	model := value.(*servingzones.Prepared).Zones(time.Now()).Match("laptop.example.")
	if model == nil || model.Lookup("laptop.example.", dns.TypeA).Kind != zone.Success {
		t.Fatal("acknowledged source record absent from complete DNS owner")
	}

	allocation, err := s.opts.Store.List(t.Context(), kinds.KindIntegrationNameAllocation, store.ListOptions{})
	if err != nil || len(allocation.Items) != 1 {
		t.Fatal("stable name reservation missing", err)
	}
	// Original proof data contains no resolved credential bytes.
	image, err := s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range image.Payload.Objects {
		if strings.Contains(string(raw), "test-only-private-credential") {
			t.Fatal("credential persisted in object image")
		}
	}
	lease.Release()
	integrationReadyCommand(t, s, integrationOwner(s, d))
	present.Store(false)
	result, err := s.integrations.host.Sync(t.Context(), integrationOwner(s, d), "")
	if err != nil || result.Source == nil || !result.Source.Committed || !result.Source.Published {
		if result != nil {
			t.Logf("omission source outcome: %+v", result.Source)
		}
		t.Fatalf("complete omission was not acknowledged: %v", err)
	}
	awaitServingRuntime(t, s, func() bool {
		snapshot := integrationInventory(t, s)
		return snapshot != nil && snapshot.Lookup(netip.MustParseAddr("192.0.2.19"), time.Now()) == nil
	})
	list, err = s.opts.Store.List(t.Context(), "DNSRecordSet", store.ListOptions{Namespace: "default"})
	if err != nil || len(list.Items) != 0 {
		t.Fatal("omission retained source-owned desired records", err != nil)
	}
	allocation, err = s.opts.Store.List(t.Context(), kinds.KindIntegrationNameAllocation, store.ListOptions{})
	if err != nil || len(allocation.Items) != 1 {
		t.Fatal("omission released permanent name reservation", err != nil)
	}
}

func TestIntegrationRuntimeRefusesDeleteAndUnlicensedExecution(t *testing.T) {
	for _, mode := range []string{"delete", "unlicensed"} {
		t.Run(mode, func(t *testing.T) {
			var polls atomic.Int32
			s, path := integrationFixture(t, func(o *servingRuntimeOptions) {
				if mode == "unlicensed" {
					var err error
					o.License, err = license.New(license.Options{})
					if err != nil {
						t.Fatal(err)
					}
				}
			}, func(string) (integration.Integration, error) {
				return &integrationProbe{observe: func(context.Context, ih.OwnerHost) (ih.SourceSnapshot, error) {
					polls.Add(1)
					return ih.SourceSnapshot{Complete: true}, nil
				}}, nil
			})
			definition := integrationDefinition("refused", path)
			if mode == "delete" {
				definition.Spec.DeletionPolicy = "Delete"
			}
			d := integrationApply(t, s, definition)
			integrationRun(t, s)
			awaitServingRuntime(t, s, func() bool {
				for _, status := range s.integrations.host.Statuses() {
					if mode == "unlicensed" && status.Reason == "LicenseUnavailable" || mode == "delete" && status.Reason == "ProvisionFailed" {
						return true
					}
				}
				return false
			})
			if polls.Load() != 0 {
				t.Fatal("unsupported/unlicensed instance acquired source authority")
			}
			integrationNoSource(t, s, integrationOwner(s, d))
			rows, err := s.opts.Store.List(t.Context(), kinds.KindIntegrationBudget, store.ListOptions{})
			if err != nil || len(rows.Items) != 0 {
				t.Fatal("refused instance performed provider request", err != nil)
			}
		})
	}
}
