package rbac

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/rbac"
)

func TestKubernetesRules(t *testing.T) {
	p, err := rbac.LoadPermissions()
	if err != nil {
		t.Fatal(err)
	}
	admin := KubernetesRules(p, "admin")
	if want := []PolicyRule{{APIGroups: []string{"kudurru.io"}, Resources: []string{"*"}, Verbs: []string{"*"}}}; !reflect.DeepEqual(admin, want) {
		t.Errorf("admin rules %+v", admin)
	}
	// Every permission of every mapped role is granted by its rules with
	// the verb SubjectAccessReview asks for (named and unnamed requests).
	for role := range p.Kubernetes {
		rules := KubernetesRules(p, role)
		for res, verbs := range p.Resources {
			for _, verb := range verbs {
				for _, named := range []bool{true, false} {
					want := p.Grants(role, res, verb)
					got := rulesAllow(rules, SARResources[res], SARVerb(verb, named))
					if got != want {
						t.Errorf("%s: %s.%s (named=%v): ClusterRole allows %v, role grants %v", role, res, verb, named, got, want)
					}
				}
			}
		}
	}
	// The auditor reads the query log; the built-in view role must not.
	if p.Kubernetes["auditor"].AggregateTo != "" {
		t.Error("auditor aggregates into a built-in role")
	}
	if p.Kubernetes["viewer"].AggregateTo != "view" || p.Kubernetes["operator"].ClusterRole != "editor" {
		t.Errorf("kubernetes mapping %+v", p.Kubernetes)
	}
}

func rulesAllow(rules []PolicyRule, resource, verb string) bool {
	for _, r := range rules {
		if (slices.Contains(r.Resources, resource) || slices.Contains(r.Resources, "*")) && (slices.Contains(r.Verbs, verb) || slices.Contains(r.Verbs, "*")) {
			return true
		}
	}
	return false
}

// TestAggregatedRolesDrift fails when the chart's generated ClusterRoles
// differ from permissions.yaml; run `make rbac-manifests`.
func TestAggregatedRolesDrift(t *testing.T) {
	p, err := rbac.LoadPermissions()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("..", "..", AggregatedRolesPath)
	got, err := os.ReadFile(path) //nolint:gosec // repository file
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, RenderAggregatedRoles(p)) {
		t.Errorf("%s is stale: run `make rbac-manifests`", AggregatedRolesPath)
	}
}
