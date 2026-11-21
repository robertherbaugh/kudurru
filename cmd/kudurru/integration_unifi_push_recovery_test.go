package main

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/store"
)

type unifiRecoveryFixture struct {
	*integrationOperationFixture
	peer     *unifiPushPeer
	router   http.Handler
	ids      []string
	baseline map[string]map[string]any
	foreign  map[string]map[string]any
	review   api.IntegrationReview
}

func newUniFiRecoveryFixture(test *testing.T, action string) *unifiRecoveryFixture {
	test.Helper()
	peer := newUniFiPushPeer(test)
	record := unifiPushRecord("web", "A", "192.0.2.80")
	record.Spec.Endpoints[0].Targets = append(record.Spec.Endpoints[0].Targets, "192.0.2.81")
	operation, router := unifiPushStart(test, peer, []*public.DNSRecordSet{record}, func(definition *public.Integration) {
		definition.Spec.UniFi.Push.MirrorRecords = public.UniFiRecordMirror{Enabled: true, Domains: []string{"example"}, Types: []string{"A"}}
	})
	fixture := &unifiRecoveryFixture{integrationOperationFixture: operation, peer: peer, router: router, baseline: map[string]map[string]any{}, foreign: map[string]map[string]any{}}
	review := unifiPushReview(test, operation, router)
	preview := unifiPushCall(test, operation, router, "/preview", review, http.StatusOK).Preview
	review = unifiPushReview(test, operation, router)
	review.Mode, review.PreviewDigest = "publish", preview.Digest
	if result := unifiPushCall(test, operation, router, "/sync", review, http.StatusOK); result.Applied != 1 || len(peer.written()) != 2 {
		test.Fatal("failed to establish the owned two-policy baseline", result, peer.written())
	}
	peer.mu.Lock()
	for id, policy := range peer.policies {
		if policy["domain"] != "web.example" {
			fixture.foreign[id] = maps.Clone(policy)
			continue
		}
		fixture.ids = append(fixture.ids, id)
		fixture.baseline[id] = maps.Clone(policy)
	}
	peer.mu.Unlock()
	slices.Sort(fixture.ids)
	if len(fixture.ids) != 2 || len(fixture.foreign) != 2 {
		test.Fatal("unexpected starting policies", fixture.ids, fixture.foreign)
	}
	if action == "update" {
		peer.mu.Lock()
		for index, address := range []string{"192.0.2.90", "192.0.2.91"} {
			fixture.baseline[fixture.ids[index]]["ipv4Address"] = address
			peer.policies[fixture.ids[index]]["ipv4Address"] = address
		}
		peer.mu.Unlock()
	}
	if action == "delete" {
		if err := operation.s.opts.Store.Delete(test.Context(), store.KeyOf(record), store.DeleteOptions{Manager: store.ManagerAPI}); err != nil {
			test.Fatal(err)
		}
		unifiPushCapturedWithout(test, operation, record.Name)
	}
	review = unifiPushReview(test, operation, router)
	preview = unifiPushCall(test, operation, router, "/preview", review, http.StatusOK).Preview
	if len(preview.Operations) != 1 || preview.Operations[0].Action != action {
		test.Fatalf("missing %s preview: %+v", action, preview)
	}
	fixture.review = unifiPushReview(test, operation, router)
	fixture.review.Mode, fixture.review.PreviewDigest = "publish", preview.Digest
	return fixture
}

func (fixture *unifiRecoveryFixture) assertWrites(test *testing.T, method string) {
	test.Helper()
	path := "/proxy/network/integration/v1/sites/" + unifiPushSite + "/dns/policies"
	want := []string{"POST " + path, "POST " + path, method + " " + path + "/" + fixture.ids[0], method + " " + path + "/" + fixture.ids[1]}
	if got := fixture.peer.written(); !slices.Equal(got, want) {
		test.Fatalf("provider writes: got %v, want %v", got, want)
	}
	fixture.peer.mu.Lock()
	defer fixture.peer.mu.Unlock()
	for id, policy := range fixture.foreign {
		if !reflect.DeepEqual(fixture.peer.policies[id], policy) {
			test.Fatalf("foreign policy %s changed", id)
		}
	}
}

func (fixture *unifiRecoveryFixture) assertPoll(test *testing.T, pending int) {
	test.Helper()
	unifiPushReview(test, fixture.integrationOperationFixture, fixture.router)
	result, err := fixture.s.integrations.host.Sync(test.Context(), integrationOwner(fixture.s, fixture.d), "")
	if err != nil || result == nil || result.Pending != pending || result.Applied != 0 || result.Source == nil {
		test.Fatalf("poll with %d pending: result %+v, error %v", pending, result, err)
	}
}

func (fixture *unifiRecoveryFixture) assertStaleReview(test *testing.T) {
	test.Helper()
	review := unifiPushReview(test, fixture.integrationOperationFixture, fixture.router)
	review.Mode, review.PreviewDigest = fixture.review.Mode, fixture.review.PreviewDigest
	unifiPushCall(test, fixture.integrationOperationFixture, fixture.router, "/sync", review, http.StatusConflict)
}

func (fixture *unifiRecoveryFixture) ledgerRow(test *testing.T) *kinds.IntegrationLedger {
	test.Helper()
	rows, err := fixture.s.opts.Store.List(test.Context(), kinds.KindIntegrationLedger, store.ListOptions{})
	if err != nil || len(rows.Items) != 1 {
		test.Fatalf("expected one ledger row: %+v, error %v", rows, err)
	}
	return rows.Items[0].(*kinds.IntegrationLedger)
}

func (fixture *unifiRecoveryFixture) assertSettled(test *testing.T, reserved *kinds.IntegrationLedger, restored bool) {
	test.Helper()
	current := fixture.ledgerRow(test)
	if current.Spec.Pending != nil || current.Spec.Sequence != reserved.Spec.Sequence+1 {
		test.Fatalf("intent did not settle exactly once: before %+v, after %+v", reserved.Spec, current.Spec)
	}
	var op kinds.IntegrationOperation
	name := kinds.OperationName(reserved.Spec.Pending.OperationID)
	if err := fixture.s.opts.Store.Get(test.Context(), store.Key{Kind: kinds.KindIntegrationOperation, Name: name}, &op); err != nil {
		test.Fatal(err)
	}
	if restored {
		if op.Spec.AbandonedAt.IsZero() || current.Spec.ContentHash != reserved.Spec.ContentHash || !slices.Equal(current.Spec.ProviderIDs, reserved.Spec.ProviderIDs) {
			test.Fatalf("baseline was not preserved: ledger %+v, operation %+v", current.Spec, op.Spec)
		}
	} else if !op.Spec.AbandonedAt.IsZero() || op.Spec.Receipt == nil || !op.Spec.Receipt.Complete || current.Spec.LastOperationID != op.Spec.OperationID || current.Spec.ContentHash != op.Spec.AfterHash {
		test.Fatalf("after-state was not confirmed: ledger %+v, operation %+v", current.Spec, op.Spec)
	} else if op.Spec.After == nil && len(current.Spec.ProviderIDs) != 0 || op.Spec.After != nil && !slices.Equal(current.Spec.ProviderIDs, fixture.ids) {
		test.Fatal("confirmed state recorded unexpected provider IDs", current.Spec.ProviderIDs)
	}
}

func TestIntegrationUniFiPushMirrorPartialUpdateDelete(test *testing.T) {
	for _, tc := range []struct {
		name, action, method string
		restore, dropAck     bool
	}{
		{name: "update/operator-completion", action: "update", method: http.MethodPut},
		{name: "update/baseline-restore", action: "update", method: http.MethodPut, restore: true},
		{name: "update/lost-acknowledgment-and-reread", action: "update", method: http.MethodPut, dropAck: true},
		{name: "delete/operator-completion", action: "delete", method: http.MethodDelete},
		{name: "delete/lost-acknowledgment-and-reread-restart", action: "delete", method: http.MethodDelete, dropAck: true},
	} {
		test.Run(tc.name, func(test *testing.T) {
			fixture := newUniFiRecoveryFixture(test, tc.action)
			fixture.peer.mu.Lock()
			fixture.peer.policyFault = unifiPolicyFault{method: tc.method, at: 2, dropAckAndRead: tc.dropAck}
			fixture.peer.mu.Unlock()
			response := integrationAdminCall(test, fixture.router, fixture.integrationOperationFixture, http.MethodPost, "/integrations/default/gateway/sync", fixture.review, fixture.token)
			if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"key":"integration_uncertain"`) || !strings.Contains(response.Body.String(), `"pending":1`) {
				test.Fatalf("expected an uncertain intent: %d %s", response.Code, response.Body.String())
			}
			reserved := fixture.ledgerRow(test)
			if reserved.Spec.Pending == nil {
				test.Fatal("sync did not persist the pending intent")
			}
			fixture.assertWrites(test, tc.method)
			if !tc.dropAck {
				fixture.assertStaleReview(test)
				fixture.assertPoll(test, 1)
				fixture.assertWrites(test, tc.method)
				fixture.finishPolicyChange(test, tc.action, tc.restore)
			}
			restart := tc.dropAck && tc.action == "delete"
			if restart {
				fixture.assertStaleReview(test)
				fixture.stop()
				if current := fixture.ledgerRow(test); current.ResourceVersion != reserved.ResourceVersion || current.Spec.Pending == nil {
					test.Fatal("original writer did not leave the same intent pending on disk")
				}
				webhookRecordsRestart(test, fixture.integrationOperationFixture)
				owner := integrationOwner(fixture.s, fixture.d)
				awaitServingRuntime(test, fixture.s, func() bool {
					for _, status := range fixture.s.integrations.host.Statuses() {
						if status.Owner == owner && status.Result.Source != nil && status.Result.Source.Published {
							return webhookRecordsPendingIn(test, fixture.s.opts.Store) == 0
						}
					}
					return false
				})
			} else {
				fixture.assertPoll(test, 0)
			}
			fixture.assertSettled(test, reserved, tc.restore)
			if !restart {
				fixture.assertStaleReview(test)
				fixture.assertPoll(test, 0)
				fixture.assertSettled(test, reserved, tc.restore)
			}
			fixture.assertWrites(test, tc.method)
		})
	}
}

func (fixture *unifiRecoveryFixture) finishPolicyChange(test *testing.T, action string, restore bool) {
	test.Helper()
	const foreign = "77777777-7777-4777-8777-777777777777"
	fixture.peer.mu.Lock()
	first, second := fixture.peer.policies[fixture.ids[0]], fixture.peer.policies[fixture.ids[1]]
	partial := false
	switch {
	case action == "delete":
		partial = first == nil && reflect.DeepEqual(second, fixture.baseline[fixture.ids[1]])
		delete(fixture.peer.policies, fixture.ids[1])
	case restore:
		partial = first["ipv4Address"] == "192.0.2.80" && reflect.DeepEqual(second, fixture.baseline[fixture.ids[1]])
		fixture.peer.policies[fixture.ids[0]] = maps.Clone(fixture.baseline[fixture.ids[0]])
	default:
		partial = first["ipv4Address"] == "192.0.2.80" && reflect.DeepEqual(second, fixture.baseline[fixture.ids[1]])
		second["ipv4Address"] = "192.0.2.81"
	}
	foreignPolicy := maps.Clone(fixture.baseline[fixture.ids[1]])
	var replaced map[string]any
	if action == "update" {
		replaced = fixture.peer.policies[fixture.ids[1]]
		foreignPolicy = maps.Clone(replaced)
		delete(fixture.peer.policies, fixture.ids[1])
	}
	foreignPolicy["id"] = foreign
	fixture.peer.policies[foreign] = maps.Clone(foreignPolicy)
	fixture.peer.mu.Unlock()
	if !partial {
		test.Fatal("only the first policy should have changed")
	}
	fixture.assertPoll(test, 1)
	fixture.peer.mu.Lock()
	untouched := reflect.DeepEqual(fixture.peer.policies[foreign], foreignPolicy)
	delete(fixture.peer.policies, foreign)
	if replaced != nil {
		fixture.peer.policies[fixture.ids[1]] = replaced
	}
	fixture.peer.mu.Unlock()
	if !untouched {
		test.Fatal("poll changed a foreign policy at the same name")
	}
}

func unifiHeldCommand(test *testing.T, fixture *unifiRecoveryFixture, command string, fault unifiPolicyFault) (*httptest.ResponseRecorder, <-chan struct{}, func()) {
	test.Helper()
	hold, entered := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(hold) }) }
	fault.hold, fault.entered = hold, entered
	fixture.peer.mu.Lock()
	fixture.peer.policyFault = fault
	fixture.peer.mu.Unlock()
	payload, err := json.Marshal(fixture.review)
	if err != nil {
		test.Fatal(err)
	}
	req := httptest.NewRequestWithContext(test.Context(), http.MethodPost, "https://kudurru.test/api/v1/integrations/default/gateway/"+command, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+fixture.token)
	response, done := httptest.NewRecorder(), make(chan struct{})
	test.Cleanup(func() {
		release()
		integrationAwaitSignal(test, done)
	})
	go func() {
		defer close(done)
		fixture.router.ServeHTTP(response, req)
	}()
	integrationAwaitSignal(test, entered)
	return response, done, release
}

func TestIntegrationUniFiPushMirrorWriterLeaseLoss(test *testing.T) {
	fixture := newUniFiRecoveryFixture(test, "update")
	response, done, release := unifiHeldCommand(test, fixture, "sync", unifiPolicyFault{method: http.MethodPut, at: 2})
	defer release()
	reserved := fixture.ledgerRow(test)
	if reserved.Spec.Pending == nil {
		test.Fatal("no durable intent while the final PUT is in flight")
	}
	stopped := make(chan struct{})
	stop := fixture.stop
	go func() {
		defer close(stopped)
		stop()
	}()
	integrationAwaitSignal(test, stopped)
	integrationAwaitSignal(test, done)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"key":"integration_uncertain"`) {
		test.Fatalf("original sync did not report lease loss as uncertain: %d %s", response.Code, response.Body.String())
	}
	if current := fixture.ledgerRow(test); current.ResourceVersion != reserved.ResourceVersion {
		test.Fatal("the lost lease changed the ledger")
	}
	var op kinds.IntegrationOperation
	if err := fixture.s.opts.Store.Get(test.Context(), store.Key{Kind: kinds.KindIntegrationOperation, Name: kinds.OperationName(reserved.Spec.Pending.OperationID)}, &op); err != nil || op.Spec.Receipt != nil {
		test.Fatalf("the lost lease wrote a receipt: %+v, error %v", op.Spec.Receipt, err)
	}
	release()
	fixture.stop = integrationWriter(test, fixture.s)
	fixture.assertPoll(test, 0)
	fixture.assertSettled(test, reserved, false)
	fixture.assertStaleReview(test)
	fixture.assertPoll(test, 0)
	fixture.assertSettled(test, reserved, false)
	fixture.assertWrites(test, http.MethodPut)
}

func TestIntegrationUniFiPushMirrorDeletedSourceReview(test *testing.T) {
	fixture := newUniFiRecoveryFixture(test, "update")
	fixture.review.Mode, fixture.review.PreviewDigest = "", ""
	response, done, release := unifiHeldCommand(test, fixture, "preview", unifiPolicyFault{method: http.MethodGet})
	defer release()
	if err := fixture.s.opts.Store.Delete(test.Context(), store.Key{Kind: "DNSRecordSet", Namespace: "default", Name: "web"}, store.DeleteOptions{Manager: store.ManagerAPI}); err != nil {
		test.Fatal(err)
	}
	release()
	integrationAwaitSignal(test, done)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"key":"conflict"`) {
		test.Fatalf("captured preview of deleted source: %d %s", response.Code, response.Body.String())
	}
	if len(fixture.peer.written()) != 2 {
		test.Fatal("stale preview reached UniFi as a write", fixture.peer.written())
	}
}
