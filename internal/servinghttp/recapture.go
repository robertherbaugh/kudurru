package servinghttp

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// ErrNotRecaptured reports that a request's command may not capture again
// (D399): its authority, credential, session or roles differ in the current
// publication, or the request carries no recapturable lease. The caller keeps
// its original refusal.
var ErrNotRecaptured = errors.New("servinghttp: request not recaptured")

// requestCell holds the lease of one HTTP request. It starts with the lease
// captured before authentication. Recapture may replace it once a command of
// the request was refused before dispatch because another commit superseded
// it (D399); every lease the request held stays retained until the request
// ends, because owner values read from it must not be released under the
// handler.
type requestCell struct {
	mu      sync.Mutex
	current *serving.Lease
	held    []*serving.Lease
}

func newRequestCell(lease *serving.Lease) *requestCell {
	return &requestCell{current: lease, held: []*serving.Lease{lease}}
}

func (c *requestCell) lease() *serving.Lease {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

func (c *requestCell) replace(lease *serving.Lease) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current = lease
	c.held = append(c.held, lease)
}

// release ends every lease the request held.
func (c *requestCell) release() {
	c.mu.Lock()
	held := c.held
	c.held, c.current = nil, nil
	c.mu.Unlock()
	for _, lease := range held {
		lease.Release()
	}
}

// requestLease returns the request's current lease: the one captured before
// authentication, or the one a pre-dispatch recapture replaced it with.
func requestLease(ctx context.Context) (*serving.Lease, bool) {
	switch v := ctx.Value(leaseKey{}).(type) {
	case *serving.Lease:
		return v, v != nil
	case *requestCell:
		lease := v.lease()
		return lease, lease != nil
	}
	return nil, false
}

type exactSourceKey struct{}

// WithExactSource marks a command whose admission was decided from the exact
// source digest that the request was captured on (a reviewed cluster
// command whose transaction does not compare it again). Recapture of such a
// request admits only a new publication of the same source generation.
func WithExactSource(ctx context.Context) context.Context {
	return context.WithValue(ctx, exactSourceKey{}, true)
}

// Recapture replaces the request's lease with the current publication after
// one of the request's commands was refused before its transaction body
// began, only because another commit superseded the publication the request
// was captured on (D399). Nothing was dispatched, so this is not a replay:
// the root dispatches the command once on the new capture, and the command
// transaction repeats its own authorization and every source check against
// it.
//
// Recapture waits for the next complete publication, bounded by wait. It
// then requires that the new publication's authority is the original one
// advanced only in its installed revision, that the original raw credential
// still verifies in the new publication's auth view as the same principal,
// with the same session (ID, CSRF secret, issue and expiry) and the same
// effective roles, and that an unauthenticated request stays
// unauthenticated. Any difference returns ErrNotRecaptured and leaves the
// request's lease unchanged, so the caller answers its original refusal.
// The superseded lease stays held until the request ends.
func Recapture(ctx, wait context.Context, publisher *serving.Publisher, now func() time.Time) error {
	if ctx == nil || wait == nil || publisher == nil || now == nil {
		return serving.ErrComponents
	}
	cell, ok := ctx.Value(leaseKey{}).(*requestCell)
	if !ok || cell == nil {
		return ErrNotRecaptured
	}
	original := cell.lease()
	if original == nil || original.Bundle() == nil {
		return serving.ErrReleased
	}
	before, err := original.SourceToken()
	if err != nil {
		return err
	}
	for {
		changed, authority := publisher.Subscribe(), publisher.AuthorityChanges()
		lease, err := publisher.CaptureHTTP(wait)
		switch {
		case err == nil && lease.Bundle() != original.Bundle():
			if err := recaptureAdmits(ctx, original, lease, before, now()); err != nil {
				lease.Release()
				return err
			}
			cell.replace(lease)
			return nil
		case err == nil:
			// The publication the command was refused on is current again;
			// wait for its successor instead of dispatching on it.
			lease.Release()
		case !errors.Is(err, serving.ErrPending):
			return err
		}
		select {
		case <-wait.Done():
			return wait.Err()
		case <-changed:
		case <-authority:
		}
	}
}

// recaptureAdmits compares the new capture with the request's original one.
func recaptureAdmits(ctx context.Context, original, next *serving.Lease, before sqlite.GenerationToken, at time.Time) error {
	after, err := next.SourceToken()
	if err != nil {
		return err
	}
	if !before.AdvancedTo(after.Authority()) {
		return ErrNotRecaptured
	}
	if exact, _ := ctx.Value(exactSourceKey{}).(bool); exact && (!before.Generation().Same(after.Generation()) || before.Generation().Digest != after.Generation().Digest) {
		return ErrNotRecaptured
	}
	oldView, ok := original.Bundle().Component("auth")
	newView, found := next.Bundle().Component("auth")
	was, typed := oldView.(*servingauth.View)
	now, current := newView.(*servingauth.View)
	if !ok || !found || !typed || !current || was == nil || now == nil {
		return ErrNotRecaptured
	}
	p, authenticated := ctx.Value(proofKey{}).(proof)
	if !authenticated {
		// An unauthenticated request (sign-in) proves its credential inside
		// the command transaction; nothing of the old view carries over.
		return nil
	}
	if p.holder != ctx.Value(leaseKey{}) {
		return ErrNotRecaptured
	}
	first, err := verifiedAs(ctx, was, p.credential, at)
	if err != nil {
		return ErrNotRecaptured
	}
	second, err := verifiedAs(ctx, now, p.credential, at)
	if err != nil || !sameVerified(first, second) {
		return ErrNotRecaptured
	}
	return nil
}

// verified is what one auth view proves about a request's raw credential.
type verified struct {
	principal auth.Principal
	session   *auth.Session
	roles     []string
}

func verifiedAs(ctx context.Context, v *servingauth.View, credential servingauth.Credential, at time.Time) (verified, error) {
	var out verified
	if credential.Kind == servingauth.Cookie {
		sess, err := v.CookieSession(credential.Value, at)
		if err != nil {
			return verified{}, err
		}
		out.principal, out.session = sess.Principal, &sess
	} else {
		principal, err := v.Authenticate(ctx, credential, at)
		if err != nil {
			return verified{}, err
		}
		out.principal = principal
	}
	roles, err := v.Roles(out.principal, at)
	if err != nil {
		return verified{}, err
	}
	out.roles = slices.Clone(roles)
	slices.Sort(out.roles)
	return out, nil
}

// sameVerified compares the identity, session and effective roles of two
// verifications of one credential. Verification time and private provider
// evidence are not compared: the new view verified the credential itself.
// A session's idle expiry may advance; its identity, CSRF secret, issue and
// absolute expiry may not.
func sameVerified(a, b verified) bool {
	p, q := a.principal, b.principal
	if p.Kind != q.Kind || p.ID != q.ID || p.Name != q.Name || !slices.Equal(p.Groups, q.Groups) || p.Provider != q.Provider ||
		p.AuthMethod != q.AuthMethod || p.AccountUID != q.AccountUID || (p.External == nil) != (q.External == nil) || p.UID != q.UID ||
		p.ServiceAccount != q.ServiceAccount || p.BuiltinAdmin != q.BuiltinAdmin || p.MustChangePassword != q.MustChangePassword ||
		!slices.Equal(p.Grants, q.Grants) || !slices.Equal(p.Scopes, q.Scopes) || p.TokenID != q.TokenID ||
		!p.CredentialIssuedAt.Equal(q.CredentialIssuedAt) || !slices.Equal(p.Implicit, q.Implicit) || !slices.Equal(a.roles, b.roles) {
		return false
	}
	if (a.session == nil) != (b.session == nil) {
		return false
	}
	if a.session != nil {
		s, t := a.session, b.session
		if s.ID != t.ID || s.CSRF != t.CSRF || s.IDP != t.IDP || !s.IssuedAt.Equal(t.IssuedAt) || !s.ExpiresAt.Equal(t.ExpiresAt) || !slices.Equal(s.RolesSnapshot, t.RolesSnapshot) {
			return false
		}
	}
	return true
}
