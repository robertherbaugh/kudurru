package rbac

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// BindingSource is the live binding authorizer: the static bindings of the
// config file (rbac.bindings) plus the RoleBinding objects of the store,
// swapped atomically when either changes (the identity layer feeds
// SetObjects from a Store.Watch-fed RoleBinding cache, internal/auth/stack).
//
// It also honours pre-resolved grants (Principal.Grants: a cookie
// session's roles snapshot or an OIDC user's frozen token grants). Such a
// grant is frozen at login (D48, D71), so BindingSource only honours it
// while
//
//   - some current binding still provides the same role at the same scope
//     (or it is a provider default role registered with AllowGrant), and
//   - no binding change removed that grant from any subject after the
//     grant was resolved (GrantsIssuedAt).
//
// The second rule over-revokes on purpose: the session does not carry the
// groups that matched (D48), so narrowing a binding revokes the grant for
// every session that resolved it earlier, and those users log in again.
// The revocation times are kept in memory on every replica (each one
// watches the bindings); the existence rule is what survives a restart.
type BindingSource struct {
	perms *Permissions
	log   *slog.Logger
	now   func() time.Time

	mu       sync.Mutex // serialises writers
	static   []Binding
	objects  map[string]Binding
	allowed  map[Grant]bool
	onRevoke []func([]Revocation)

	state atomic.Pointer[sourceState]
}

type sourceState struct {
	bindings  []Binding // valid bindings, static first
	granted   map[Grant]bool
	revokedAt map[Grant]time.Time
	pairs     map[pair]bool
	diagnoses []string
}

type pair struct {
	subject Subject
	grant   Grant
}

// Revocation is one (subject, grant) pair a binding change removed.
type Revocation struct {
	Subject Subject
	Grant   Grant
	At      time.Time
}

// SourceOptions configures a BindingSource.
type SourceOptions struct {
	// Logger receives diagnoses of invalid RoleBinding objects.
	Logger *slog.Logger
	// Now is the clock; default time.Now.
	Now func() time.Time
}

// NewBindingSource validates the static bindings (an invalid config file
// binding is a startup error) and returns a source with no objects yet.
func NewBindingSource(p *Permissions, static []Binding, opts SourceOptions) (*BindingSource, error) {
	if p == nil {
		return nil, fmt.Errorf("rbac: binding source needs the permissions table")
	}
	for i, b := range static {
		if err := checkBinding(p, b); err != nil {
			return nil, fmt.Errorf("rbac.bindings[%d]: %w", i, err)
		}
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &BindingSource{perms: p, log: opts.Logger, now: opts.Now, static: slices.Clone(static), objects: map[string]Binding{}, allowed: map[Grant]bool{}}
	s.rebuild()
	return s, nil
}

func checkBinding(p *Permissions, b Binding) error {
	if _, ok := p.Roles[b.Role]; !ok {
		return fmt.Errorf("unknown role %q", b.Role)
	}
	if contains(p.Scoped, b.Role) && (b.Scope.Kind == "" || b.Scope.Kind == ScopeCluster) {
		return fmt.Errorf("role %q requires a non-cluster scope", b.Role)
	}
	if len(b.Subjects) == 0 {
		return fmt.Errorf("no subjects")
	}
	return nil
}

// Permissions returns the permissions table.
func (s *BindingSource) Permissions() *Permissions { return s.perms }

// OnRevoke registers fn to receive the (subject, grant) pairs every binding
// change removes; the user store uses it to bump sessionsValidAfter of
// local users named by removed User subjects.
func (s *BindingSource) OnRevoke(fn func([]Revocation)) {
	s.mu.Lock()
	s.onRevoke = append(s.onRevoke, fn)
	s.mu.Unlock()
}

// AllowGrant registers a grant that is valid without a binding: an OIDC
// provider's provisioning.defaultRole.
func (s *BindingSource) AllowGrant(g Grant) {
	s.mu.Lock()
	s.allowed[g.normalized()] = true
	s.mu.Unlock()
	s.rebuild()
}

// SetObjects replaces the bindings that come from RoleBinding objects,
// keyed by object key. Invalid bindings (unknown or misplaced roles) are
// skipped and reported by Diagnoses.
func (s *BindingSource) SetObjects(objects map[string]Binding) {
	s.mu.Lock()
	s.objects = make(map[string]Binding, len(objects))
	for k, b := range objects {
		s.objects[k] = b
	}
	s.mu.Unlock()
	s.rebuild()
}

func (s *BindingSource) rebuild() {
	s.mu.Lock()
	now := s.now()
	next := &sourceState{granted: map[Grant]bool{}, pairs: map[pair]bool{}, revokedAt: map[Grant]time.Time{}}
	next.bindings = append(next.bindings, s.static...)
	keys := make([]string, 0, len(s.objects))
	for k := range s.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b := s.objects[k]
		if err := checkBinding(s.perms, b); err != nil {
			next.diagnoses = append(next.diagnoses, fmt.Sprintf("RoleBinding %s: %v (ignored)", k, err))
			continue
		}
		next.bindings = append(next.bindings, b)
	}
	for g := range s.allowed {
		next.granted[g] = true
	}
	for _, b := range next.bindings {
		g := Grant{Role: b.Role, Scope: b.Scope}.normalized()
		next.granted[g] = true
		for _, sub := range b.Subjects {
			next.pairs[pair{subject: sub, grant: g}] = true
		}
	}
	prev := s.state.Load()
	var revoked []Revocation
	if prev != nil {
		for g, t := range prev.revokedAt {
			next.revokedAt[g] = t
		}
		for pr := range prev.pairs {
			if !next.pairs[pr] {
				next.revokedAt[pr.grant] = now
				revoked = append(revoked, Revocation{Subject: pr.subject, Grant: pr.grant, At: now})
			}
		}
	}
	s.state.Store(next)
	fns := slices.Clone(s.onRevoke)
	s.mu.Unlock()
	for _, d := range next.diagnoses {
		s.log.Warn("rbac: invalid role binding", "diagnosis", d)
	}
	if len(revoked) > 0 {
		sort.Slice(revoked, func(i, j int) bool { return revoked[i].Grant.String() < revoked[j].Grant.String() })
		for _, fn := range fns {
			fn(revoked)
		}
	}
}

// Bindings returns the current valid bindings (static first).
func (s *BindingSource) Bindings() []Binding { return slices.Clone(s.state.Load().bindings) }

// Diagnoses lists the RoleBinding objects that were ignored and why.
func (s *BindingSource) Diagnoses() []string { return slices.Clone(s.state.Load().diagnoses) }

// GrantsFor resolves the grants of p from the current bindings.
func (s *BindingSource) GrantsFor(p Principal) []Grant { return GrantsFor(s.state.Load().bindings, p) }

// GrantValid reports whether a grant resolved at issuedAt is still honoured.
func (s *BindingSource) GrantValid(g Grant, issuedAt time.Time) bool {
	st := s.state.Load()
	g = g.normalized()
	if !st.granted[g] {
		return false
	}
	if t, ok := st.revokedAt[g]; ok && !issuedAt.After(t) {
		return false
	}
	return true
}

// ValidGrants filters grants through GrantValid.
func (s *BindingSource) ValidGrants(grants []Grant, issuedAt time.Time) []Grant {
	out := make([]Grant, 0, len(grants))
	for _, g := range grants {
		if s.GrantValid(g, issuedAt) {
			out = append(out, g.normalized())
		}
	}
	return out
}

// Authorize implements Authorizer: live bindings matching p, then p's
// pre-resolved grants that are still valid. Path is "binding".
func (s *BindingSource) Authorize(_ context.Context, p Principal, a Attributes) (Decision, error) {
	st := s.state.Load()
	for _, b := range st.bindings {
		if b.Scope.Covers(a.Scope) && SubjectsMatch(b.Subjects, p) && s.perms.Grants(b.Role, a.Resource, a.Verb) {
			return Decision{Allowed: true, Path: "binding", Reason: "role " + b.Role}, nil
		}
	}
	for _, g := range p.Grants {
		if g.Scope.Covers(a.Scope) && s.perms.Grants(g.Role, a.Resource, a.Verb) && s.GrantValid(g, p.GrantsIssuedAt) {
			return Decision{Allowed: true, Path: "binding", Reason: "role " + g.Role + " (session)"}, nil
		}
	}
	missing := a
	return Decision{Allowed: false, Path: "binding", Missing: &missing, Reason: "no binding grants " + a.Resource + "." + a.Verb}, nil
}
