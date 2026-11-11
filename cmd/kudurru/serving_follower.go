package main

import (
	"context"
	"errors"
	"net/http"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// ClusterRole reports whether a request began on an enrolled follower, from
// the source its lease captured before authentication. It reads no store and
// captures nothing, and it still answers after the request's context ended.
// The API mounts it as api.Options.ClusterRole: every mutation that the
// follower table does not name as forwarded or local is refused with
// primary_required before its handler (D368).
func (s *servingRuntime) ClusterRole(ctx context.Context) api.ClusterRole {
	if s == nil || ctx == nil {
		return api.ClusterRole{}
	}
	mode, primary, err := servinghttp.SourceRole(ctx)
	if err != nil || mode != replication.Follower {
		return api.ClusterRole{}
	}
	return api.ClusterRole{Follower: true, Primary: primary}
}

// primaryRequired is the command-admission half of the follower refusal. The
// router refuses unforwarded mutations first; a command that still reaches a
// writer seam on a request that began on a follower (a safe-method route with
// an effect, or a route mounted outside the API) gets the same typed answer
// instead of the generic fenced-writer one. It returns nil for a request that
// began in any other role.
func (s *servingRuntime) primaryRequired(ctx context.Context) error {
	if role := s.ClusterRole(ctx); role.Follower {
		return api.PrimaryRequired(role.Primary)
	}
	return nil
}

// storePrimaryRequired answers for a request that holds no lease yet (the
// browser and CLI login admission runs before capture). The sampled authority
// selects the refusal text only: the request is refused in either case.
func (s *servingRuntime) storePrimaryRequired(hint string) (*api.Error, bool) {
	if s == nil || s.opts.Store == nil {
		return nil, false
	}
	authority, blocked := s.opts.Store.ServingAuthority()
	if blocked || authority.Mode != replication.Follower {
		return nil, false
	}
	return api.PrimaryRequiredWithHint(authority.Term.PrimaryName, hint), true
}

// asPrimaryRequired finds the typed follower refusal in err so that a private
// error mapper relays it instead of its own generic text.
func asPrimaryRequired(err error) (*api.Error, bool) {
	var refusal *api.Error
	if errors.As(err, &refusal) && refusal.Body.Key == api.KeyPrimaryRequired {
		return refusal, true
	}
	return nil, false
}

// primaryRequiredAuthError carries the refusal through the authentication
// envelope without Retry-After: waiting does not make a follower the primary.
func primaryRequiredAuthError(e *api.Error) *auth.Error {
	return &auth.Error{Status: e.Status, Key: e.Body.Key, Message: e.Body.Message, Hint: e.Body.Hint, NoRetryAfter: true}
}

// followerLocalCommand admits an action on this follower's own node-local
// state. A follower has no writer transaction to reauthorize in, so the raw
// credential is verified against the request's original leased view, exactly
// as a forward's early admission does, and that lease must still be the
// current publication when run starts. Nothing is stored and nothing is sent
// to the primary. run must not block.
func (s *servingRuntime) followerLocalCommand(ctx context.Context, attrs rbac.Attributes, run func()) error {
	if s == nil || ctx == nil || run == nil {
		return serving.ErrComponents
	}
	view, err := servinghttp.Component[*servingauth.View](ctx, "auth")
	if err != nil {
		return err
	}
	proof, err := servinghttp.Credential(ctx)
	if err != nil {
		return err
	}
	lease, err := servinghttp.CloneLease(ctx)
	if err != nil {
		return err
	}
	defer lease.Release()
	_, _, lifetime, err := view.AuthorizeOriginalTransition(ctx, proof, attrs, s.opts.Now)
	if err != nil {
		return err
	}
	if err := s.publisher.CheckReadLease(ctx, lease); err != nil {
		return err
	}
	if err := lifetime.Check(ctx); err != nil {
		return err
	}
	run()
	return nil
}

// followerLocalError maps the admission errors of followerLocalCommand. A
// superseded lease is a changed source, not a missing writer.
func followerLocalError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, servingauth.ErrCredentials), errors.Is(err, servingauth.ErrDenied):
		return currentCommandAPIError(err)
	default:
		return &api.Error{Status: http.StatusServiceUnavailable, Body: api.ErrorBody{Key: api.KeyUnavailable,
			Message: "this follower's serving data changed or the request ended before the command ran; it was not run"}}
	}
}
