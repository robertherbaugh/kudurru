package kube

import (
	"fmt"
	"reflect"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// Use the real apiserver directly so the store's embedded validator cannot
// conceal a stale or more permissive deployed CRD.
func TestBlockListClientGroupScopeAdmission(t *testing.T) {
	requireEnv(t)
	purge(t, shared.admin)
	makeList := func(name string, groups []string) *v1alpha1.BlockList {
		return &v1alpha1.BlockList{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "apps"}, Spec: v1alpha1.BlockListSpec{ClientGroupRefs: groups, Sources: []v1alpha1.ListSource{{Inline: []string{"scope.example"}}}}}
	}
	many := make([]string, 65)
	for i := range many {
		many[i] = fmt.Sprintf("group-%d", i)
	}
	for _, tc := range []struct {
		name   string
		groups []string
	}{
		{"ui-allow-kids", []string{"adults"}},
		{"ui-deny", []string{"kids"}},
		{"duplicate-scope", []string{"kids", "kids"}},
		{"invalid-name", []string{"UPPERCASE"}},
		{"too-many", many},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := makeList(tc.name, tc.groups)
			if err := shared.admin.Create(t.Context(), obj); !apierrors.IsInvalid(err) {
				t.Fatalf("expected CRD admission refusal, got %v", err)
			}
			var back v1alpha1.BlockList
			if err := shared.admin.Get(t.Context(), client.ObjectKeyFromObject(obj), &back); !apierrors.IsNotFound(err) {
				t.Fatal("rejected scope created an object")
			}
		})
	}
	for _, tc := range []struct {
		name   string
		groups []string
	}{
		{"explicit-scope", []string{"kids", "guests"}},
		{"ui-allow-kids", []string{"kids"}},
		{"ui-deny", nil},
	} {
		obj := makeList(tc.name, tc.groups)
		if err := shared.admin.Create(t.Context(), obj); err != nil {
			t.Fatal(err)
		}
		var back v1alpha1.BlockList
		if err := shared.admin.Get(t.Context(), client.ObjectKeyFromObject(obj), &back); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(back.Spec.ClientGroupRefs, tc.groups) {
			t.Fatal("apiserver pruned or changed accepted group scope")
		}
	}
}
