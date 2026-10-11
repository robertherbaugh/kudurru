package kube

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

var kudurruResources = []string{"dnszones", "dnsrecordsets", "blocklists", "clientgroups", "policies"}

// driverClusterRules is the cluster role the kube driver needs for
// kudurru.io objects (proposed for §5.3; see the spike report). Server-side
// apply sends PATCH, but creating an object through it is also authorized
// as "create".
func driverClusterRules(objectVerbs ...string) []rbacv1.PolicyRule {
	status := make([]string, 0, len(kudurruResources))
	for _, r := range kudurruResources {
		status = append(status, r+"/status")
	}
	return []rbacv1.PolicyRule{
		{APIGroups: []string{v1alpha1.Group}, Resources: kudurruResources, Verbs: objectVerbs},
		{APIGroups: []string{v1alpha1.Group}, Resources: status, Verbs: []string{"update", "patch"}},
		{APIGroups: []string{""}, Resources: []string{"namespaces"}, ResourceNames: []string{"kube-system"}, Verbs: []string{"get"}},
	}
}

// driverLeaseRules is the namespaced role for the driver's Leases.
var driverLeaseRules = []rbacv1.PolicyRule{
	{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"get", "create", "update"}},
}

// grant binds user to fresh roles with the given rules and returns a
// driver authenticated as that user.
func grant(t *testing.T, user string, cluster []rbacv1.PolicyRule) *Store {
	t.Helper()
	ctx := context.Background()
	au, err := shared.env.AddUser(envtest.User{Name: user, Groups: []string{"system:authenticated"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	name := strings.ReplaceAll(user, ":", "-")
	cr := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}, Rules: cluster}
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: user}},
	}
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: electorNamespace, Name: name}, Rules: driverLeaseRules}
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: electorNamespace, Name: name},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: user}},
	}
	for _, o := range []client.Object{cr, crb, role, rb} {
		if err := shared.admin.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = shared.admin.Delete(context.Background(), o) })
	}
	return newStoreOn(t, au.Config(), Options{
		Identity:      name,
		LeaseDuration: testLeaseDuration, RenewDeadline: testRenewDeadline, RetryPeriod: testRetryPeriod,
	})
}

// TestLeastPrivilegeRole runs every driver operation as a user holding only
// driverClusterRules(get, list, watch, create, patch, delete) and
// driverLeaseRules.
func TestLeastPrivilegeRole(t *testing.T) {
	requireEnv(t)
	purge(t, shared.admin)
	s := grant(t, fmt.Sprintf("system:serviceaccount:kudurru-system:least-%d", time.Now().UnixNano()),
		driverClusterRules("get", "list", "watch", "create", "patch", "delete"))
	ctx := context.Background()

	w, err := s.Watch(ctx, "DNSRecordSet", store.WatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	if ev := <-w.ResultChan(); ev.Type != store.Bookmark {
		t.Fatalf("empty watch: want Bookmark first, got %s", ev.Type)
	}
	out, err := s.Apply(ctx, rrset("apps", "nas", nil), store.ApplyOptions{Manager: store.ManagerAPI})
	if err != nil {
		t.Fatalf("Apply (create through SSA): %v", err)
	}
	if ev := <-w.ResultChan(); ev.Type != store.Added {
		t.Fatalf("watch: want Added, got %s", ev.Type)
	}
	if _, err := s.Apply(ctx, rrset("apps", "nas", map[string]string{"x": "y"}), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatalf("Apply (update): %v", err)
	}
	rs := out.(*v1alpha1.DNSRecordSet)
	rs.Status.Names = 1
	rs.ResourceVersion = ""
	if err := s.UpdateStatus(ctx, rs); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	var got v1alpha1.DNSRecordSet
	if err := s.Get(ctx, store.KeyOf(rs), &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := s.List(ctx, "DNSRecordSet", store.ListOptions{Limit: 1}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if !s.WaitForSync(ctx) {
		t.Fatal("informers did not sync")
	}
	if _, err := s.MaterializedSnapshot(ctx); err != nil {
		t.Fatalf("MaterializedSnapshot: %v", err)
	}
	var buf bytes.Buffer
	if err := s.Snapshot(ctx, &buf, store.SnapshotOptions{}); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := s.Delete(ctx, store.KeyOf(rs), store.DeleteOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Restore(ctx, &buf); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	eventually(t, 10*time.Second, "cluster ID via namespaces/kube-system get", func() bool { return s.Info().ClusterID != "" })

	ectx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	e := s.Elector("controller")
	go func() { done <- e.Run(ectx, nil, nil) }()
	eventually(t, 10*time.Second, "leadership with get/create/update on leases", e.IsLeader)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestApplyVerbs pins the verbs server-side apply needs: patch always, and
// create when the object does not exist yet. update is not used for spec
// writes.
func TestApplyVerbs(t *testing.T) {
	requireEnv(t)
	tests := []struct {
		name  string
		verbs []string
	}{
		{"without patch", []string{"get", "list", "watch", "create", "update", "delete"}},
		{"without create", []string{"get", "list", "watch", "patch", "update", "delete"}},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			purge(t, shared.admin)
			s := grant(t, fmt.Sprintf("system:serviceaccount:kudurru-system:verbs-%d-%d", i, time.Now().UnixNano()), driverClusterRules(tt.verbs...))
			_, err := s.Apply(context.Background(), zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI})
			if !apierrors.IsForbidden(err) {
				t.Fatalf("want Forbidden %s, got %v", tt.name, err)
			}
		})
	}
}
