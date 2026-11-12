package k8s

import (
	"strings"

	"github.com/robertherbaugh/kudurru/internal/auth"
)

// Kubernetes names of kudurru's own principals in impersonate mode (D386).
// The fixed kind segment keeps a user, a group and a kudurru service account
// of the same name apart, and the kudurru: stem keeps every one of them out
// of the system: names the apiserver reserves.
const (
	ImpersonatedUserPrefix           = "kudurru:user:"
	ImpersonatedGroupPrefix          = "kudurru:group:"
	ImpersonatedServiceAccountPrefix = "kudurru:serviceaccount:"
)

// Impersonation returns the Kubernetes user and groups an API write by p is
// sent as when auth.kubernetes.mode is impersonate (D386):
//
//   - a Kubernetes service account is itself: its username
//     (system:serviceaccount:<namespace>:<name>) and the groups from its
//     token;
//   - a local or OIDC user is kudurru:user:<name>, with every group as
//     kudurru:group:<group>;
//   - a kudurru service account (and its API tokens) is
//     kudurru:serviceaccount:<namespace>:<name>, with its groups prefixed
//     the same way.
//
// The unix-socket peer (LocalRoot, the recovery path) and peer nodes are not
// impersonated: ok is false and they write as kudurru itself. A principal of
// an impersonated kind whose name is missing yields an empty user, which the
// store refuses instead of writing as kudurru.
func Impersonation(p auth.Principal) (user string, groups []string, ok bool) {
	switch p.Kind {
	case auth.KindKubernetesServiceAccount:
		if !strings.HasPrefix(p.Name, ServiceAccountPrefix) {
			return "", nil, true
		}
		return p.Name, append([]string(nil), p.Groups...), true
	case auth.KindUser:
		if p.Name == "" {
			return "", nil, true
		}
		return ImpersonatedUserPrefix + p.Name, prefixed(p.Groups), true
	case auth.KindServiceAccount:
		ns, name, found := strings.Cut(p.ServiceAccount, "/")
		if !found || ns == "" || name == "" {
			return "", nil, true
		}
		return ImpersonatedServiceAccountPrefix + ns + ":" + name, prefixed(p.Groups), true
	}
	return "", nil, false
}

func prefixed(groups []string) []string {
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		out = append(out, ImpersonatedGroupPrefix+g)
	}
	return out
}
