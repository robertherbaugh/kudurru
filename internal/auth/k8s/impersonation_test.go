package k8s_test

import (
	"slices"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/k8s"
)

func TestImpersonationNames(t *testing.T) {
	for _, tc := range []struct {
		name   string
		p      auth.Principal
		user   string
		groups []string
		ok     bool
	}{
		{"kubernetes service account", auth.Principal{Kind: auth.KindKubernetesServiceAccount, Name: "system:serviceaccount:dns:external-dns", Groups: []string{"system:serviceaccounts", "system:serviceaccounts:dns"}},
			"system:serviceaccount:dns:external-dns", []string{"system:serviceaccounts", "system:serviceaccounts:dns"}, true},
		{"kubernetes service account without its name", auth.Principal{Kind: auth.KindKubernetesServiceAccount, Name: "system:admin"}, "", nil, true},
		{"local user", auth.Principal{Kind: auth.KindUser, Name: "admin", Provider: "local"}, "kudurru:user:admin", []string{}, true},
		{"oidc user", auth.Principal{Kind: auth.KindUser, Name: "alice@example.com", Groups: []string{"dns-admins"}, Provider: "corp"}, "kudurru:user:alice@example.com", []string{"kudurru:group:dns-admins"}, true},
		{"user without a name", auth.Principal{Kind: auth.KindUser}, "", nil, true},
		{"kudurru service account", auth.Principal{Kind: auth.KindServiceAccount, Name: "ci", ServiceAccount: "default/ci"}, "kudurru:serviceaccount:default:ci", []string{}, true},
		{"kudurru service account without a namespace", auth.Principal{Kind: auth.KindServiceAccount, Name: "ci"}, "", nil, true},
		{"unix socket", auth.Principal{Kind: auth.KindLocalRoot, Name: "root"}, "", nil, false},
		{"peer node", auth.Principal{Kind: auth.KindNode, Name: "node-b"}, "", nil, false},
	} {
		user, groups, ok := k8s.Impersonation(tc.p)
		if user != tc.user || ok != tc.ok || !slices.Equal(groups, tc.groups) {
			t.Errorf("%s: got (%q, %q, %v), want (%q, %q, %v)", tc.name, user, groups, ok, tc.user, tc.groups, tc.ok)
		}
	}
}
