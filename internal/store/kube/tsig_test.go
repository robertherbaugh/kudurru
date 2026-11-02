package kube

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	kinds "github.com/robertherbaugh/kudurru/api/tsigkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/tsigkeys"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// This qualifies encrypted Secret persistence/startup readback, not hot command
// authority: Kubernetes does not implement the SQLite original CommandTx lane.
func TestTSIGEncryptedSecretOwnerRestartAndStrictSource(t *testing.T) {
	requireEnv(t)
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	var private []store.Kind
	for _, k := range runtimekinds.Private() {
		private = append(private, store.Kind(k))
	}
	opts := Options{Scheme: scheme, InternalKinds: private}
	s := newTestStore(t, opts)
	ctx := t.Context()
	const cluster = "tsig-envtest-cluster"
	master := bytes.Repeat([]byte{0x31}, 32)
	secret := bytes.Repeat([]byte{0x69}, 32)
	seal, err := tsigkeys.NewSealer(master, nil)
	if err != nil {
		t.Fatal("prepare sealer")
	}
	owner := tsigkeys.Owner{ClusterID: cluster, UID: "nonserving-provisioning-owner", KeyName: "secret-test.example.", Algorithm: dns.HmacSHA256}
	makeRecord := func() *kinds.TSIGKey {
		material, err := tsigkeys.NewMaterial(owner, secret)
		if err != nil {
			t.Fatal("prepare credential")
		}
		record, err := seal.Seal(material)
		if err != nil {
			t.Fatal("seal credential")
		}
		return record
	}
	created, err := s.Apply(ctx, makeRecord(), store.ApplyOptions{Manager: store.ManagerSystem, CreateOnly: true})
	if err != nil {
		t.Fatal("create encrypted nonserving carrier", err)
	}
	key := store.KeyOf(created)
	t.Cleanup(func() {
		_ = s.c.Delete(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: s.opts.Namespace, Name: InternalSecretName(key.Kind, key.Namespace, key.Name)}})
	})
	if string(created.GetUID()) == owner.UID || created.GetUID() == "" {
		t.Fatal("API server did not assign actual Secret UID")
	}
	cut, err := tsigkeys.Read(ctx, s, cluster)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := cut.Open(seal); err == nil {
		t.Fatal("preallocated UID became serving ownership")
	}
	// Explicit setup CAS binds to the actual Secret UID; no reader mutates it.
	owner.UID = string(created.GetUID())
	final := makeRecord()
	updated, err := s.Apply(ctx, final, store.ApplyOptions{Manager: store.ManagerSystem, ExpectedResourceVersion: created.GetResourceVersion()})
	if err != nil {
		t.Fatal("actual-owner setup CAS", err)
	}
	cut, err = tsigkeys.Read(ctx, s, cluster)
	if err != nil {
		t.Fatal(err)
	}
	materials, previous, err := cut.Open(seal)
	if err != nil || previous || len(materials) != 1 || !bytes.Equal(materials[0].Key().Secret, secret) {
		t.Fatal("actual Secret source unavailable")
	}
	var carrier corev1.Secret
	carrierKey := client.ObjectKey{Namespace: s.opts.Namespace, Name: InternalSecretName(kinds.KindTSIGKey, "", final.Name)}
	if err := s.c.Get(ctx, carrierKey, &carrier); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(carrier.Data)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(carrier.Data[InternalObjectKey], secret) || bytes.Contains(carrier.Data[InternalObjectKey], []byte(base64.StdEncoding.EncodeToString(secret))) || bytes.Contains(raw, secret) {
		t.Fatal("plaintext escaped actual Secret")
	}
	for _, safe := range []bool{true, false} {
		var backup bytes.Buffer
		if err := s.Snapshot(ctx, &backup, store.SnapshotOptions{Safe: safe}); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(backup.Bytes(), []byte("kind: TSIGKey")) == safe {
			t.Fatal("private Secret snapshot classification mismatch")
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := newTestStore(t, opts)
	fresh, err := tsigkeys.Read(ctx, restarted, cluster)
	if err != nil || fresh.Identity() != cut.Identity() {
		t.Fatal("restart lost actual source identity", err)
	}
	nextSeal, err := tsigkeys.NewSealer(bytes.Repeat([]byte{0x32}, 32), master)
	if err != nil {
		t.Fatal(err)
	}
	if _, previous, err := fresh.Open(nextSeal); err != nil || !previous {
		t.Fatal("restart with previous key failed")
	}
	missing, _ := tsigkeys.NewSealer(bytes.Repeat([]byte{0x32}, 32), nil)
	if _, _, err := fresh.Open(missing); err == nil {
		t.Fatal("missing encryption key skipped stored credential")
	}
	if _, err := restarted.Apply(ctx, final, store.ApplyOptions{Manager: store.ManagerSystem, ExpectedResourceVersion: created.GetResourceVersion()}); err == nil {
		t.Fatal("stale provisioning CAS accepted")
	}
	// Corrupt carriers cannot disappear from the complete key source. Restore
	// each through the actual API server, preserving its authoritative UID/RV.
	pristine := carrier.DeepCopy()
	foreign := pristine.DeepCopy()
	foreign.Name += "-foreign"
	foreign.ResourceVersion = ""
	foreign.UID = ""
	foreign.Type = corev1.SecretTypeOpaque
	if err := restarted.c.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	if _, err := tsigkeys.Read(ctx, restarted, cluster); err == nil {
		t.Fatal("wrong type disappeared from key inventory")
	}
	if err := restarted.c.Delete(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*corev1.Secret)
	}{
		{"missing", func(s *corev1.Secret) { delete(s.Data, InternalObjectKey) }},
		{"json", func(s *corev1.Secret) { s.Data[InternalObjectKey] = []byte(`{"kind":"TSIGKey","unknown":true}`) }},
		{"oversize", func(s *corev1.Secret) { s.Data[InternalObjectKey] = bytes.Repeat([]byte{' '}, kinds.MaxObjectBytes+1) }},
		{"binding", func(s *corev1.Secret) {
			var r kinds.TSIGKey
			_ = json.Unmarshal(s.Data[InternalObjectKey], &r)
			r.Spec.KeyName = "substituted.example."
			s.Data[InternalObjectKey], _ = json.Marshal(&r)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var actual corev1.Secret
			if err := restarted.c.Get(ctx, carrierKey, &actual); err != nil {
				t.Fatal(err)
			}
			bad := pristine.DeepCopy()
			bad.ResourceVersion = actual.ResourceVersion
			tc.mutate(bad)
			if err := restarted.c.Update(ctx, bad); err != nil {
				t.Fatal("install malformed test carrier", err)
			}
			if _, err := tsigkeys.Read(ctx, restarted, cluster); err == nil {
				t.Fatal("malformed private carrier skipped")
			}
			restored := pristine.DeepCopy()
			restored.ResourceVersion = bad.ResourceVersion
			if err := restarted.c.Update(ctx, restored); err != nil {
				t.Fatal("restore fixture carrier", err)
			}
		})
	}
	last, err := tsigkeys.Read(ctx, restarted, cluster)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := last.Open(seal); err != nil {
		t.Fatal("restored actual owner unavailable")
	}
	if updated.GetUID() != created.GetUID() {
		t.Fatal("setup CAS changed carrier incarnation")
	}
}
