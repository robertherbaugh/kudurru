package kube

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

type testActorKey struct{}

// testImpersonate stands in for the serve wiring: the identity travels in
// the context of the call.
func testImpersonate(ctx context.Context) (Impersonation, bool) {
	im, ok := ctx.Value(testActorKey{}).(Impersonation)
	return im, ok
}

func asActor(ctx context.Context, im Impersonation) context.Context {
	return context.WithValue(ctx, testActorKey{}, im)
}

type recordingTransport struct{ got []http.Header }

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.got = append(r.got, req.Header.Clone())
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
}

// The transport sends a marked request as exactly its identity: headers that
// anything else set (a UID, extras, another user) are removed, and unmarked
// requests pass unchanged.
func TestImpersonatingTransportHeaders(t *testing.T) {
	rec := &recordingTransport{}
	rt := impersonating(rec)
	s := &Store{opts: Options{Impersonate: testImpersonate}}
	ctx, im, err := s.writeContext(asActor(context.Background(), Impersonation{User: "kudurru:user:alice", Groups: []string{"kudurru:group:ops", "kudurru:group:dns"}}))
	if err != nil || im == nil {
		t.Fatalf("writeContext: %v %v", im, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, "https://apiserver.invalid/apis/kudurru.io/v1alpha1/dnszones/home", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Impersonate-Uid", "forged")
	req.Header.Set("Impersonate-Extra-Scopes", "forged")
	req.Header.Set("Impersonate-User", "system:admin")
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Impersonate-User") != "system:admin" {
		t.Fatal("RoundTrip modified the caller's request")
	}
	got := rec.got[0]
	if got.Get("Impersonate-User") != "kudurru:user:alice" || !slices.Equal(got.Values("Impersonate-Group"), []string{"kudurru:group:ops", "kudurru:group:dns"}) ||
		got.Get("Impersonate-Uid") != "" || got.Get("Impersonate-Extra-Scopes") != "" {
		t.Fatalf("impersonated headers %v", got)
	}

	plain, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://apiserver.invalid/api/v1/namespaces", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.RoundTrip(plain); err != nil {
		t.Fatal(err)
	}
	if len(rec.got[1]) != 0 {
		t.Fatalf("unmarked request gained headers %v", rec.got[1])
	}

	// No identity for the context: the driver's own; no hook: never marked.
	if ctx, im, err := s.writeContext(context.Background()); err != nil || im != nil || ctx.Value(impersonationKey{}) != nil {
		t.Fatalf("writeContext without an actor: %v %v", im, err)
	}
	none := &Store{}
	if _, im, err := none.writeContext(asActor(context.Background(), Impersonation{User: "kudurru:user:alice"})); err != nil || im != nil {
		t.Fatalf("writeContext without Options.Impersonate: %v %v", im, err)
	}
}

// An identity the apiserver could not receive intact is refused, never
// downgraded to the driver's own identity.
func TestImpersonationRefusesInvalidIdentity(t *testing.T) {
	s := &Store{opts: Options{Impersonate: testImpersonate}}
	// No user (the credential ended during the request): an authentication
	// failure, not a Kubernetes refusal.
	if _, _, err := s.writeContext(asActor(context.Background(), Impersonation{})); !errors.Is(err, store.ErrNoIdentity) || errors.Is(err, store.ErrForbidden) {
		t.Errorf("empty user: err = %v, want ErrNoIdentity only", err)
	}
	for _, im := range []Impersonation{
		{User: "kudurru:user:a\nb"},
		{User: " kudurru:user:a"},
		{User: "kudurru:user:a", Groups: []string{""}},
		{User: "kudurru:user:a", Groups: []string{"ops\x7f"}},
	} {
		if _, _, err := s.writeContext(asActor(context.Background(), im)); !errors.Is(err, store.ErrForbidden) {
			t.Errorf("%q: err = %v, want ErrForbidden", im, err)
		}
	}
}

// client-go's static impersonation would overwrite the per-write identity.
func TestImpersonationRefusesImpersonatingKubeconfig(t *testing.T) {
	cfg := &rest.Config{Host: "https://127.0.0.1:1", Impersonate: rest.ImpersonationConfig{UserName: "someone"}}
	if _, err := New(cfg, Options{Identity: "t", Namespace: "kudurru-system", Impersonate: testImpersonate}); err == nil || !strings.Contains(err.Error(), "already impersonates") {
		t.Fatalf("New with an impersonating kubeconfig: %v", err)
	}
}

// grantImpersonated gives the subject (a User or Group name) the verbs on
// dnsrecordsets and waits until the apiserver's RBAC authorizer honors it.
func grantImpersonated(t *testing.T, kind, subject string, verbs ...string) {
	t.Helper()
	ctx := context.Background()
	name := fmt.Sprintf("imp-%d", time.Now().UnixNano())
	cr := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}, Rules: []rbacv1.PolicyRule{
		{APIGroups: []string{v1alpha1.Group}, Resources: []string{"dnsrecordsets"}, Verbs: verbs},
	}}
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name},
		Subjects:   []rbacv1.Subject{{Kind: kind, APIGroup: rbacv1.GroupName, Name: subject}},
	}
	for _, o := range []client.Object{cr, crb} {
		if err := shared.admin.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = shared.admin.Delete(context.Background(), o) })
	}
}

// D386 against a real apiserver: Apply and Delete of a kudurru.io object run
// as the impersonated identity, so Kubernetes RBAC decides them; a refusal is
// store.ErrForbidden and writes nothing; reads and writes without an
// identity stay on the driver's own.
func TestImpersonatedWritesFollowKubernetesRBAC(t *testing.T) {
	requireEnv(t)
	purge(t, shared.admin)
	s := newTestStore(t, Options{Impersonate: testImpersonate})
	ctx := context.Background()
	n := time.Now().UnixNano()
	alice := Impersonation{User: fmt.Sprintf("kudurru:user:alice-%d", n)}
	ops := Impersonation{User: fmt.Sprintf("kudurru:user:bob-%d", n), Groups: []string{fmt.Sprintf("kudurru:group:ops-%d", n)}}

	// Without Kubernetes RBAC: refused, nothing written.
	_, err := s.Apply(asActor(ctx, alice), rrset("apps", "denied", nil), store.ApplyOptions{Manager: store.ManagerAPI})
	if !errors.Is(err, store.ErrForbidden) || !strings.Contains(err.Error(), alice.User) {
		t.Fatalf("Apply without RBAC: %v, want ErrForbidden naming %s", err, alice.User)
	}
	if err := shared.admin.Get(ctx, client.ObjectKey{Namespace: "apps", Name: "denied"}, &v1alpha1.DNSRecordSet{}); !apierrors.IsNotFound(err) {
		t.Fatalf("refused Apply left an object: %v", err)
	}
	if _, err := s.Apply(asActor(ctx, alice), rrset("apps", "denied", nil), store.ApplyOptions{Manager: store.ManagerAPI, CreateOnly: true}); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("CreateOnly without RBAC: %v", err)
	}

	// The same write without an identity runs as the driver.
	if _, err := s.Apply(ctx, rrset("apps", "system", nil), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatalf("driver-identity Apply: %v", err)
	}
	// Reads stay on the driver's identity even with an identity in ctx.
	if err := s.Get(asActor(ctx, alice), store.Key{Kind: "DNSRecordSet", Namespace: "apps", Name: "system"}, &v1alpha1.DNSRecordSet{}); err != nil {
		t.Fatalf("Get with an identity: %v", err)
	}
	// Delete of the driver's object as alice: refused, the object stays.
	if err := s.Delete(asActor(ctx, alice), store.Key{Kind: "DNSRecordSet", Namespace: "apps", Name: "system"}, store.DeleteOptions{Manager: store.ManagerAPI}); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("Delete without RBAC: %v", err)
	}
	if err := shared.admin.Get(ctx, client.ObjectKey{Namespace: "apps", Name: "system"}, &v1alpha1.DNSRecordSet{}); err != nil {
		t.Fatalf("refused Delete removed the object: %v", err)
	}

	// With RBAC for the user: allowed. Server-side apply needs create+patch.
	grantImpersonated(t, rbacv1.UserKind, alice.User, "create", "patch")
	eventually(t, 10*time.Second, "alice's grant to take effect", func() bool {
		_, err := s.Apply(asActor(ctx, alice), rrset("apps", "allowed", nil), store.ApplyOptions{Manager: store.ManagerAPI})
		return err == nil
	})
	if _, err := s.Apply(asActor(ctx, alice), rrset("apps", "allowed", map[string]string{"edited": "yes"}), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatalf("update with RBAC: %v", err)
	}
	// alice has no delete verb.
	if err := s.Delete(asActor(ctx, alice), store.Key{Kind: "DNSRecordSet", Namespace: "apps", Name: "allowed"}, store.DeleteOptions{Manager: store.ManagerAPI}); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("Delete without the delete verb: %v", err)
	}

	// Groups are impersonated too: bob holds delete only through his group.
	grantImpersonated(t, rbacv1.GroupKind, ops.Groups[0], "delete")
	eventually(t, 10*time.Second, "the group grant to take effect", func() bool {
		err := s.Delete(asActor(ctx, ops), store.Key{Kind: "DNSRecordSet", Namespace: "apps", Name: "allowed"}, store.DeleteOptions{Manager: store.ManagerAPI})
		return err == nil
	})
	if err := shared.admin.Get(ctx, client.ObjectKey{Namespace: "apps", Name: "allowed"}, &v1alpha1.DNSRecordSet{}); !apierrors.IsNotFound(err) {
		t.Fatalf("group-authorized Delete: %v", err)
	}
}

// D386: the managed-fields takeover of a forced write (and its dry run) is
// sent as the impersonated identity too. Without patch it is refused and
// the object keeps its manager and content; with create and patch the
// takeover succeeds.
func TestImpersonatedForceTakeoverFollowsKubernetesRBAC(t *testing.T) {
	requireEnv(t)
	purge(t, shared.admin)
	s := newTestStore(t, Options{Impersonate: testImpersonate})
	ctx := context.Background()
	carol := Impersonation{User: fmt.Sprintf("kudurru:user:carol-%d", time.Now().UnixNano())}
	key := store.Key{Kind: "DNSRecordSet", Namespace: "apps", Name: "owned"}
	if _, err := s.Apply(ctx, rrset("apps", "owned", nil), store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	unchanged := func() {
		t.Helper()
		var got v1alpha1.DNSRecordSet
		if err := shared.admin.Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: key.Name}, &got); err != nil {
			t.Fatal(err)
		}
		if store.ManagedBy(&got) != store.ManagerSystem || got.Labels["taken"] != "" {
			t.Fatalf("refused takeover changed the object: managedBy %q labels %v", store.ManagedBy(&got), got.Labels)
		}
	}
	force := func() error {
		_, err := s.Apply(asActor(ctx, carol), rrset("apps", "owned", map[string]string{"taken": "yes"}), store.ApplyOptions{Manager: store.ManagerAPI, Force: true})
		return err
	}

	// No Kubernetes RBAC: the takeover itself is refused.
	if err := force(); !errors.Is(err, store.ErrForbidden) || !strings.Contains(err.Error(), "take over") || !strings.Contains(err.Error(), carol.User) {
		t.Fatalf("forced Apply without RBAC: %v, want ErrForbidden for the takeover as %s", err, carol.User)
	}
	unchanged()
	// create alone is not enough: the takeover is a patch.
	grantImpersonated(t, rbacv1.UserKind, carol.User, "create")
	time.Sleep(time.Second)
	if err := force(); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("forced Apply with create only: %v, want ErrForbidden", err)
	}
	unchanged()

	grantImpersonated(t, rbacv1.UserKind, carol.User, "patch")
	eventually(t, 10*time.Second, "carol's patch grant to take effect", func() bool { return force() == nil })
	var got v1alpha1.DNSRecordSet
	if err := shared.admin.Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: key.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if store.ManagedBy(&got) != store.ManagerAPI || got.Labels["taken"] != "yes" {
		t.Fatalf("forced Apply with patch: managedBy %q labels %v", store.ManagedBy(&got), got.Labels)
	}
}

// D386: when the driver's own identity lacks the impersonate verb, an
// impersonated write is store.ErrImpersonationDenied (a deployment fault),
// not the principal's ErrForbidden, and the error does not carry the
// apiserver's message naming the driver's identity. Nothing is written.
func TestImpersonationWithoutImpersonateVerb(t *testing.T) {
	requireEnv(t)
	purge(t, shared.admin)
	driver := fmt.Sprintf("system:serviceaccount:kudurru-system:noimp-%d", time.Now().UnixNano())
	s := grantWith(t, driver, driverClusterRules("get", "list", "watch", "create", "patch", "delete"), Options{Impersonate: testImpersonate})
	ctx := context.Background()
	dave := Impersonation{User: fmt.Sprintf("kudurru:user:dave-%d", time.Now().UnixNano())}
	grantImpersonated(t, rbacv1.UserKind, dave.User, "create", "patch", "delete")

	_, err := s.Apply(asActor(ctx, dave), rrset("apps", "noimp", nil), store.ApplyOptions{Manager: store.ManagerAPI})
	if !errors.Is(err, store.ErrImpersonationDenied) || errors.Is(err, store.ErrForbidden) || strings.Contains(err.Error(), driver) {
		t.Fatalf("Apply without the impersonate verb: %v, want ErrImpersonationDenied without %s", err, driver)
	}
	if err := shared.admin.Get(ctx, client.ObjectKey{Namespace: "apps", Name: "noimp"}, &v1alpha1.DNSRecordSet{}); !apierrors.IsNotFound(err) {
		t.Fatalf("refused Apply left an object: %v", err)
	}
	// The same driver still writes as itself without a principal.
	if _, err := s.Apply(ctx, rrset("apps", "noimp", nil), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatalf("driver-identity Apply: %v", err)
	}
	if err := s.Delete(asActor(ctx, dave), store.Key{Kind: "DNSRecordSet", Namespace: "apps", Name: "noimp"}, store.DeleteOptions{Manager: store.ManagerAPI}); !errors.Is(err, store.ErrImpersonationDenied) {
		t.Fatalf("Delete without the impersonate verb: %v", err)
	}
}
