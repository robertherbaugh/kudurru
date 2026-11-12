package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
)

// TestRunServeKubeCloudflareDeleteCleanupAndRelease mounts runServe on
// envtest with the local TLS Cloudflare peer (F09b-DEL-8, D367, D384). It
// shows: deletionPolicy Delete arms the reserved cleanup finalizer before any
// provider request (the instance is suspended until then and makes none);
// a server-side apply forced by another field manager keeps it; deleting the
// Integration leaves a terminating object, deletes exactly the record it
// published at the peer, forgets its ledger and operation Secrets, keeps the
// shared budget and removes the object; a second incarnation that cannot
// clean up (suspended) stays Terminating and is shown so by the API, and the
// reviewed release command removes it without cleanup, leaving its record
// and its ledger in place.
func TestRunServeKubeCloudflareDeleteCleanupAndRelease(t *testing.T) {
	env := newKubeIntegrationEnv(t)
	ctx := t.Context()
	peer := integrationCloudflareTLS(t)
	fixture := http.DefaultTransport.(*http.Transport)
	routed := fixture.Clone()
	toPeer := fixture.DialContext
	routed.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == "api.cloudflare.com:443" {
			return toPeer(ctx, network, address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	http.DefaultTransport = routed
	t.Cleanup(func() { http.DefaultTransport = fixture; routed.CloseIdleConnections() })
	credential := kubeTestCredential(t)
	for _, object := range []client.Object{
		&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "home"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: "example.", Nameservers: []string{"ns.example."}}},
		&public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "published", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "home"}, Endpoints: []public.Endpoint{{DNSName: "web.example.", RecordType: "A", RecordTTL: 300, Targets: []string{"192.0.2.80"}}}}},
	} {
		if err := env.admin.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	cfg := env.replica(t, "cleanup-a", newPartitionGate(t, env.rest), credential)
	_ = kubeIntegrationMount(t, cfg)
	counts := func() (requests, writes int, deleted []string) {
		peer.mu.Lock()
		defer peer.mu.Unlock()
		return peer.requests, peer.writes, append([]string(nil), peer.deleted...)
	}
	rowID := func() string {
		peer.mu.Lock()
		defer peer.mu.Unlock()
		for id, row := range peer.rows {
			if row["name"] == "web.example" || row["name"] == "web.example." {
				return id
			}
		}
		return ""
	}
	carriers := func(kind string) int {
		var list corev1.SecretList
		if err := env.admin.List(ctx, &list, client.InNamespace("default"), client.MatchingLabels{kube.InternalKindLabel: kind}); err != nil {
			t.Fatal(err)
		}
		return len(list.Items)
	}
	get := func(name string) (*public.Integration, error) {
		var d public.Integration
		err := env.admin.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &d)
		return &d, err
	}
	gone := func(name string) bool {
		_, err := get(name)
		return err != nil && strings.Contains(err.Error(), "not found")
	}
	definition := func(name string) *public.Integration {
		return &public.Integration{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: public.IntegrationSpec{Type: "cloudflare", Suspend: true, DeletionPolicy: public.IntegrationDeletionDelete, Resync: metav1.Duration{Duration: time.Hour}, Credentials: map[string]public.CredentialReference{"apiToken": {File: credential}}, Cloudflare: &public.CloudflareIntegration{Zones: []public.CloudflareZone{{Zone: "example", ZoneID: integrationCloudflareZone}}}}}
	}
	resume := func(name string) {
		t.Helper()
		kubeAwait(t, 30*time.Second, "resume "+name, func() bool {
			d, err := get(name)
			if err != nil {
				return false
			}
			d.Spec.Suspend = false
			return env.admin.Update(ctx, d) == nil
		})
	}

	// Arming precedes every provider request.
	if err := env.admin.Create(ctx, definition("publisher")); err != nil {
		t.Fatal(err)
	}
	kubeAwait(t, 60*time.Second, "the cleanup finalizer is armed", func() bool {
		d, err := get("publisher")
		return err == nil && store.HasFinalizer(d, store.IntegrationCleanupFinalizer)
	})
	if requests, _, _ := counts(); requests != 0 {
		t.Fatal("a provider request preceded arming", requests)
	}
	resume("publisher")
	kubeAwait(t, 60*time.Second, "the armed instance publishes", func() bool {
		d, err := get("publisher")
		c := meta.FindStatusCondition(d.Status.Conditions, "Ready")
		return err == nil && rowID() != "" && c != nil && c.Status == metav1.ConditionTrue
	})
	published := rowID()
	if carriers(kinds.KindIntegrationLedger) != 1 || carriers(kinds.KindIntegrationOperation) != 1 {
		t.Fatal("the publication did not write one ledger row and one operation")
	}

	// A forced server-side apply by another field manager that omits the
	// finalizer does not strip it.
	d, err := get("publisher")
	if err != nil {
		t.Fatal(err)
	}
	other := &public.Integration{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "Integration"}, ObjectMeta: metav1.ObjectMeta{Name: "publisher", Namespace: "default", Labels: map[string]string{"team": "dns"}}, Spec: d.Spec}
	raw, err := json.Marshal(other)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.admin.Patch(ctx, other, client.RawPatch(types.ApplyPatchType, raw), client.ForceOwnership, client.FieldOwner("someone-else")); err != nil {
		t.Fatal(err)
	}
	kubeAwait(t, 10*time.Second, "the forced apply landed", func() bool {
		d, err := get("publisher")
		return err == nil && d.Labels["team"] == "dns"
	})
	if d, err := get("publisher"); err != nil || !store.HasFinalizer(d, store.IntegrationCleanupFinalizer) {
		t.Fatal("a forced apply by another manager stripped the cleanup finalizer", err)
	}

	// Delete: terminating object, owned record deleted, ledger forgotten,
	// budget kept, object removed.
	_, writesBefore, _ := counts()
	if d, err = get("publisher"); err != nil {
		t.Fatal(err)
	}
	if err := env.admin.Delete(ctx, d); err != nil {
		t.Fatal(err)
	}
	if d, err := get("publisher"); err == nil && d.DeletionTimestamp == nil {
		t.Fatal("the delete did not leave a terminating object")
	}
	kubeAwait(t, 90*time.Second, "the cleanup deletes the record and removes the object", func() bool { return gone("publisher") })
	_, writes, deleted := counts()
	if rowID() != "" || len(deleted) != 1 || deleted[0] != published || writes != writesBefore+1 {
		t.Fatal("cleanup did not delete exactly the published record", deleted, writes-writesBefore)
	}
	if carriers(kinds.KindIntegrationLedger) != 0 || carriers(kinds.KindIntegrationOperation) != 0 {
		t.Fatal("cleanup left ledger or operation Secrets", carriers(kinds.KindIntegrationLedger), carriers(kinds.KindIntegrationOperation))
	}
	if carriers(kinds.KindIntegrationBudget) != 1 {
		t.Fatal("the shared budget was removed")
	}

	// A second incarnation that cannot clean up stays Terminating, the API
	// shows it, and the reviewed release removes it without cleanup.
	if err := env.admin.Create(ctx, definition("second")); err != nil {
		t.Fatal(err)
	}
	kubeAwait(t, 60*time.Second, "the second incarnation is armed", func() bool {
		d, err := get("second")
		return err == nil && store.HasFinalizer(d, store.IntegrationCleanupFinalizer)
	})
	resume("second")
	kubeAwait(t, 60*time.Second, "the second incarnation publishes", func() bool { return rowID() != "" && carriers(kinds.KindIntegrationLedger) == 1 })
	kubeAwait(t, 30*time.Second, "suspend second", func() bool {
		d, err := get("second")
		if err != nil {
			return false
		}
		d.Spec.Suspend = true
		return env.admin.Update(ctx, d) == nil
	})
	time.Sleep(2 * time.Second)
	if d, err = get("second"); err != nil {
		t.Fatal(err)
	}
	if err := env.admin.Delete(ctx, d); err != nil {
		t.Fatal(err)
	}
	var summary map[string]any
	kubeAwait(t, 30*time.Second, "the API shows the terminating instance", func() bool {
		status, body := env.api(t, cfg, http.MethodGet, "/integrations/default/second", nil)
		summary = body
		return status == http.StatusOK && body["terminating"] == true && body["deletionPolicy"] == public.IntegrationDeletionDelete && body["statusKnown"] == true && body["phase"] == ih.PhaseTerminating
	})
	time.Sleep(3 * time.Second)
	if gone("second") || rowID() == "" {
		t.Fatal("a suspended cleanup removed the object or its record")
	}
	if d, err = get("second"); err != nil {
		t.Fatal(err)
	}
	status, body := env.api(t, cfg, http.MethodPost, "/integrations/default/second/release", map[string]any{"uid": string(d.UID), "revision": "1"})
	if status != http.StatusConflict {
		t.Fatal("a stale release review was not a conflict", status, body)
	}
	released := false
	for attempt := 0; attempt < 20 && !released; attempt++ {
		if d, err = get("second"); err != nil {
			t.Fatal(err)
		}
		status, body = env.api(t, cfg, http.MethodPost, "/integrations/default/second/release", map[string]any{"uid": string(d.UID), "revision": d.ResourceVersion})
		switch status {
		case http.StatusOK:
			released = true
		case http.StatusConflict:
			time.Sleep(200 * time.Millisecond)
		default:
			t.Fatal("release", status, body)
		}
	}
	if !released || body["released"] != true || body["ledgerRows"] != float64(1) {
		raw, _ := json.Marshal(summary)
		t.Fatal("release answer", status, body, string(raw))
	}
	kubeAwait(t, 10*time.Second, "the released object is gone", func() bool { return gone("second") })
	time.Sleep(2 * time.Second)
	if rowID() == "" || carriers(kinds.KindIntegrationLedger) != 1 {
		t.Fatal("release did not retain the record and the ledger")
	}
}
