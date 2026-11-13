package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
)

// kubeCloudflarePeer starts the local TLS Cloudflare peer and routes only
// the Cloudflare origin to it, so the replicas' plain-HTTP Kubernetes clients
// keep reaching the API gate through the default transport.
func kubeCloudflarePeer(t *testing.T) *integrationCloudflarePeer {
	t.Helper()
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
	return peer
}

// kubePeerTarget returns the content of the peer's row named name.
func kubePeerTarget(peer *integrationCloudflarePeer, name string) string {
	peer.mu.Lock()
	defer peer.mu.Unlock()
	for _, row := range peer.rows {
		if row["name"] == name || row["name"] == name+"." {
			content, _ := row["content"].(string)
			return content
		}
	}
	return ""
}

func kubePeerCounts(peer *integrationCloudflarePeer) (requests, writes int) {
	peer.mu.Lock()
	defer peer.mu.Unlock()
	return peer.requests, peer.writes
}

// TestRunServeKubeCloudflareConflictMidFlightReviewAndRotation closes what
// verification left open in the Kubernetes root (D384), through runServe on
// envtest and the local TLS Cloudflare peer:
//   - a refused Integration in another namespace has no credential
//     fingerprint written to its status (its references are not resolved);
//   - a source record set that changes while a sync is in flight (its record
//     listing held at the peer) fences the planned effect: the stale content
//     is never sent, and the new content is published by exactly one write;
//   - a duplicated admitted record set halts the publisher with
//     SourceConflicted and a message naming it, without a provider write,
//     and publication resumes once it is removed;
//   - a reviewed preview that went stale (the remote changed after it) is
//     refused with 409 and no write, at the same revision a fresh preview
//     then syncs at;
//   - rotating the allowlisted credential file replaces the provider: once a
//     request carries the new credential, no request carries the old one.
func TestRunServeKubeCloudflareConflictMidFlightReviewAndRotation(t *testing.T) {
	env := newKubeIntegrationEnv(t)
	ctx := t.Context()
	peer := kubeCloudflarePeer(t)
	credential := kubeTestCredential(t)
	for _, object := range []client.Object{
		&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "home"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: "example.", Nameservers: []string{"ns.example."}}},
		&public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "published", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "home"}, Endpoints: []public.Endpoint{{DNSName: "web.example.", RecordType: "A", RecordTTL: 300, Targets: []string{"192.0.2.80"}}}}},
	} {
		if err := env.admin.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	gate := newPartitionGate(t, env.rest)
	cfg := env.replica(t, "open-a", gate, credential)
	_ = kubeIntegrationMount(t, cfg)
	kubeAwait(t, 60*time.Second, "the replica answers the API", func() bool {
		status, _ := env.api(t, cfg, http.MethodGet, "/integrations?namespace=default", nil)
		return status == http.StatusOK
	})
	readyIn := func(namespace, name string) (*metav1.Condition, *public.Integration) {
		var d public.Integration
		if err := env.admin.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &d); err != nil {
			return nil, nil
		}
		return meta.FindStatusCondition(d.Status.Conditions, "Ready"), &d
	}
	ready := func() (*metav1.Condition, *public.Integration) { return readyIn("default", "publisher") }
	target := func() string { return kubePeerTarget(peer, "web.example") }
	setTarget := func(name, address string) {
		t.Helper()
		var record public.DNSRecordSet
		if err := env.admin.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &record); err != nil {
			t.Fatal(err)
		}
		record.Spec.Endpoints[0].Targets = []string{address}
		if err := env.admin.Update(ctx, &record); err != nil {
			t.Fatal(err)
		}
	}
	publisher := &public.Integration{ObjectMeta: metav1.ObjectMeta{Name: "publisher", Namespace: "default"}, Spec: public.IntegrationSpec{Type: "cloudflare", Resync: metav1.Duration{Duration: time.Hour}, Credentials: map[string]public.CredentialReference{"apiToken": {File: credential}}, Cloudflare: &public.CloudflareIntegration{Zones: []public.CloudflareZone{{Zone: "example", ZoneID: integrationCloudflareZone}}}}}
	if err := env.admin.Create(ctx, publisher); err != nil {
		t.Fatal(err)
	}
	kubeAwait(t, 60*time.Second, "the Cloudflare instance publishes and reports Ready", func() bool {
		c, _ := ready()
		return target() == "192.0.2.80" && c != nil && c.Status == metav1.ConditionTrue
	})
	if _, writes := kubePeerCounts(peer); writes != 1 {
		t.Fatal("first publication was not exactly one mutation", writes)
	}

	// (1) A refused Integration in another namespace naming the allowlisted
	// credential gets no fingerprint of it; the accepted publisher does.
	if err := env.admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant"}}); err != nil {
		t.Fatal(err)
	}
	foreign := publisher.DeepCopy()
	foreign.ObjectMeta = metav1.ObjectMeta{Name: "foreign", Namespace: "tenant"}
	if err := env.admin.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	kubeAwait(t, 60*time.Second, "the foreign Integration is refused on its object", func() bool {
		c, _ := readyIn("tenant", "foreign")
		return c != nil && c.Status == metav1.ConditionFalse && c.Reason == reasonNamespaceNotAllowed
	})
	if _, d := readyIn("tenant", "foreign"); len(d.Status.CredentialFingerprints) != 0 {
		t.Fatal("a refused foreign Integration has the allowlisted credential's fingerprint", len(d.Status.CredentialFingerprints))
	}
	_, own := ready()
	if len(own.Status.CredentialFingerprints) != 1 {
		t.Fatal("the publisher lost its own credential fingerprint", len(own.Status.CredentialFingerprints))
	}
	fingerprint := own.Status.CredentialFingerprints["apiToken"]

	// (2) A source change while a sync is in flight. The sync for .83 is
	// held at its record listing; the source moves to .84 meanwhile. The
	// plan for .83 is fenced at its effect and never sent.
	entered, release := make(chan struct{}), make(chan struct{})
	peer.mu.Lock()
	peer.holdRead, peer.readEntered = release, entered
	peer.mu.Unlock()
	_, writesBefore := kubePeerCounts(peer)
	setTarget("published", "192.0.2.83")
	select {
	case <-entered:
	case <-time.After(60 * time.Second):
		t.Fatal("the source change did not start a sync")
	}
	setTarget("published", "192.0.2.84")
	// Let the replica's watch deliver the second change before the held
	// sync continues.
	time.Sleep(3 * time.Second)
	close(release)
	kubeAwait(t, 60*time.Second, "the changed source is published", func() bool { return target() == "192.0.2.84" })
	time.Sleep(3 * time.Second)
	peer.mu.Lock()
	contents := slices.Clone(peer.contents)
	writes := peer.writes
	peer.mu.Unlock()
	if slices.Contains(contents, "192.0.2.83") {
		t.Fatal("the plan of the superseded source was sent", contents)
	}
	if writes-writesBefore != 1 {
		t.Fatal("the changed source was not published by exactly one write", writes-writesBefore)
	}

	// (3) A duplicated admitted record set halts the publisher with a reason
	// on its object and no provider write; removing it resumes publication.
	_, writesBefore = kubePeerCounts(peer)
	shadow := &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "shadow", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "home"}, Endpoints: []public.Endpoint{{DNSName: "web.example.", RecordType: "A", RecordTTL: 300, Targets: []string{"192.0.2.99"}}}}}
	if err := env.admin.Create(ctx, shadow); err != nil {
		t.Fatal(err)
	}
	kubeAwait(t, 60*time.Second, "the publisher reports the source conflict", func() bool {
		c, _ := ready()
		return c != nil && c.Status == metav1.ConditionFalse && c.Reason == reasonSourceConflicted && strings.Contains(c.Message, "default/") && strings.Contains(c.Message, "(Duplicated)")
	})
	time.Sleep(3 * time.Second)
	if _, w := kubePeerCounts(peer); w != writesBefore {
		t.Fatal("a conflicted source reached the provider", w-writesBefore)
	}
	if got := target(); got != "192.0.2.84" {
		t.Fatal("the remote record changed during the conflict", got)
	}
	if err := env.admin.Delete(ctx, shadow); err != nil {
		t.Fatal(err)
	}
	kubeAwait(t, 60*time.Second, "the publisher resumes after the conflict is removed", func() bool {
		c, _ := ready()
		return c != nil && c.Status == metav1.ConditionTrue
	})
	time.Sleep(2 * time.Second)
	if _, w := kubePeerCounts(peer); w != writesBefore || target() != "192.0.2.84" {
		t.Fatal("resuming after the conflict changed the remote", w-writesBefore, target())
	}

	// (4) A stale reviewed preview. The remote drifts (its rows are cleared,
	// which wakes nothing), a preview reviews the repair, the remote returns
	// to its former state, and a publish sync with that preview's digest is
	// refused before any write. A fresh preview at the same revision syncs.
	current := func() *public.Integration {
		_, d := ready()
		if d == nil {
			t.Fatal("publisher not readable")
		}
		return d
	}
	call := func(path string, body map[string]any) (int, map[string]any) {
		t.Helper()
		return env.api(t, cfg, http.MethodPost, "/integrations/default/publisher/"+path, body)
	}
	digestOf := func(body map[string]any) (string, int) {
		preview, _ := body["preview"].(map[string]any)
		digest, _ := preview["digest"].(string)
		operations, _ := preview["operations"].([]any)
		return digest, len(operations)
	}
	peer.mu.Lock()
	saved := map[string]map[string]any{}
	for id, row := range peer.rows {
		saved[id] = row
	}
	peer.rows = map[string]map[string]any{}
	_, writesBefore = peer.requests, peer.writes
	peer.mu.Unlock()
	preview := func() (string, int) {
		t.Helper()
		for attempt := 0; ; attempt++ {
			d := current()
			status, body := call("preview", map[string]any{"uid": string(d.UID), "revision": d.ResourceVersion})
			if status == http.StatusConflict && attempt < 20 {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			digest, operations := digestOf(body)
			if status != http.StatusOK || !kinds.ValidDigest(digest) {
				t.Fatalf("preview: %d %v", status, body)
			}
			return digest, operations
		}
	}
	staleDigest, operations := preview()
	if operations == 0 {
		t.Fatal("the drift preview reviewed no operation")
	}
	peer.mu.Lock()
	peer.rows = saved
	peer.mu.Unlock()
	freshDigest, _ := preview()
	if freshDigest == staleDigest {
		t.Fatal("the preview did not change with the remote")
	}
	// The stale sync and the fresh one must be sent at the same revision,
	// so the stale one's 409 can only be the stale preview, not a moved
	// revision. A status write between them makes the test try again.
	proven := false
	for attempt := 0; attempt < 20 && !proven; attempt++ {
		d := current()
		status, body := call("sync", map[string]any{"uid": string(d.UID), "revision": d.ResourceVersion, "mode": "publish", "previewDigest": staleDigest})
		if _, w := kubePeerCounts(peer); status != http.StatusConflict || w != writesBefore {
			t.Fatalf("a stale reviewed preview was not refused before effects: %d %v writes=%d", status, body, w-writesBefore)
		}
		if again := current(); again.ResourceVersion != d.ResourceVersion {
			continue
		}
		status, body = call("sync", map[string]any{"uid": string(d.UID), "revision": d.ResourceVersion, "mode": "publish", "previewDigest": freshDigest})
		if status == http.StatusConflict {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if status != http.StatusOK || body["completed"] != true {
			t.Fatalf("the fresh reviewed sync was refused: %d %v", status, body)
		}
		proven = true
	}
	if !proven {
		t.Fatal("no stable revision for the stale and fresh reviewed syncs")
	}
	if _, w := kubePeerCounts(peer); w != writesBefore {
		t.Fatal("an in-sync reviewed sync wrote", w-writesBefore)
	}

	// (5) Credential rotation. The peer accepts both credentials while the
	// file is replaced. A sync of the old provider is held at its record
	// listing across the rotation: the old provider's request must be
	// canceled, and the old provider joined, before the new credential's
	// first request; after that request none uses the old credential.
	const rotated = "test-only-rotated-credential"
	entered, release = make(chan struct{}), make(chan struct{})
	peer.mu.Lock()
	peer.accept = []string{"test-only-private-credential", rotated}
	peer.holdRead, peer.readEntered = release, entered
	peer.mu.Unlock()
	setTarget("published", "192.0.2.85")
	select {
	case <-entered:
	case <-time.After(60 * time.Second):
		t.Fatal("the source change did not start a sync of the old provider")
	}
	staged := filepath.Join(filepath.Dir(credential), "credential.next")
	if err := os.WriteFile(staged, []byte(rotated), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staged, credential); err != nil {
		t.Fatal(err)
	}
	kubeAwait(t, 60*time.Second, "the held request of the old provider is canceled", func() bool {
		peer.mu.Lock()
		defer peer.mu.Unlock()
		return peer.readCanceled
	})
	close(release)
	kubeAwait(t, 60*time.Second, "the rotated credential replaces the provider", func() bool {
		c, d := ready()
		return c != nil && c.Status == metav1.ConditionTrue && d.Status.CredentialFingerprints["apiToken"] != "" && d.Status.CredentialFingerprints["apiToken"] != fingerprint
	})
	setTarget("published", "192.0.2.86")
	kubeAwait(t, 60*time.Second, "the rotated provider publishes", func() bool { return target() == "192.0.2.86" })
	peer.mu.Lock()
	tokens := slices.Clone(peer.tokens)
	peer.mu.Unlock()
	first := slices.Index(tokens, rotated)
	if first < 0 {
		t.Fatal("no request used the rotated credential")
	}
	peer.mu.Lock()
	canceledAt := peer.readCanceledAt
	peer.mu.Unlock()
	if first < canceledAt {
		t.Fatalf("the rotated provider sent request %d before the old provider's held request was canceled (at %d)", first, canceledAt)
	}
	for i := first; i < len(tokens); i++ {
		if tokens[i] != rotated {
			t.Fatalf("request %d used the old credential after the rotated provider started (first rotated request %d of %d)", i, first, len(tokens))
		}
	}
}

// TestRunServeKubeCloudflareTwoReplicasShareBudget mounts two replicas,
// each holding the Leases of half of two or four Cloudflare instances that
// publish to as many zones of the same account. (Two instances on one zone
// each count the other's owned records as conflicts; that is provider
// behavior on either store and not what this test is about.) Every instance
// publishes exactly once, and every request either replica sent was reserved
// in the one shared budget Secret first (D384: the budget is one
// exact-resourceVersion Secret; a lost reservation is attempted again, fenced,
// until the request's own deadline, and is never sent unreserved). Four
// instances reserving together lose the budget CAS many times in a row; a
// fixed bound of four attempts left one of them inert.
func TestRunServeKubeCloudflareTwoReplicasShareBudget(t *testing.T) {
	for _, n := range []int{2, 4} {
		t.Run(fmt.Sprintf("instances=%d", n), func(t *testing.T) { testKubeCloudflareReplicasShareBudget(t, n) })
	}
}

func testKubeCloudflareReplicasShareBudget(t *testing.T, n int) {
	env := newKubeIntegrationEnv(t)
	ctx := t.Context()
	peer := kubeCloudflarePeer(t)
	names := []string{"one", "two", "three", "four"}[:n]
	zoneIDs := map[string]string{}
	peer.mu.Lock()
	peer.zones = map[string]string{}
	for i, name := range names {
		zoneIDs[name] = strings.Repeat(string(rune('3'+i)), 32)
		peer.zones[zoneIDs[name]] = name + ".example"
	}
	peer.mu.Unlock()
	credential := kubeTestCredential(t)
	objects := []client.Object{&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "home"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: "example.", Nameservers: []string{"ns.example."}}}}
	for _, name := range names {
		objects = append(objects, &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: map[string]string{"publish": name}}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "home"}, Endpoints: []public.Endpoint{{DNSName: name + ".example.", RecordType: "A", RecordTTL: 300, Targets: []string{"192.0.2.1"}}}}})
	}
	// Place each instance's Lease on its replica: a Lease held by a
	// replica's own identity is renewed by it and waited on by the other.
	holders := map[string]string{}
	for i, name := range names {
		holders[name] = []string{"budget-a", "budget-b"}[i%2]
	}
	leaseName := func(name string) string {
		return kube.DefaultLeasePrefix + ih.ElectionKey(ih.InstanceID{Namespace: "default", Name: name})
	}
	for name, holder := range holders {
		now, duration := metav1.NewMicroTime(time.Now()), int32(60)
		objects = append(objects, &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: leaseName(name), Namespace: "default"}, Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &duration, AcquireTime: &now, RenewTime: &now}})
	}
	for _, object := range objects {
		if err := env.admin.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"budget-a", "budget-b"} {
		gate := newPartitionGate(t, env.rest)
		cfg := env.replica(t, name, gate, credential)
		// Start each replica before the next configuration: the test
		// configuration sets the process-wide KUBECONFIG.
		_ = kubeIntegrationMount(t, cfg)
		kubeAwait(t, 60*time.Second, name+" answers the API", func() bool {
			status, _ := env.api(t, cfg, http.MethodGet, "/integrations?namespace=default", nil)
			return status == http.StatusOK
		})
	}
	for _, name := range names {
		d := &public.Integration{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: public.IntegrationSpec{Type: "cloudflare", Resync: metav1.Duration{Duration: time.Hour}, Credentials: map[string]public.CredentialReference{"apiToken": {File: credential}}, Cloudflare: &public.CloudflareIntegration{Zones: []public.CloudflareZone{{Zone: name + ".example", ZoneID: zoneIDs[name]}}}}}
		if err := env.admin.Create(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	readyOf := func(name string) bool {
		var d public.Integration
		if err := env.admin.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &d); err != nil {
			return false
		}
		c := meta.FindStatusCondition(d.Status.Conditions, "Ready")
		return c != nil && c.Status == metav1.ConditionTrue
	}
	deadline := time.Now().Add(90 * time.Second)
	published := func() bool {
		for _, name := range names {
			if !readyOf(name) || kubePeerTarget(peer, name+".example") != "192.0.2.1" {
				return false
			}
		}
		return true
	}
	for !published() {
		if time.Now().After(deadline) {
			for _, name := range names {
				var d public.Integration
				_ = env.admin.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &d)
				t.Logf("%s: status %+v", name, d.Status)
			}
			peer.mu.Lock()
			t.Logf("peer rows %v requests %d writes %d", peer.rows, peer.requests, peer.writes)
			peer.mu.Unlock()
			t.Fatal("never converged: every instance publishes and reports Ready")
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(3 * time.Second)
	for name, holder := range holders {
		var lease coordinationv1.Lease
		if err := env.admin.Get(ctx, client.ObjectKey{Namespace: "default", Name: leaseName(name)}, &lease); err != nil {
			t.Fatal(err)
		}
		if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != holder {
			t.Fatalf("the Lease of %s is not held by %s", name, holder)
		}
	}
	requests, writes := kubePeerCounts(peer)
	if writes != n {
		t.Fatal("the instances did not publish by exactly one write each", n, writes)
	}
	var list corev1.SecretList
	if err := env.admin.List(ctx, &list, client.InNamespace("default"), client.MatchingLabels{kube.InternalKindLabel: kinds.KindIntegrationBudget}); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatal("the two replicas did not share one budget Secret", len(list.Items))
	}
	var budget kinds.IntegrationBudget
	if err := json.Unmarshal(list.Items[0].Data[kube.InternalObjectKey], &budget); err != nil {
		t.Fatal(err)
	}
	t.Logf("peer requests %d, budget reservations %d, budget sequence %d", requests, len(budget.Spec.Reservations), budget.Spec.Sequence)
	if requests == 0 || len(budget.Spec.Reservations) < requests {
		t.Fatalf("a request was sent without a reservation in the shared budget: %d requests, %d reservations", requests, len(budget.Spec.Reservations))
	}
}
