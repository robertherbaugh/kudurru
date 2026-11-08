package externaldns

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
)

type crdObservedTransport struct {
	http.RoundTripper
	watches, writes *atomic.Int32
	report          func(string)
}

func (t crdObservedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.Contains(r.URL.Path, "/dnsendpoints") {
		if r.Method != http.MethodGet {
			t.writes.Add(1)
		}
		if r.URL.Query().Get("watch") == "true" {
			t.watches.Add(1)
		}
	}
	res, err := t.RoundTripper.RoundTrip(r)
	if err == nil && res.StatusCode == 409 && t.report != nil {
		data, readErr := io.ReadAll(io.LimitReader(res.Body, 32<<10))
		_ = res.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		t.report(string(data))
		res.Body = io.NopCloser(bytes.NewReader(data))
	}
	return res, err
}

func TestCRDActualWatchOwnershipAndRefusals(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		paths, _ := filepath.Glob("../../../bin/k8s/*")
		for _, path := range paths {
			if _, err := os.Stat(filepath.Join(path, "kube-apiserver")); err == nil {
				assets = path
			}
		}
	}
	if assets == "" {
		t.Skip("envtest assets unavailable")
	}
	e := &envtest.Environment{BinaryAssetsDirectory: assets, CRDDirectoryPaths: []string{"../../../deploy/kustomize/crd/bases", "testdata"}, ErrorIfCRDPathMissing: true}
	e.ControlPlane.GetAPIServer().Configure().Set("advertise-address", "127.0.0.1").Set("endpoint-reconciler-type", "none")
	t.Cleanup(func() {
		if err := e.Stop(); err != nil {
			t.Error(err)
		}
	})
	cfg, err := e.Start()
	if err != nil {
		t.Fatal(err)
	}

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, rbacv1.AddToScheme, v1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	admin, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"apps", "other"} {
		if err := admin.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatal(err)
		}
	}
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "endpoint-reader"}, Rules: []rbacv1.PolicyRule{
		{APIGroups: []string{"externaldns.k8s.io"}, Resources: []string{"dnsendpoints"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{"kudurru.io"}, Resources: []string{"dnszones"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{"kudurru.io"}, Resources: []string{"dnsrecordsets"}, Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
		{APIGroups: []string{""}, Resources: []string{"namespaces"}, Verbs: []string{"get"}},
	}}
	if err := admin.Create(t.Context(), role); err != nil {
		t.Fatal(err)
	}
	if err := admin.Create(t.Context(), &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: role.Name}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name}, Subjects: []rbacv1.Subject{{Kind: "User", Name: "endpoint-reader"}}}); err != nil {
		t.Fatal(err)
	}
	user, err := e.AddUser(envtest.User{Name: "endpoint-reader"}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	userCfg := rest.CopyConfig(user.Config())
	var watches, sourceWrites atomic.Int32
	userCfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return crdObservedTransport{rt, &watches, &sourceWrites, func(body string) { t.Log("actual API-server conflict:", body) }}
	})
	limitedScheme := runtime.NewScheme()
	limitedScheme.AddKnownTypes(v1alpha1.GroupVersion, &v1alpha1.DNSZone{}, &v1alpha1.DNSZoneList{}, &v1alpha1.DNSRecordSet{}, &v1alpha1.DNSRecordSetList{})
	metav1.AddToGroupVersion(limitedScheme, v1alpha1.GroupVersion)
	if err := corev1.AddToScheme(limitedScheme); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := kube.New(userCfg, kube.Options{Namespace: "apps", Identity: "endpoint-reader", Scheme: limitedScheme, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	v, err := sharedValidator()
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewCRDController(st.RESTConfig(), CRDOptions{Store: st, Validator: v, Namespace: "apps", Logger: log, Admit: func(ctx context.Context) error { return ctx.Err() }})
	if err != nil {
		t.Fatal(err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, z := range []*v1alpha1.DNSZone{zone("base", "example.com.", nil), zone("child", "child.example.com.", nil)} {
		if err := admin.Create(t.Context(), z); err != nil {
			t.Fatal(err)
		}
	}
	resource := dyn.Resource(endpointResource).Namespace("apps")
	source := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": endpointResource.GroupVersion().String(), "kind": "DNSEndpoint",
		"metadata": map[string]any{"name": "web", "namespace": "apps"},
		"spec": map[string]any{"endpoints": []any{
			map[string]any{"dnsName": "web.example.com.", "recordType": "A", "recordTTL": int64(45), "targets": []any{"192.0.2.10"}},
			map[string]any{"dnsName": "www.child.example.com", "recordType": "AAAA", "targets": []any{"2001:db8::10"}},
		}},
	}}
	source, err = resource.Create(t.Context(), source, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	initial := source.DeepCopy()
	syncOnce := func() error {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		_, err := controller.reconcile(ctx)
		return err
	}
	rows := func() []*v1alpha1.DNSRecordSet {
		items, err := controller.records(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return items
	}
	update := func(target string) {
		current, err := resource.Get(t.Context(), "web", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		data, _, _ := unstructured.NestedSlice(current.Object, "spec", "endpoints")
		data[0].(map[string]any)["targets"] = []any{target}
		if err := unstructured.SetNestedSlice(current.Object, data, "spec", "endpoints"); err != nil {
			t.Fatal(err)
		}
		if _, err := resource.Update(t.Context(), current, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("initial-and-read-only-source", func(t *testing.T) {
		if err := syncOnce(); err != nil {
			t.Fatal(err)
		}
		sets := rows()
		if len(sets) != 2 {
			t.Fatal("longest-suffix projection", len(sets))
		}
		for _, rs := range sets {
			if rs.Namespace != "apps" || rs.Annotations[crdOwner] != string(source.GetUID()) || store.ManagedBy(rs) != sourceManager(string(source.GetUID())) || len(rs.OwnerReferences) != 1 || rs.OwnerReferences[0].UID != source.GetUID() {
				t.Fatal("source ownership was lost", rs.Name)
			}
		}
		after, err := resource.Get(t.Context(), "web", metav1.GetOptions{})
		if err != nil || !reflect.DeepEqual(after.Object, initial.Object) {
			t.Fatal("source was changed", err)
		}
		if sourceWrites.Load() != 0 {
			t.Fatal("controller tried to write source")
		}
		_, err = controller.sources().Update(t.Context(), after, metav1.UpdateOptions{})
		if !apierrors.IsForbidden(err) {
			t.Fatal("read-only source RBAC was not enforced", err)
		}
		sourceWrites.Store(0)
	})
	t.Run("invalid-source-withdrawal", func(t *testing.T) {
		update("not-an-address")
		if err := syncOnce(); err != nil {
			t.Fatal(err)
		}
		if len(rows()) != 0 {
			t.Fatal("invalid current source kept stale records")
		}
		update("192.0.2.10")
		if err := syncOnce(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("watch-update-and-term-stop", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- controller.Run(ctx) }()
		defer func() {
			cancel()
			if err := <-done; err != nil {
				t.Error(err)
			}
		}()
		deadline := time.Now().Add(5 * time.Second)
		for watches.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if watches.Load() == 0 {
			t.Fatal("actual source watch did not start")
		}
		update("192.0.2.11")
		deadline = time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			for _, rs := range rows() {
				if rs.Spec.ZoneRef.Name == "base" && rs.Spec.Endpoints[0].Targets[0] == "192.0.2.11" {
					return
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("watch did not reconcile actual source change")
	})
	t.Run("original-source-refusal", func(t *testing.T) {
		update("192.0.2.12")
		var once atomic.Bool
		controller.opts.Store = &crdInterceptStore{Store: st, get: func(key store.Key) {
			if key.Kind == "DNSRecordSet" && once.CompareAndSwap(false, true) {
				update("192.0.2.13")
			}
		}}
		defer func() { controller.opts.Store = st }()
		if err := syncOnce(); !errors.Is(err, store.ErrConflict) {
			t.Fatal("superseded source was rescued", err)
		}
		for _, rs := range rows() {
			if rs.Spec.ZoneRef.Name == "base" && rs.Spec.Endpoints[0].Targets[0] != "192.0.2.11" {
				t.Fatal("refusal wrote stale source")
			}
		}
	})
	t.Run("final-term-cancellation", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		var once atomic.Bool
		controller.opts.Store = &crdInterceptStore{Store: st, get: func(key store.Key) {
			if key.Kind == "DNSRecordSet" && once.CompareAndSwap(false, true) {
				cancel()
			}
		}}
		defer func() { controller.opts.Store = st }()
		if _, err := controller.reconcile(ctx); !errors.Is(err, context.Canceled) {
			t.Fatal("canceled term wrote", err)
		}
	})
	t.Run("unknown-write-not-replayed", func(t *testing.T) {
		wrapper := &crdInterceptStore{Store: st, loseReply: true}
		controller.opts.Store = wrapper
		defer func() { controller.opts.Store = st }()
		if err := syncOnce(); !errors.Is(err, io.ErrUnexpectedEOF) || wrapper.applies != 1 {
			t.Fatal("lost response was replayed or succeeded", err, wrapper.applies)
		}
		// A separate, new observation sees the actual committed object. No
		// second write is necessary, even after process/worker replacement.
		committed := wrapper.lastKey
		wrapper.loseReply = false
		if err := syncOnce(); err != nil {
			t.Fatal(err)
		}
		if wrapper.counts[committed] != 1 {
			t.Fatal("new observation rewrote confirmed state")
		}
	})
	t.Run("zone-selector-withdrawal", func(t *testing.T) {
		var z v1alpha1.DNSZone
		if err := admin.Get(t.Context(), client.ObjectKey{Name: "child"}, &z); err != nil {
			t.Fatal(err)
		}
		z.Spec.RecordSetSelector = &v1alpha1.RecordSetSelector{Namespaces: []string{"other"}}
		if err := admin.Update(t.Context(), &z); err != nil {
			t.Fatal(err)
		}
		if err := syncOnce(); err != nil {
			t.Fatal(err)
		}
		if got := rows(); len(got) != 1 || got[0].Spec.ZoneRef.Name != "base" {
			t.Fatal("selector did not withdraw own child", len(got))
		}
	})
	t.Run("foreign-owner-preserved", func(t *testing.T) {
		rs := rows()[0]
		store.SetManagedBy(rs, "api")
		if err := admin.Update(t.Context(), rs); err != nil {
			t.Fatal(err)
		}
		unrelated := initial.DeepCopy()
		unrelated.SetUID("")
		unrelated.SetResourceVersion("")
		unrelated.SetCreationTimestamp(metav1.Time{})
		unrelated.SetManagedFields(nil)
		unrelated.SetName("zz-unrelated")
		if err := unstructured.SetNestedSlice(unrelated.Object, []any{map[string]any{"dnsName": "unrelated.example.com", "recordType": "A", "targets": []any{"192.0.2.90"}}}, "spec", "endpoints"); err != nil {
			t.Fatal(err)
		}
		unrelated, err = resource.Create(t.Context(), unrelated, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		update("192.0.2.14")
		if err := syncOnce(); !store.IsManagedBy(err) {
			t.Fatal("foreign row taken over", err)
		}
		var after *v1alpha1.DNSRecordSet
		independent := false
		for _, row := range rows() {
			if row.Name == rs.Name {
				after = row
			}
			if row.Annotations[crdOwner] == string(unrelated.GetUID()) {
				independent = true
			}
		}
		if after == nil || !independent {
			t.Fatal("conflicting source starved independent reconciliation")
		}

		if after.Spec.Endpoints[0].Targets[0] != "192.0.2.13" || store.ManagedBy(after) != "api" {
			t.Fatal("foreign object changed")
		}
		if err := resource.Delete(t.Context(), unrelated.GetName(), metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := admin.Delete(t.Context(), after); err != nil {
			t.Fatal(err)
		}
		if err := syncOnce(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("uid-replacement-and-deletion", func(t *testing.T) {
		if err := resource.Delete(t.Context(), "web", metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		replacement := initial.DeepCopy()
		replacement.SetUID("")
		replacement.SetResourceVersion("")
		replacement.SetCreationTimestamp(metav1.Time{})
		replacement.SetManagedFields(nil)
		replacement, err = resource.Create(t.Context(), replacement, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if replacement.GetUID() == initial.GetUID() {
			t.Fatal("fixture did not replace source UID")
		}
		if err := syncOnce(); err != nil {
			t.Fatal(err)
		}
		for _, rs := range rows() {
			if rs.Annotations[crdOwner] != string(replacement.GetUID()) {
				t.Fatal("old UID survived replacement")
			}
		}
		if err := resource.Delete(t.Context(), "web", metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := syncOnce(); err != nil {
			t.Fatal(err)
		}
		if len(rows()) != 0 {
			t.Fatal("deleted source retained records")
		}
	})
	if sourceWrites.Load() != 0 {
		t.Fatal("controller mutated source")
	}
}

type crdInterceptStore struct {
	store.Store
	get       func(store.Key)
	loseReply bool
	applies   int
	lastKey   store.Key
	counts    map[store.Key]int
}

func (s *crdInterceptStore) Get(ctx context.Context, key store.Key, into store.Object) error {
	err := s.Store.Get(ctx, key, into)
	if s.get != nil {
		s.get(key)
	}
	return err
}
func (s *crdInterceptStore) Apply(ctx context.Context, obj store.Object, opts store.ApplyOptions) (store.Object, error) {
	s.applies++
	s.lastKey = store.KeyOf(obj)
	if s.counts == nil {
		s.counts = map[store.Key]int{}
	}
	s.counts[s.lastKey]++
	out, err := s.Store.Apply(ctx, obj, opts)
	if err == nil && s.loseReply {
		return nil, io.ErrUnexpectedEOF
	}
	return out, err
}
