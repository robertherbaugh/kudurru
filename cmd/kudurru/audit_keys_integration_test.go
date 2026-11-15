package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	auditv1 "github.com/robertherbaugh/kudurru/api/audit/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/api/apitest"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/audit/auditkeys"
	"github.com/robertherbaugh/kudurru/internal/audit/auditlog"
	"github.com/robertherbaugh/kudurru/internal/audit/spool"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
)

func TestAuditIndependentKeysMountedLifecycle(t *testing.T) {
	cfg := auditLogTestAPI(t)
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = auditv1.AddToScheme(scheme)
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal("create fixture client")
	}
	var cluster corev1.Namespace
	if c.Get(t.Context(), client.ObjectKey{Name: "kube-system"}, &cluster) != nil {
		t.Fatal("get original cluster binding")
	}
	namespace := kube.PodNamespace()
	ring, err := auditkeys.New(string(cluster.UID), namespace, nil, nil, time.Now())
	if err != nil {
		t.Fatal("initialize independent ring")
	}
	var secret corev1.Secret
	publish := func(recreate bool) {
		t.Helper()
		raw, _ := ring.EncodePrivate()
		if secret.UID == "" || recreate {
			if recreate {
				if c.Delete(t.Context(), &secret) != nil {
					t.Fatal("delete Secret for restore")
				}
			}
			secret = corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: auditkeys.DefaultSecret, Namespace: namespace}, Data: map[string][]byte{"ring.json": raw}}
			if c.Create(t.Context(), &secret) != nil {
				t.Fatal("provision independent Secret")
			}
		} else {
			secret.Data = map[string][]byte{"ring.json": raw}
			if c.Update(t.Context(), &secret) != nil {
				t.Fatal("publish rotated ring")
			}
		}
	}
	sp, err := spool.Open(t.Context(), spool.Options{Path: filepath.Join(t.TempDir(), spool.FileName), Node: "independent-keys"})
	if err != nil {
		t.Fatal("open local spool")
	}
	t.Cleanup(func() { _ = sp.Close() })
	open := func() *auditLogStore {
		t.Helper()
		source, err := auditkeys.NewSource(cfg, namespace, auditkeys.DefaultSecret, func() string { return string(cluster.UID) })
		if err != nil {
			t.Fatal("build independent trust source")
		}
		store, err := openAuditLog(t.Context(), cfg, sp, source, slog.New(slog.DiscardHandler), 2*time.Second)
		if err != nil || store == nil {
			t.Fatal("open production AuditLog constructor")
		}
		return store
	}
	first := open() // no Secret yet: DNS startup seam retains reader and local spool
	if !errors.Is(first.reader.Verify(t.Context()), auditkeys.ErrUnavailable) {
		t.Fatal("missing independent trust accepted")
	}
	ship := func(store *auditLogStore) error {
		t.Helper()
		after, err := store.shipper.Target.Cursor(t.Context(), sp)
		if err != nil {
			return err
		}
		events, err := sp.ReadAfter(t.Context(), after, 10)
		if err != nil {
			return err
		}
		return store.shipper.Target.Append(t.Context(), sp, events)
	}
	record := func() {
		t.Helper()
		if sp.Record(t.Context(), audit.Event{Action: "dnszone.update", Result: audit.ResultSuccess}) != nil {
			t.Fatal("record local event")
		}
	}
	record()
	if !errors.Is(ship(first), auditkeys.ErrUnavailable) {
		t.Fatal("unsigned shipping during trust outage")
	}
	if sp.Verify(t.Context()) != nil {
		t.Fatal("trust outage broke durable local spool")
	}
	// Preserve a real pre-upgrade HMAC batch and admit it only through an
	// explicit imported purpose key plus a past cutover.
	master := bytes.Repeat([]byte{9}, 32)
	legacy, err := openLegacyAuditLog(t.Context(), cfg, sp, master, nil, slog.New(slog.DiscardHandler), 2*time.Second)
	if err != nil || legacy == nil || ship(legacy) != nil {
		t.Fatal("create historical legacy batch")
	}
	// Kubernetes creation timestamps serialize whole seconds. Choose the next
	// server second and wait for it before admitting new-format writers.
	var historical auditv1.AuditLogList
	if c.List(t.Context(), &historical, client.InNamespace(namespace)) != nil || len(historical.Items) != 1 {
		t.Fatal("read original legacy metadata")
	}
	cutoff := historical.Items[0].CreationTimestamp.Add(time.Second)
	timer := time.NewTimer(time.Until(cutoff))
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-t.Context().Done():
		t.Fatal("cutover canceled")
	}
	purpose, err := auditlog.SealKey(master)
	if err != nil {
		t.Fatal("derive explicit legacy import")
	}
	ring, err = auditkeys.New(string(cluster.UID), namespace, [][]byte{purpose}, &cutoff, time.Now())
	if err != nil {
		t.Fatal("initialize explicit legacy admission")
	}
	publish(false)
	record()
	if ship(first) != nil || first.reader.Verify(t.Context()) != nil {
		t.Fatal("existing constructor did not recover after provisioning")
	}
	oldestID := ring.ActiveID()
	var initial auditv1.AuditLogList
	if c.List(t.Context(), &initial, client.InNamespace(namespace)) != nil || len(initial.Items) != 2 {
		t.Fatal("initial retained batch")
	}
	original := initial.Items[0].DeepCopy()
	if original.Spec.FirstID == 1 {
		original = initial.Items[1].DeepCopy()
	}
	legacyOriginal := initial.Items[0].DeepCopy()
	if legacyOriginal.Spec.FirstID != 1 {
		legacyOriginal = initial.Items[1].DeepCopy()
	}
	// Master/session material is deliberately absent from this production API;
	// independent rotations and reconstructed replicas retain every old seal.
	for range 4 {
		ring, err = ring.Rotate()
		if err != nil {
			t.Fatal("rotate independent signer")
		}
		publish(false)
		replica := open()
		record()
		if ship(replica) != nil || replica.reader.Verify(t.Context()) != nil {
			t.Fatal("retained history after restart/rotation")
		}
	}
	var unchanged auditv1.AuditLog
	if c.Get(t.Context(), client.ObjectKeyFromObject(original), &unchanged) != nil || !reflect.DeepEqual(original.Spec, unchanged.Spec) {
		t.Fatal("rotation changed historical source or seal")
	}
	var legacyUnchanged auditv1.AuditLog
	if c.Get(t.Context(), client.ObjectKeyFromObject(legacyOriginal), &legacyUnchanged) != nil || !reflect.DeepEqual(legacyOriginal.Spec, legacyUnchanged.Spec) {
		t.Fatal("upgrade changed legacy evidence")
	}
	second := open()
	mounted, _ := auditVerifyAPI(t, first.reader, license.Unrestricted())
	replicaMounted, _ := auditVerifyAPI(t, second.reader, license.Unrestricted())
	get := func(server, user string) (int, api.AuditVerification) {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, server+"/api/v1/audit/verify", nil)
		req.Header.Set("Authorization", "Bearer "+apitest.BearerPrefix+user)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal("mounted verification request")
		}
		defer func() { _ = response.Body.Close() }()
		data, err := io.ReadAll(io.LimitReader(response.Body, 16384))
		if err != nil {
			t.Fatal("read mounted response")
		}
		var value api.AuditVerification
		if response.StatusCode == http.StatusOK && json.Unmarshal(data, &value) != nil {
			t.Fatal("decode mounted response")
		}
		return response.StatusCode, value
	}
	for _, server := range []string{mounted.URL, replicaMounted.URL} {
		if code, value := get(server, "auditor"); code != 200 || !value.Verified {
			t.Fatal("replicas disagree about shared independent trust")
		}
	}
	if code, _ := get(mounted.URL, "viewer"); code != http.StatusForbidden {
		t.Fatal("cache bypassed permission")
	}
	community, err := license.New(license.Options{})
	if err != nil {
		t.Fatal("community license fixture")
	}
	unlicensed, _ := auditVerifyAPI(t, first.reader, community)
	if code, _ := get(unlicensed.URL, "auditor"); code != http.StatusPaymentRequired {
		t.Fatal("bypassed license")
	}
	// A UID replacement with exactly the same ring is a new cache cut, preserving
	// the original signing domain and cluster rather than inventing new trust.
	held, err := first.reader.SnapshotVerification(t.Context())
	if err != nil {
		t.Fatal("capture original ring")
	}
	publish(true)
	if held.Current(t.Context()) == nil {
		t.Fatal("Secret recreation did not supersede old cache cut")
	}
	if code, value := get(mounted.URL, "auditor"); code != 200 || !value.Verified {
		t.Fatal("same-cluster exact-ring restore did not verify")
	}
	trustedRing := ring
	// Hold a real scan after its batch walk, then revoke its original key.
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	heldReader := open().reader
	heldReader.VerifyPending = func(ctx context.Context) error {
		once.Do(func() { close(started) })
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return sp.Verify(ctx)
		}
	}
	heldAPI, _ := auditVerifyAPI(t, heldReader, license.Unrestricted())
	done := make(chan int, 1)
	go func() { code, _ := get(heldAPI.URL, "auditor"); done <- code }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("held verification did not start")
	}
	ring, err = ring.Revoke(oldestID)
	if err != nil {
		close(release)
		t.Fatal("revoke retained signer")
	}
	publish(false)
	close(release)
	select {
	case code := <-done:
		if code != http.StatusServiceUnavailable {
			t.Fatal("held scan silently adopted successor trust")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("held scan did not finish")
	}
	for _, server := range []string{mounted.URL, replicaMounted.URL} {
		if code, _ := get(server, "auditor"); code != http.StatusServiceUnavailable {
			t.Fatal("cached healthy result survived revocation")
		}
	}
	ring = trustedRing
	publish(false)
	if code, value := get(mounted.URL, "auditor"); code != http.StatusOK || !value.Verified {
		t.Fatal("explicit same-cluster trust recovery failed")
	}
	var changed auditv1.AuditLog
	if c.Get(t.Context(), client.ObjectKeyFromObject(original), &changed) != nil {
		t.Fatal("get corruption fixture")
	}
	if changed.CreationTimestamp.Time.Before(cutoff) {
		t.Fatal("new batch admitted before legacy cutoff")
	}
	forged := changed.Spec
	forged.Seal = ""
	body, _ := json.Marshal(forged)
	mac := hmac.New(sha256.New, purpose)
	_, _ = mac.Write(body)
	changed.Spec.Seal = hex.EncodeToString(mac.Sum(nil))
	if c.Update(t.Context(), &changed) != nil {
		t.Fatal("prepare explicit downgrade fixture")
	}
	downgradeAPI, _ := auditVerifyAPI(t, open().reader, license.Unrestricted())
	if code, _ := get(downgradeAPI.URL, "auditor"); code != http.StatusServiceUnavailable {
		t.Fatal("new batch downgraded into historical HMAC verifier")
	}
	changed.Spec = original.Spec
	if c.Update(t.Context(), &changed) != nil {
		t.Fatal("restore signature fixture")
	}
	changed.Spec.Events[0].Reason = "changed"
	if c.Update(t.Context(), &changed) != nil {
		t.Fatal("change corruption fixture")
	}
	freshAPI, _ := auditVerifyAPI(t, open().reader, license.Unrestricted())
	if code, value := get(freshAPI.URL, "auditor"); code != http.StatusOK || value.Verified {
		t.Fatal("invalid signature was not an integrity failure")
	}
	// Restore only fixture data so subsequent cases isolate trust admission.
	changed.Spec = original.Spec
	if c.Update(t.Context(), &changed) != nil {
		t.Fatal("restore fixture source evidence")
	}
	// Changing the ring to a new cluster is not an authorized recovery path.
	wrong, err := auditkeys.New("new-cluster", namespace, nil, nil, time.Now())
	if err != nil {
		t.Fatal("wrong cluster fixture")
	}
	ring = wrong
	publish(false)
	if code, _ := get(mounted.URL, "auditor"); code != http.StatusServiceUnavailable {
		t.Fatal("accepted wrong-cluster ring")
	}
	// Production source errors never embed the credential-bearing config.
	if _, err := openAuditLog(t.Context(), rest.CopyConfig(cfg), sp, nil, slog.New(slog.DiscardHandler), time.Second); !errors.Is(err, auditkeys.ErrUnavailable) {
		t.Fatal("nil production trust admitted")
	}
}
