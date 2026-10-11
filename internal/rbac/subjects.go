package rbac

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	internalv1alpha1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
)

// Grant is one resolved (role, scope) pair: the unit of a session's roles
// snapshot and of an API token's frozen grants and scopes. Its wire form
// is "role" for cluster scope and "role@kind:name" otherwise, identical to
// auth.Grant (which mirrors it to stay a leaf package).
type Grant struct {
	Role  string
	Scope Scope
}

// String renders the wire form.
func (g Grant) String() string {
	if g.Scope.Kind == "" || g.Scope.Kind == ScopeCluster {
		return g.Role
	}
	return g.Role + "@" + string(g.Scope.Kind) + ":" + g.Scope.Name
}

// normalized returns g with an explicit cluster scope kind.
func (g Grant) normalized() Grant {
	if g.Scope.Kind == "" || g.Scope.Kind == ScopeCluster {
		g.Scope = Scope{Kind: ScopeCluster}
	}
	return g
}

// ParseGrant parses the wire form produced by Grant.String.
func ParseGrant(s string) (Grant, error) {
	role, scope, scoped := strings.Cut(s, "@")
	if role == "" {
		return Grant{}, fmt.Errorf("rbac: grant %q has no role", s)
	}
	if !scoped {
		return Grant{Role: role, Scope: Scope{Kind: ScopeCluster}}, nil
	}
	kind, name, ok := strings.Cut(scope, ":")
	if !ok || kind == "" || name == "" || kind == string(ScopeCluster) {
		return Grant{}, fmt.Errorf("rbac: grant %q: scope must be <kind>:<name>", s)
	}
	return Grant{Role: role, Scope: Scope{Kind: ScopeKind(kind), Name: name}}, nil
}

// ParseGrants parses a list of wire-form grants; the first malformed entry
// fails the whole list.
func ParseGrants(list []string) ([]Grant, error) {
	out := make([]Grant, 0, len(list))
	for _, s := range list {
		g, err := ParseGrant(s)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, nil
}

// SubjectMatches reports whether one binding subject names p. It is the
// single subject-matching rule of kudurru (the followup of the auth spike):
//
//   - User matches p.Name; an empty subject or principal name never
//     matches.
//   - Group matches one of p.Groups; an empty group never matches.
//   - ServiceAccount matches p.ServiceAccount ("<namespace>/<name>"), set
//     for kudurru service accounts and Kubernetes service accounts alike.
func SubjectMatches(s Subject, p Principal) bool {
	switch s.Kind {
	case SubjectUser:
		return s.Name != "" && p.Name != "" && s.Name == p.Name
	case SubjectGroup:
		return s.Name != "" && slices.Contains(p.Groups, s.Name)
	case SubjectServiceAccount:
		return s.Name != "" && p.ServiceAccount != "" && s.Namespace+"/"+s.Name == p.ServiceAccount
	}
	return false
}

// SubjectsMatch reports whether any subject names p.
func SubjectsMatch(subjects []Subject, p Principal) bool {
	for _, s := range subjects {
		if SubjectMatches(s, p) {
			return true
		}
	}
	return false
}

// GrantsFor returns the grants of every binding whose subjects match p,
// de-duplicated, in binding order. The OIDC login resolver seals them into
// the session (D48).
func GrantsFor(bindings []Binding, p Principal) []Grant {
	var out []Grant
	for _, b := range bindings {
		if !SubjectsMatch(b.Subjects, p) {
			continue
		}
		g := Grant{Role: b.Role, Scope: b.Scope}.normalized()
		if !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	return out
}

// ParseBindings decodes rbac.bindings[] entries of the config file
// (maps in the RoleBinding spec shape) strictly: unknown fields are
// errors, so a typo does not silently drop a subject.
func ParseBindings(entries []map[string]any) ([]Binding, error) {
	var errs []error
	out := make([]Binding, 0, len(entries))
	for i, e := range entries {
		raw, err := json.Marshal(e)
		if err != nil {
			errs = append(errs, fmt.Errorf("rbac.bindings[%d]: %w", i, err))
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		var spec internalv1alpha1.RoleBindingSpec
		if err := dec.Decode(&spec); err != nil {
			errs = append(errs, fmt.Errorf("rbac.bindings[%d]: %w", i, err))
			continue
		}
		rb := internalv1alpha1.RoleBinding{Spec: spec}
		rb.Name, rb.Namespace = fmt.Sprintf("config-%d", i), "config"
		if err := rb.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("rbac.bindings[%d]: %w", i, err))
			continue
		}
		out = append(out, FromRoleBinding(&rb))
	}
	return out, errors.Join(errs...)
}

// FromRoleBinding converts a RoleBinding object.
func FromRoleBinding(rb *internalv1alpha1.RoleBinding) Binding {
	b := Binding{Role: rb.Spec.Role, Scope: Scope{Kind: ScopeKind(rb.Spec.Scope.Kind), Name: rb.Spec.Scope.Name}}
	if b.Scope.Kind == "" {
		b.Scope.Kind = ScopeCluster
	}
	for _, s := range rb.Spec.Subjects {
		b.Subjects = append(b.Subjects, Subject{Kind: SubjectKind(s.Kind), Name: s.Name, Namespace: s.Namespace})
	}
	return b
}
