package kube

import (
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	private "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

type originalDeleteTransport struct {
	http.RoundTripper
	calls  *atomic.Int32
	change func()
}

func (d originalDeleteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/dnszones/delete-original") {
		if d.calls.Add(1) == 1 {
			d.change()
		}
	}
	return d.RoundTripper.RoundTrip(r)
}

func TestDeleteOriginalActualConflictIsNotRetried(t *testing.T) {
	requireEnv(t)
	purge(t, shared.admin)
	var calls atomic.Int32
	cfg := rest.CopyConfig(shared.cfg)
	cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return originalDeleteTransport{rt, &calls, func() {
			obj := &v1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "delete-original"}}
			if err := shared.admin.Patch(t.Context(), obj, client.RawPatch(types.MergePatchType, []byte(`{"spec":{"defaultTTL":1001}}`))); err != nil {
				t.Fatal(err)
			}
		}}
	})
	s := newStoreOn(t, cfg, Options{})
	original, err := s.Apply(t.Context(), zone("delete-original", "delete.example."), store.ApplyOptions{Manager: store.ManagerAPI})
	if err != nil {
		t.Fatal(err)
	}
	err = s.Delete(t.Context(), store.KeyOf(original), store.DeleteOptions{Manager: store.ManagerAPI, ExpectedUID: string(original.GetUID()), ExpectedResourceVersion: original.GetResourceVersion()})
	if !errors.Is(err, store.ErrConflict) || calls.Load() != 1 {
		t.Fatal("stale delete was recaptured", err, calls.Load())
	}
	var after v1alpha1.DNSZone
	if err := s.Get(t.Context(), store.KeyOf(original), &after); err != nil || after.Spec.DefaultTTL != 1001 {
		t.Fatal("newer object lost", err)
	}
}

func TestDeleteOriginalPrivateSecretIdentity(t *testing.T) {
	requireEnv(t)
	s := newTestStore(t, Options{Scheme: internalScheme(t), InternalKinds: internalKindList()})
	obj := &private.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: "delete-original-user"}, Spec: private.LocalUserSpec{MustChangePassword: true}}
	original, err := s.Apply(t.Context(), obj, store.ApplyOptions{Manager: store.ManagerSystem, CreateOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	obj = original.DeepCopyObject().(*private.LocalUser)
	obj.Spec.MustChangePassword = false
	next, err := s.Apply(t.Context(), obj, store.ApplyOptions{Manager: store.ManagerSystem, ExpectedResourceVersion: original.GetResourceVersion()})
	if err != nil {
		t.Fatal(err)
	}
	opts := store.DeleteOptions{Manager: store.ManagerSystem, ExpectedUID: string(original.GetUID()), ExpectedResourceVersion: original.GetResourceVersion()}
	if err := s.Delete(t.Context(), store.KeyOf(original), opts); !errors.Is(err, store.ErrConflict) {
		t.Fatal("Secret delete adopted successor", err)
	}
	opts.ExpectedResourceVersion = next.GetResourceVersion()
	if err := s.Delete(t.Context(), store.KeyOf(original), opts); err != nil {
		t.Fatal("exact Secret delete failed", err)
	}
}
