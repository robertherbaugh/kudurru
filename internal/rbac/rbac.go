// Package rbac is the hand-rolled (subject, role, scope) authorizer behind
// the Authorizer interface (dev-docs/design-record.md §8.5). permissions.yaml is
// the single source of roles; the SAR authorizer for Kubernetes identities
// plugs into the same Chain.
package rbac

import (
	"context"
	_ "embed"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

//go:embed permissions.yaml
var permissionsYAML []byte

// ScopeKind is what a binding or attribute set is scoped to.
type ScopeKind string

// Scope kinds. Routes present cluster and zone scopes. A zonegroup binding
// covers every zone whose kudurru.io/zonegroup label names the group
// (D385): the code that resolves the zone of a request states the zone's
// group in Attributes.ZoneGroup, read from the same captured publication or
// transaction as the zone itself. clientgroup is reserved: a binding may name
// one, but no request carries such a scope, so the binding grants nothing.
const (
	ScopeCluster     ScopeKind = "cluster"
	ScopeZone        ScopeKind = "zone"
	ScopeZoneGroup   ScopeKind = "zonegroup"
	ScopeClientGroup ScopeKind = "clientgroup" // reserved, planned for v1.0
)

// Scope narrows a binding.
type Scope struct {
	Kind ScopeKind `json:"kind"`
	Name string    `json:"name,omitempty"`
}

// Covers reports whether a binding scope grants access to an attribute
// scope: cluster covers everything, otherwise kind and name must match. It
// knows nothing of zone membership; authorizers use Admits.
func (s Scope) Covers(a Scope) bool {
	if s.Kind == "" || s.Kind == ScopeCluster {
		return true
	}
	return s.Kind == a.Kind && s.Name == a.Name
}

// Admits reports whether a binding or grant scope covers the target of a:
// Covers, and in addition a zonegroup scope covers a zone attribute whose
// ZoneGroup names the group (D385). A zone in no group (empty ZoneGroup) is
// covered by no zonegroup scope.
func (s Scope) Admits(a Attributes) bool {
	if s.Covers(a.Scope) {
		return true
	}
	return s.Kind == ScopeZoneGroup && s.Name != "" && a.Scope.Kind == ScopeZone && a.Scope.Name != "" && a.ZoneGroup == s.Name
}

// Attributes describe one access decision.
type Attributes struct {
	Resource  string
	Verb      string
	Scope     Scope
	Namespace string
	Name      string
	// ZoneGroup is the zone group of the zone in a zone Scope: the value of
	// its kudurru.io/zonegroup label, empty for none. Only the code that read
	// the zone sets it, from the same captured publication or transaction;
	// a command transaction resolves it again and never trusts a caller's
	// value (D385). It is ignored for other scopes.
	ZoneGroup string
}

// Decision is the outcome; Path records which authorizer granted.
type Decision struct {
	Allowed bool
	Path    string // binding | sar | local | none
	Missing *Attributes
	Reason  string
}

// Principal is the minimal identity the authorizer needs.
type Principal struct {
	Name   string
	Groups []string
	// ServiceAccount is "<namespace>/<name>" for service accounts.
	ServiceAccount string

	// Kind is the auth principal kind (User, ServiceAccount,
	// KubernetesServiceAccount, LocalRoot, Node). The SAR authorizer acts
	// only on KubernetesServiceAccount.
	Kind string
	// UID is passed to SubjectAccessReview for Kubernetes identities.
	UID string
	// Grants are grants resolved earlier: a cookie session's roles
	// snapshot (D48: the session carries no groups) or the frozen grants
	// of an OIDC user's personal token. BindingSource honours each only
	// while a live binding still provides it and it was not revoked after
	// GrantsIssuedAt.
	Grants []Grant
	// GrantsIssuedAt is when Grants were resolved (session iat, token
	// creation).
	GrantsIssuedAt time.Time
	// Restrict, when non-empty, narrows every decision to what one of
	// these grants allows (API token scopes, §8.6).
	Restrict []Grant
	// BuiltinAdmin is the implicit admin of the unix-socket peer, the
	// first-boot admin and the bootstrap service account (Path "local").
	BuiltinAdmin bool
	// Implicit are grants the principal holds by construction, without a
	// binding (Path "local"): the read-only grants of a peer node that
	// presented the internal token.
	Implicit []Grant
}

// Authorizer decides.
type Authorizer interface {
	Authorize(ctx context.Context, p Principal, a Attributes) (Decision, error)
}

// Chain tries authorizers in order; the first Allowed wins (§5.5).
type Chain []Authorizer

// Authorize implements Authorizer.
func (c Chain) Authorize(ctx context.Context, p Principal, a Attributes) (Decision, error) {
	var last Decision
	for _, az := range c {
		d, err := az.Authorize(ctx, p, a)
		if err != nil {
			return Decision{}, err
		}
		if d.Allowed {
			return d, nil
		}
		last = d
	}
	if last.Path == "" {
		last.Path = "none"
	}
	return last, nil
}

// SubjectKind is the type of a binding subject.
type SubjectKind string

// Subject kinds.
const (
	SubjectUser           SubjectKind = "User"
	SubjectGroup          SubjectKind = "Group"
	SubjectServiceAccount SubjectKind = "ServiceAccount"
)

// Subject is one party in a binding.
type Subject struct {
	Kind      SubjectKind `json:"kind"`
	Name      string      `json:"name"`
	Namespace string      `json:"namespace,omitempty"`
}

// Binding grants a role at a scope to subjects.
type Binding struct {
	Role     string    `json:"role"`
	Scope    Scope     `json:"scope"`
	Subjects []Subject `json:"subjects"`
}

// Permissions is the parsed permissions.yaml.
type Permissions struct {
	Resources  map[string][]string       `json:"resources"`
	Roles      map[string][]string       `json:"roles"`
	Scoped     []string                  `json:"scoped"`
	Kubernetes map[string]KubernetesRole `json:"kubernetes"`
	// Reserved names the declared "<resource>.<verb>" permissions that no
	// route or handler checks yet, each with the release that is to use it
	// (D393). The reverse lint (LintVerbUse) fails for a declared verb that
	// is neither used nor reserved, and for a reservation that is in use.
	Reserved map[string]string `json:"reserved"`
}

// KubernetesRole is how a kudurru role appears as a Kubernetes ClusterRole.
type KubernetesRole struct {
	// ClusterRole is the name suffix: <release fullname>-<ClusterRole>.
	ClusterRole string `json:"clusterRole"`
	// AggregateTo is the built-in ClusterRole (view, edit, admin) the role
	// aggregates into; empty for none.
	AggregateTo string `json:"aggregateTo,omitempty"`
}

// LoadPermissions parses and validates the embedded permissions.yaml.
func LoadPermissions() (*Permissions, error) {
	return parsePermissions(permissionsYAML)
}

func parsePermissions(data []byte) (*Permissions, error) {
	var p Permissions
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("rbac: parse permissions.yaml: %w", err)
	}
	for role, k := range p.Kubernetes {
		if _, ok := p.Roles[role]; !ok {
			return nil, fmt.Errorf("rbac: kubernetes: unknown role %q", role)
		}
		if k.ClusterRole == "" {
			return nil, fmt.Errorf("rbac: kubernetes: role %s has no clusterRole", role)
		}
		switch k.AggregateTo {
		case "", "view", "edit", "admin":
		default:
			return nil, fmt.Errorf("rbac: kubernetes: role %s aggregates to %q (want view, edit or admin)", role, k.AggregateTo)
		}
	}
	for role, perms := range p.Roles {
		for _, perm := range perms {
			res, verb, ok := strings.Cut(perm, ".")
			if !ok {
				return nil, fmt.Errorf("rbac: role %s: permission %q is not <resource>.<verb>", role, perm)
			}
			if res == "*" {
				continue
			}
			verbs, known := p.Resources[res]
			if !known {
				return nil, fmt.Errorf("rbac: role %s: unknown resource %q", role, res)
			}
			if verb != "*" && !contains(verbs, verb) {
				return nil, fmt.Errorf("rbac: role %s: resource %s has no verb %q", role, res, verb)
			}
		}
	}
	for perm, note := range p.Reserved {
		res, verb, ok := strings.Cut(perm, ".")
		if !ok || !contains(p.Resources[res], verb) {
			return nil, fmt.Errorf("rbac: reserved: %q is not a declared <resource>.<verb>", perm)
		}
		if !reservedNote.MatchString(note) {
			return nil, fmt.Errorf("rbac: reserved: %s must start with its target release (\"v1.0: ...\"), got %q", perm, note)
		}
	}
	return &p, nil
}

// reservedNote is a reservation's text: the target release, then why.
var reservedNote = regexp.MustCompile(`^v[0-9]+\.[0-9]+: \S`)

// RoleNames lists roles sorted.
func (p *Permissions) RoleNames() []string {
	out := make([]string, 0, len(p.Roles))
	for r := range p.Roles {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// Grants reports whether role includes resource.verb.
func (p *Permissions) Grants(role, resource, verb string) bool {
	for _, perm := range p.Roles[role] {
		res, v, _ := strings.Cut(perm, ".")
		if (res == "*" || res == resource) && (v == "*" || v == verb) {
			return true
		}
	}
	return false
}

// BindingAuthorizer evaluates bindings against the permissions table. It is
// a pure function of its inputs so it can be table-tested.
type BindingAuthorizer struct {
	perms    *Permissions
	bindings []Binding
}

// NewBindingAuthorizer validates bindings against the permissions table.
func NewBindingAuthorizer(p *Permissions, bindings []Binding) (*BindingAuthorizer, error) {
	for i, b := range bindings {
		if _, ok := p.Roles[b.Role]; !ok {
			return nil, fmt.Errorf("rbac: binding %d: unknown role %q", i, b.Role)
		}
		if contains(p.Scoped, b.Role) && (b.Scope.Kind == "" || b.Scope.Kind == ScopeCluster) {
			return nil, fmt.Errorf("rbac: binding %d: role %q requires a non-cluster scope", i, b.Role)
		}
	}
	return &BindingAuthorizer{perms: p, bindings: bindings}, nil
}

// Authorize implements Authorizer.
func (b *BindingAuthorizer) Authorize(_ context.Context, p Principal, a Attributes) (Decision, error) {
	for _, bd := range b.bindings {
		if !bd.Scope.Admits(a) || !matchesSubject(bd.Subjects, p) {
			continue
		}
		if b.perms.Grants(bd.Role, a.Resource, a.Verb) {
			return Decision{Allowed: true, Path: "binding", Reason: "role " + bd.Role}, nil
		}
	}
	missing := a
	return Decision{Allowed: false, Path: "binding", Missing: &missing, Reason: "no binding grants " + a.Resource + "." + a.Verb}, nil
}

func matchesSubject(subjects []Subject, p Principal) bool { return SubjectsMatch(subjects, p) }

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
