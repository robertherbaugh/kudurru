package kube

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	internalkinds "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// TestEnvtestAuditWrap: the audit decorator serve puts around this driver
// (D156) records one event per change, none for an identical apply, and
// no documents for internal kinds.
func TestEnvtestAuditWrap(t *testing.T) {
	requireEnv(t)
	purge(t, shared.admin)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	scheme := internalScheme(t)
	inner := newTestStore(t, Options{Scheme: scheme, InternalKinds: internalKindList()})
	if !inner.WaitForSync(ctx) {
		t.Fatal("informers did not sync")
	}
	t.Cleanup(func() {
		_ = shared.admin.DeleteAllOf(context.Background(), &corev1.Secret{}, client.InNamespace("kudurru-system"), client.HasLabels{InternalKindLabel})
	})
	rec := &audit.Memory{}
	st := audit.WrapStore(inner, rec, audit.WrapOptions{Node: "node-a", Scheme: scheme})
	actx := store.WithActor(ctx, "User:alice")

	if _, err := st.Apply(actx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	// The informer cache lags the write; the decorator reads through it.
	waitFor(t, func() bool {
		var z = zone("home", "")
		return inner.Get(ctx, store.KeyOf(z), z) == nil
	})
	if _, err := st.Apply(actx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	changed := zone("home", "home.example.")
	changed.Spec.Nameservers = []string{"ns1.home.example."}
	if _, err := st.Apply(actx, changed, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		z := zone("home", "")
		return inner.Get(ctx, store.KeyOf(z), z) == nil && len(z.Spec.Nameservers) == 1
	})
	if err := st.Delete(actx, store.Key{Kind: "DNSZone", Name: "home"}, store.DeleteOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	u := &internalkinds.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: "admin"}, Spec: internalkinds.LocalUserSpec{ //nolint:gosec // G101: a dummy hash
		PasswordHash: "$argon2id$v=19$m=19456,t=2,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
	}}
	if _, err := st.Apply(actx, u, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, ev := range rec.Events() {
		got = append(got, ev.Action)
		if ev.Actor.String() != "User:alice" || ev.Node != "node-a" {
			t.Errorf("%s: actor %v node %q", ev.Action, ev.Actor, ev.Node)
		}
	}
	if strings.Join(got, ",") != "dnszone.create,dnszone.update,dnszone.delete,localuser.create" {
		t.Fatalf("actions %v", got)
	}
	evs := rec.Events()
	if strings.Contains(string(evs[1].Before), "ns1.home.example.") || !strings.Contains(string(evs[1].After), "ns1.home.example.") ||
		strings.Contains(string(evs[1].After), "managedFields") {
		t.Fatalf("update documents: before %s after %s", evs[1].Before, evs[1].After)
	}
	if evs[3].Before != nil || evs[3].After != nil {
		t.Fatalf("internal kind recorded with documents: %s", evs[3].After)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
