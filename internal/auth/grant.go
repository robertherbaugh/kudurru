package auth

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ScopeCluster is the scope kind of a grant that applies everywhere. It
// mirrors rbac.ScopeCluster without importing rbac, so auth stays a leaf.
const ScopeCluster = "cluster"

// Grant is one resolved role binding: the unit stored in
// Session.RolesSnapshot (decision D48). A session carries the grants that
// matched at login time, never the raw IdP group list, so its size is
// bounded by the number of bindings rather than by group membership.
type Grant struct {
	Role      string
	ScopeKind string // "cluster" (or empty) | "zone" | "zonegroup" | "clientgroup"
	ScopeName string // empty for cluster scope
}

// String renders the wire form used in RolesSnapshot: "admin" for a
// cluster-scoped grant and "zone-admin@zone:home-example-com" otherwise.
func (g Grant) String() string {
	if g.ScopeKind == "" || g.ScopeKind == ScopeCluster {
		return g.Role
	}
	return g.Role + "@" + g.ScopeKind + ":" + g.ScopeName
}

// IsCluster reports whether the grant applies cluster-wide.
func (g Grant) IsCluster() bool { return g.ScopeKind == "" || g.ScopeKind == ScopeCluster }

var errEmptyGrant = errors.New("auth: empty grant")

// ParseGrant parses the wire form produced by Grant.String. Cluster grants
// come back with ScopeKind "cluster".
func ParseGrant(s string) (Grant, error) {
	if s == "" {
		return Grant{}, errEmptyGrant
	}
	role, scope, scoped := strings.Cut(s, "@")
	if role == "" {
		return Grant{}, fmt.Errorf("auth: grant %q has no role", s)
	}
	if !scoped {
		return Grant{Role: role, ScopeKind: ScopeCluster}, nil
	}
	kind, name, ok := strings.Cut(scope, ":")
	if !ok || kind == "" || name == "" || kind == ScopeCluster {
		return Grant{}, fmt.Errorf("auth: grant %q: scope must be <kind>:<name>", s)
	}
	return Grant{Role: role, ScopeKind: kind, ScopeName: name}, nil
}

// GrantStrings renders grants in their wire form, sorted and de-duplicated,
// ready to be stored in Session.RolesSnapshot.
func GrantStrings(grants []Grant) []string {
	if len(grants) == 0 {
		return nil
	}
	out := make([]string, 0, len(grants))
	seen := make(map[string]struct{}, len(grants))
	for _, g := range grants {
		s := g.String()
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// ParseGrants parses a RolesSnapshot; the first malformed entry fails the
// whole snapshot so a tampered or corrupted session never half-authorizes.
func ParseGrants(snapshot []string) ([]Grant, error) {
	out := make([]Grant, 0, len(snapshot))
	for _, s := range snapshot {
		g, err := ParseGrant(s)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, nil
}
