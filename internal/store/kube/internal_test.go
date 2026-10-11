package kube

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	internalkinds "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

func internalScheme(t testing.TB) *runtime.Scheme {
	t.Helper()
	s := testScheme()
	if err := internalkinds.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func internalKindList() []store.Kind {
	out := make([]store.Kind, 0, len(internalkinds.Kinds))
	for _, k := range internalkinds.Kinds {
		out = append(out, store.Kind(k))
	}
	return out
}

func TestInternalSecretName(t *testing.T) {
	for _, tc := range []struct {
		kind      store.Kind
		ns, name  string
		want      string
		namespace bool
	}{
		{kind: "LocalUser", name: "admin", want: "kudurru-localuser.admin"},
		{kind: "RoleBinding", ns: "dns", name: "zone-admins", want: "kudurru-rolebinding.dns.zone-admins"},
		{kind: "APIToken", name: "abcdefghijkl", want: "kudurru-apitoken.abcdefghijkl"},
	} {
		if got := InternalSecretName(tc.kind, tc.ns, tc.name); got != tc.want {
			t.Errorf("InternalSecretName(%s,%q,%q) = %q, want %q", tc.kind, tc.ns, tc.name, got, tc.want)
		}
	}
}

func TestSplitInternalRejectsUnknownKind(t *testing.T) {
	r, err := newRegistry(internalScheme(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := splitInternal(r, []store.Kind{"Nope"}); err == nil {
		t.Fatal("unknown internal kind accepted")
	}
	r, _ = newRegistry(internalScheme(t))
	ik, err := splitInternal(r, internalKindList())
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range internalKindList() {
		if _, ok := r.byKind[k]; ok {
			t.Errorf("%s still in the CRD registry", k)
		}
		if _, ok := ik.get(k); !ok {
			t.Errorf("%s missing from the internal set", k)
		}
	}
	if _, ok := r.byKind["DNSZone"]; !ok {
		t.Error("DNSZone dropped from the CRD registry")
	}
}

func TestEnvtestInternalKinds(t *testing.T) {
	requireEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s := newTestStore(t, Options{Scheme: internalScheme(t), InternalKinds: internalKindList()})
	if !s.WaitForSync(ctx) {
		t.Fatal("informers did not sync")
	}
	t.Cleanup(func() {
		_ = shared.admin.DeleteAllOf(context.Background(), &corev1.Secret{}, client.InNamespace("kudurru-system"), client.HasLabels{InternalKindLabel})
	})

	w, err := s.Watch(ctx, internalkinds.KindRoleBinding, store.WatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	if ev := next(t, w); ev.Type != store.Bookmark {
		t.Fatalf("first event %s, want the bookmark of an empty kind", ev.Type)
	}

	t.Run("create, get and the backing Secret", func(t *testing.T) {
		u := &internalkinds.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: "admin", Labels: map[string]string{"tier": "builtin"}}, Spec: internalkinds.LocalUserSpec{ //nolint:gosec // G101: a dummy hash
			PasswordHash: "$argon2id$v=19$m=19456,t=2,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA", MustChangePassword: true, BuiltinAdmin: true,
		}, Status: internalkinds.LocalUserStatus{LastLoginAt: &metav1.Time{Time: time.Unix(1, 0)}}}
		got, err := s.Apply(ctx, u, store.ApplyOptions{Manager: store.ManagerSystem})
		if err != nil {
			t.Fatal(err)
		}
		if got.GetResourceVersion() == "" || got.GetUID() == "" || got.GetGeneration() != 1 || store.ManagedBy(got) != store.ManagerSystem {
			t.Fatalf("metadata not filled: %+v", got)
		}
		if got.(*internalkinds.LocalUser).Status.LastLoginAt != nil {
			t.Error("Apply wrote status")
		}
		var sec corev1.Secret
		if err := shared.admin.Get(ctx, client.ObjectKey{Namespace: "kudurru-system", Name: "kudurru-localuser.admin"}, &sec); err != nil {
			t.Fatal(err)
		}
		if sec.Labels[InternalKindLabel] != "LocalUser" || sec.Type != InternalSecretType || !bytes.Contains(sec.Data[InternalObjectKey], []byte(`"passwordHash"`)) {
			t.Fatalf("backing secret: labels %v type %s data %s", sec.Labels, sec.Type, sec.Data[InternalObjectKey])
		}
		if sec.ResourceVersion != got.GetResourceVersion() {
			t.Errorf("object rv %s != secret rv %s", got.GetResourceVersion(), sec.ResourceVersion)
		}
		var back internalkinds.LocalUser
		if err := s.Get(ctx, store.Key{Kind: internalkinds.KindLocalUser, Name: "admin"}, &back); err != nil {
			t.Fatal(err)
		}
		if !back.Spec.BuiltinAdmin || back.GetResourceVersion() != got.GetResourceVersion() || back.Kind != "LocalUser" {
			t.Fatalf("Get: %+v", back)
		}
		if err := s.Get(ctx, store.Key{Kind: internalkinds.KindLocalUser, Name: "nobody"}, &back); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing user: %v", err)
		}
	})

	t.Run("apply contract", func(t *testing.T) {
		var u internalkinds.LocalUser
		if err := s.Get(ctx, store.Key{Kind: internalkinds.KindLocalUser, Name: "admin"}, &u); err != nil {
			t.Fatal(err)
		}
		// Identical re-apply: no write.
		again, err := s.Apply(ctx, u.DeepCopy(), store.ApplyOptions{Manager: store.ManagerSystem})
		if err != nil {
			t.Fatal(err)
		}
		if again.GetResourceVersion() != u.ResourceVersion {
			t.Errorf("identical re-apply moved rv %s -> %s", u.ResourceVersion, again.GetResourceVersion())
		}
		// Another manager is refused, Force takes over.
		other := u.DeepCopy()
		other.Spec.DisplayName = "Admin"
		if _, err := s.Apply(ctx, other, store.ApplyOptions{Manager: store.ManagerAPI}); !store.IsManagedBy(err) {
			t.Fatalf("foreign manager: %v", err)
		}
		// Stale resourceVersion is a conflict.
		stale := u.DeepCopy()
		stale.Spec.DisplayName = "x"
		if _, err := s.Apply(ctx, stale, store.ApplyOptions{Manager: store.ManagerSystem, ExpectedResourceVersion: "1"}); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("stale rv: %v", err)
		}
		// A spec change bumps generation.
		changed, err := s.Apply(ctx, other, store.ApplyOptions{Manager: store.ManagerSystem, ExpectedResourceVersion: u.ResourceVersion})
		if err != nil {
			t.Fatal(err)
		}
		if changed.GetGeneration() != 2 {
			t.Errorf("generation %d after spec change, want 2", changed.GetGeneration())
		}
		// Status writes keep spec and do not bump generation.
		st := changed.DeepCopyObject().(*internalkinds.LocalUser)
		st.Spec.DisplayName = "ignored"
		st.Status.LastLoginAt = &metav1.Time{Time: time.Unix(1700000000, 0)}
		if err := s.UpdateStatus(ctx, st); err != nil {
			t.Fatal(err)
		}
		var after internalkinds.LocalUser
		if err := s.Get(ctx, store.Key{Kind: internalkinds.KindLocalUser, Name: "admin"}, &after); err != nil {
			t.Fatal(err)
		}
		if after.Spec.DisplayName != "Admin" || after.Status.LastLoginAt == nil || after.Generation != 2 {
			t.Fatalf("after status update: spec %q status %+v gen %d", after.Spec.DisplayName, after.Status, after.Generation)
		}
		// Apply keeps the stored status.
		re := after.DeepCopy()
		re.Status = internalkinds.LocalUserStatus{}
		re.Spec.Groups = []string{"ops"}
		res, err := s.Apply(ctx, re, store.ApplyOptions{Manager: store.ManagerSystem})
		if err != nil {
			t.Fatal(err)
		}
		if res.(*internalkinds.LocalUser).Status.LastLoginAt == nil {
			t.Error("Apply dropped the stored status")
		}
	})

	t.Run("validation", func(t *testing.T) {
		bad := &internalkinds.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "dns"}, Spec: internalkinds.RoleBindingSpec{Role: "admin"}}
		_, err := s.Apply(ctx, bad, store.ApplyOptions{Manager: store.ManagerAPI})
		var verr *validator.Error
		if !errors.As(err, &verr) || len(verr.Causes) == 0 || verr.Causes[0].Field != "spec.subjects" {
			t.Fatalf("invalid binding: %v", err)
		}
		long := &internalkinds.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("a", 250), Namespace: "dns"}, Spec: internalkinds.RoleBindingSpec{
			Role: "admin", Subjects: []internalkinds.BindingSubject{{Kind: "User", Name: "x"}},
		}}
		if _, err := s.Apply(ctx, long, store.ApplyOptions{Manager: store.ManagerAPI}); !errors.As(err, &verr) {
			t.Fatalf("over-long name: %v", err)
		}
	})

	t.Run("namespaced kinds, list and watch", func(t *testing.T) {
		for _, ns := range []string{"dns", "apps"} {
			rb := &internalkinds.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "ops", Namespace: ns, Labels: map[string]string{"team": ns}}, Spec: internalkinds.RoleBindingSpec{
				Role: "operator", Subjects: []internalkinds.BindingSubject{{Kind: "Group", Name: "ops-" + ns}},
			}}
			if _, err := s.Apply(ctx, rb, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
				t.Fatal(err)
			}
			if ev := next(t, w); ev.Type != store.Added || ev.Object.GetNamespace() != ns {
				t.Fatalf("watch: %s %v", ev.Type, ev.Object)
			}
		}
		all, err := s.List(ctx, internalkinds.KindRoleBinding, store.ListOptions{})
		if err != nil || len(all.Items) != 2 {
			t.Fatalf("list: %d %v", len(all.Items), err)
		}
		dns, err := s.List(ctx, internalkinds.KindRoleBinding, store.ListOptions{Namespace: "dns"})
		if err != nil || len(dns.Items) != 1 || dns.Items[0].GetNamespace() != "dns" {
			t.Fatalf("list dns: %v %v", dns.Items, err)
		}
		sel, err := s.List(ctx, internalkinds.KindRoleBinding, store.ListOptions{LabelSelector: map[string]string{"team": "apps"}})
		if err != nil || len(sel.Items) != 1 || sel.Items[0].GetNamespace() != "apps" {
			t.Fatalf("label selector: %v %v", sel.Items, err)
		}
		page, err := s.List(ctx, internalkinds.KindRoleBinding, store.ListOptions{Limit: 1})
		if err != nil || len(page.Items) != 1 || page.Continue == "" {
			t.Fatalf("page 1: %v %q %v", page.Items, page.Continue, err)
		}
		page2, err := s.List(ctx, internalkinds.KindRoleBinding, store.ListOptions{Limit: 1, Continue: page.Continue})
		if err != nil || len(page2.Items) != 1 || store.KeyOf(page2.Items[0]) == store.KeyOf(page.Items[0]) {
			t.Fatalf("page 2: %v %v", page2.Items, err)
		}
		// Other kinds stay out of RoleBinding lists and watches.
		if l, _ := s.List(ctx, internalkinds.KindLocalUser, store.ListOptions{}); len(l.Items) != 1 {
			t.Fatalf("users: %d", len(l.Items))
		}

		if err := s.Delete(ctx, store.Key{Kind: internalkinds.KindRoleBinding, Namespace: "dns", Name: "ops"}, store.DeleteOptions{Manager: store.ManagerSystem}); !store.IsManagedBy(err) {
			t.Fatalf("foreign delete: %v", err)
		}
		if err := s.Delete(ctx, store.Key{Kind: internalkinds.KindRoleBinding, Namespace: "dns", Name: "ops"}, store.DeleteOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
		if ev := next(t, w); ev.Type != store.Deleted || ev.Object.GetNamespace() != "dns" || ev.Object.(*internalkinds.RoleBinding).Spec.Role != "operator" {
			t.Fatalf("delete event: %s %v", ev.Type, ev.Object)
		}
		if err := s.Delete(ctx, store.Key{Kind: internalkinds.KindRoleBinding, Namespace: "dns", Name: "ops"}, store.DeleteOptions{}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("second delete: %v", err)
		}
	})

	t.Run("a new watch replays current objects", func(t *testing.T) {
		w2, err := s.Watch(ctx, internalkinds.KindRoleBinding, store.WatchOptions{Namespace: "apps"})
		if err != nil {
			t.Fatal(err)
		}
		defer w2.Stop()
		if ev := next(t, w2); ev.Type != store.Added || ev.Object.GetName() != "ops" {
			t.Fatalf("replay: %s %v", ev.Type, ev.Object)
		}
		if ev := next(t, w2); ev.Type != store.Bookmark {
			t.Fatalf("after replay: %s", ev.Type)
		}
	})

	t.Run("snapshot strips internal kinds when safe", func(t *testing.T) {
		var full, safe bytes.Buffer
		if err := s.Snapshot(ctx, &full, store.SnapshotOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := s.Snapshot(ctx, &safe, store.SnapshotOptions{Safe: true}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(full.String(), "kind: LocalUser") || !strings.Contains(full.String(), "kind: RoleBinding") {
			t.Errorf("full snapshot lacks internal kinds:\n%s", full.String())
		}
		if strings.Contains(safe.String(), "LocalUser") || strings.Contains(safe.String(), "passwordHash") {
			t.Errorf("safe snapshot leaks internal kinds:\n%s", safe.String())
		}
	})
}

func next(t *testing.T, w store.Watcher) store.Event {
	t.Helper()
	select {
	case ev, ok := <-w.ResultChan():
		if !ok {
			t.Fatal("watch closed")
		}
		return ev
	case <-time.After(20 * time.Second):
		t.Fatal("no watch event within 20s")
	}
	return store.Event{}
}
