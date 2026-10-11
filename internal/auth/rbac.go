package auth

import (
	"github.com/robertherbaugh/kudurru/internal/rbac"
)

// RBACPrincipal maps an authenticated principal to the identity the
// authorizer evaluates (the Authorize step of §8.8):
//
//   - Name and Groups match User and Group subjects; a KubernetesServiceAccount
//     keeps its "system:serviceaccount:<ns>:<name>" name and groups for
//     SubjectAccessReview;
//   - ServiceAccount ("<namespace>/<name>") matches ServiceAccount subjects
//     for kudurru and Kubernetes service accounts alike;
//   - Grants (a cookie session's roles snapshot, an OIDC user's frozen
//     token grants) are honoured by rbac.BindingSource only while a live
//     binding still provides them;
//   - Scopes become Restrict (API token scopes narrow every decision);
//   - BuiltinAdmin is the implicit admin (unix-socket peer, first-boot
//     admin, bootstrap service account; Decision.Path "local").
//
// Malformed grants are dropped rather than failing the request: a grant
// that does not parse can never authorize anything. Malformed scopes,
// however, restrict to nothing, so a token with a corrupted scope list is
// denied rather than widened.
func RBACPrincipal(p Principal) rbac.Principal {
	out := rbac.Principal{
		Name:           p.Name,
		Groups:         p.Groups,
		ServiceAccount: p.ServiceAccount,
		Kind:           string(p.Kind),
		UID:            p.UID,
		GrantsIssuedAt: p.GrantsIssuedAt,
		BuiltinAdmin:   p.BuiltinAdmin,
	}
	for _, s := range p.Grants {
		if g, err := rbac.ParseGrant(s); err == nil {
			out.Grants = append(out.Grants, g)
		}
	}
	for _, s := range p.Implicit {
		if g, err := rbac.ParseGrant(s); err == nil {
			out.Implicit = append(out.Implicit, g)
		}
	}
	if len(p.Scopes) > 0 {
		for _, s := range p.Scopes {
			if g, err := rbac.ParseGrant(s); err == nil {
				out.Restrict = append(out.Restrict, g)
			}
		}
		if len(out.Restrict) == 0 {
			// Every scope was malformed: restrict to a role that grants
			// nothing instead of dropping the restriction.
			out.Restrict = []rbac.Grant{{Role: "\x00none", Scope: rbac.Scope{Kind: rbac.ScopeCluster}}}
		}
	}
	return out
}
