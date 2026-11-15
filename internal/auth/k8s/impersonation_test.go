package k8s_test

import (
	"bytes"
	"context"
	"slices"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/k8s"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
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
		{"local user with groups", auth.Principal{Kind: auth.KindUser, Name: "bob", Groups: []string{"ops"}, Provider: "local"}, "kudurru:user:bob", []string{"kudurru:group:ops"}, true},
		{"oidc user on a bearer JWT", auth.Principal{Kind: auth.KindUser, Name: "corp:alice", Groups: []string{"corp:dns-admins"}, Provider: "corp"}, "kudurru:user:corp:alice", []string{"kudurru:group:corp:dns-admins"}, true},
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

// D48 keeps an OIDC user's groups out of the sealed session (and out of
// personal API tokens), so a dashboard write in impersonate mode carries the
// user's name and no Impersonate-Group: Kubernetes RBAC must bind
// kudurru:user:<name>, and a binding to kudurru:group:<group> does not match
// (D386, docs/architecture/access.md).
func TestImpersonationOfSessionOIDCUserHasNoGroups(t *testing.T) {
	st, err := session.NewSealed(session.Options{Key: bytes.Repeat([]byte{1}, session.KeySize)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p := auth.Principal{Kind: auth.KindUser, ID: "oidc:corp:alice", Name: "corp:alice", Groups: []string{"corp:dns-admins"}, Provider: "corp"}
	cookie, err := st.Issue(ctx, st.NewSession(p, []string{"admin"}, "corp"))
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := st.Load(ctx, cookie)
	if err != nil {
		t.Fatal(err)
	}
	user, groups, ok := k8s.Impersonation(loaded.Principal)
	if !ok || user != "kudurru:user:corp:alice" || len(groups) != 0 {
		t.Fatalf("session OIDC user impersonated as (%q, %q, %v), want kudurru:user:corp:alice with no groups", user, groups, ok)
	}
}
