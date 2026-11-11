package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

const memberLogout = "auth.logout"

// memberLogoutPayload is the version-2 payload of one forwarded sign-out or
// own-session revocation. Session is empty for a sign-out: the primary then
// revokes the session that the verified cookie itself names, never a claimed
// one. For DELETE /auth/sessions/{id} it is the path's session, which the
// primary revokes only when it belongs to the verified caller's account.
type memberLogoutPayload struct {
	Session string `json:"session,omitempty"`
}

// memberLogoutResult is the primary's retained result. It holds no secret.
type memberLogoutResult struct {
	// Revoked is false for a sign-out with a credential that is no session
	// (an API token or a provider JWT), which revokes nothing, as on the
	// primary's own route.
	Revoked bool `json:"revoked"`
}

// forwardSignOut ends a session on an enrolled follower: the follower verifies
// the credential against its leased view, captures its source inside the
// route's 30-second bound and sends one auth.logout command; the primary
// verifies the raw credential again and revokes in one transaction. Nothing is
// retried. A sign-out (session == "") always clears this browser's cookies,
// also when the answer says that the session was not revoked, so that the
// browser never keeps a credential that the user asked to drop. handled=false
// means this node runs its own command.
func (h *servingHTTP) forwardSignOut(w http.ResponseWriter, r *http.Request, session string) (bool, error) {
	token, forward, err := memberForwardSource(r.Context())
	if !forward {
		return false, nil
	}
	signOut := session == ""
	clear := func(err error) error {
		if signOut {
			if clearErr := h.clearLogoutCookies(w, r); clearErr != nil {
				return clearErr
			}
		}
		return err
	}
	if err != nil {
		return true, clear(memberLogoutNotSent(err, signOut))
	}
	s := h.opts.Runtime
	if s.memberForward == nil || s.opts.Config.Cluster.Role == "static" {
		return true, clear(memberLogoutUnavailable(signOut, "sessions on this follower are ended by its enrolled primary"))
	}
	ctx := r.Context()
	if len(session) > 256 {
		return true, servingHTTPMissing()
	}
	_, principal, err := h.readPrincipal(ctx)
	if err != nil {
		return true, err
	}
	raw, err := servinghttp.Credential(ctx)
	if err != nil {
		return true, err
	}
	request, err := s.memberForward.Capture(ctx, token)
	if err != nil {
		return true, clear(memberLogoutNotSent(memberCaptureError(ctx, err), signOut))
	}
	proof, err := clusterreplica.NewCredentialProof(raw)
	if err != nil {
		return true, err
	}
	reference, err := proof.Reference()
	if err != nil {
		return true, err
	}
	payload, err := json.Marshal(memberLogoutPayload{Session: session})
	if err != nil {
		return true, err
	}
	kind := map[servingauth.CredentialKind]string{servingauth.APIToken: "api-token", servingauth.Cookie: "session", servingauth.OIDCJWT: "oidc"}[raw.Kind]
	deadline, _ := ctx.Deadline()
	command := clusterreplica.Command{Format: clusterreplica.CommandFormat, Version: 2, ID: uuid.NewString(),
		ExpectedTerm: request.OriginalTerm(), Hops: 1, IssuedAt: s.opts.Now().UTC(), NotAfter: deadline, Operation: memberLogout,
		Subject:    clusterreplica.SubjectRef{Kind: principal.Kind, ID: principal.ID, Provider: principal.Provider, CredentialKind: kind, CredentialID: reference, IssuedAt: principal.CredentialIssuedAt},
		Credential: proof, Payload: payload}
	var refusal *api.Error
	sent, err := s.memberForward.Forward(ctx, request, command, func(_ context.Context, out clusterreplica.Outcome) error {
		if out.Status == http.StatusOK {
			var result memberLogoutResult
			d := json.NewDecoder(bytes.NewReader(out.Body))
			d.DisallowUnknownFields()
			if d.Decode(&result) != nil || d.More() {
				return clusterreplica.ErrProtocol
			}
			return nil
		}
		var relayed api.Error
		if out.Status < 400 || out.Status >= 500 || json.Unmarshal(out.Body, &relayed) != nil || relayed.Body.Key == "" {
			return clusterreplica.ErrProtocol
		}
		relayed.Status = out.Status
		refusal = &relayed
		return nil
	})
	if err != nil {
		return true, clear(memberLogoutError(err, sent, signOut))
	}
	if refusal != nil {
		// A credential that the primary no longer accepts is unusable once
		// this node installs the primary's state; a sign-out still drops it.
		if refusal.Status == http.StatusUnauthorized {
			return true, clear(refusal)
		}
		return true, refusal
	}
	// The primary committed the revocation. This node refuses the session
	// after it installs that state; an explicit pull shortens the wait and
	// replays nothing. Its failure leaves the scheduled pull to do it.
	if s.member != nil {
		_ = s.member.Pull(ctx)
	}
	return true, clear(nil)
}

func memberLogoutUnavailable(signOut bool, message string) error {
	if signOut {
		message += "; the session was not revoked and stays valid until it expires; this browser's session cookie was cleared"
	} else {
		message += "; the session was not revoked"
	}
	return &api.Error{Status: http.StatusServiceUnavailable, Body: api.ErrorBody{Key: api.KeyUnavailable, Message: message,
		Hint: "revoke the session under Sessions once the primary is reachable, or on the primary"}}
}

// memberLogoutNotSent answers a session command that was never sent, with the
// cause that the follower named.
func memberLogoutNotSent(err error, signOut bool) error {
	message := "this follower could not send the request to its primary"
	var e *api.Error
	if errors.As(err, &e) && e.Body.Message != "" {
		message = e.Body.Message
	}
	return memberLogoutUnavailable(signOut, message)
}

// memberLogoutError keeps clean primary refusals distinct from an unsent
// command and from an unknown outcome. Nothing is retried.
func memberLogoutError(err error, sent, signOut bool) error {
	switch {
	case errors.Is(err, clusterreplica.ErrForbidden), errors.Is(err, servingauth.ErrDenied):
		return &api.Error{Status: http.StatusForbidden, Body: api.ErrorBody{Key: api.KeyForbidden, Message: "the primary refused this credential; the session was not revoked"}}
	case errors.Is(err, clusterreplica.ErrUnauthorized):
		return &api.Error{Status: http.StatusUnauthorized, Body: api.ErrorBody{Key: api.KeyUnauthenticated, Message: "the primary did not accept this credential"}}
	case !sent && !errors.Is(err, clusterreplica.ErrOutcomeUnknown):
		return memberLogoutUnavailable(signOut, "the enrolled primary is unavailable")
	default:
		message := "the outcome of revoking the session is unknown; it may stay valid until it expires; do not replay this request"
		if signOut {
			message = "the sign-out's outcome is unknown; the session may stay valid until it expires; this browser's session cookie was cleared; do not replay this request"
		}
		return &api.Error{Status: http.StatusServiceUnavailable, Body: api.ErrorBody{Key: "completion_unknown", Message: message,
			Hint: "sign in and review the session list; revoke the session there if it is still listed"}}
	}
}

// rootMemberLogoutDomain revokes one session on the primary for a follower's
// sign-out or session revocation, with the bodies of the primary's own routes.
type rootMemberLogoutDomain struct {
	runtime *servingRuntime
	deps    servingauth.Dependencies
	in      memberLogoutPayload
	// principal and session come from verifying the raw credential in this
	// transaction; session is "" for a credential that is no cookie.
	principal auth.Principal
	session   string
	// refused is set when this transaction does not accept the credential:
	// Execute retains a 401 instead of a revocation.
	refused bool
}

func (d *rootMemberLogoutDomain) bind(deps servingauth.Dependencies) { d.deps = deps }

// A sign-out revokes the credential that it was sent with, so its result is
// emitted only under the command and source scope. It contains no secret.
func (d *rootMemberLogoutDomain) output(scope context.Context, write func(context.Context) error) error {
	return write(scope)
}

func (d *rootMemberLogoutDomain) Authorize(ctx context.Context, tx clusterreplica.CommandTx, command clusterreplica.Command) (context.Context, error) {
	concrete, ok := tx.State().(*sqlite.CommandTx)
	if !ok || concrete == nil || command.Version != 2 || command.Operation != memberLogout || len(command.Preconditions) != 0 {
		return nil, clusterreplica.ErrProtocol
	}
	decoder := json.NewDecoder(bytes.NewReader(command.Payload))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&d.in) != nil || len(d.in.Session) > 256 {
		return nil, clusterreplica.ErrProtocol
	}
	raw, err := command.Credential.Raw()
	if err != nil {
		return nil, err
	}
	reference, err := command.Credential.Reference()
	if err != nil {
		return nil, err
	}
	principal, session, err := servingauth.AuthenticateCurrentSessionWithClock(ctx, concrete, d.deps, raw, d.runtime.opts.Now)
	if errors.Is(err, servingauth.ErrCredentials) {
		// The follower's view accepted a credential that the primary no
		// longer does (revoked or expired since). That is a clean refusal,
		// retained as such. A retained outcome of the same command is never
		// handed to a credential that no longer verifies.
		if _, found, dedupErr := tx.GetDedup(ctx, command.ID); dedupErr != nil || found {
			return nil, errors.Join(clusterreplica.ErrUnauthorized, dedupErr)
		}
		d.refused = true
		return sqlite.WithActor(ctx, "anonymous"), nil
	}
	if err != nil {
		return nil, err
	}
	claim := command.Subject
	if principal.Kind != claim.Kind || principal.ID != claim.ID || principal.Provider != claim.Provider || reference != claim.CredentialID || !principal.CredentialIssuedAt.Equal(claim.IssuedAt) {
		return nil, clusterreplica.ErrForbidden
	}
	d.principal, d.session = principal, session
	actor := audit.Actor{Kind: string(principal.Kind), ID: principal.ID, Name: principal.Name, Groups: principal.Groups, Auth: principal.AuthMethod}
	ctx = auth.WithPrincipal(ctx, principal)
	ctx = sqlite.WithActor(ctx, actor.String())
	method := http.MethodPost
	if d.in.Session != "" {
		method = http.MethodDelete
	}
	return audit.WithRequest(ctx, actor, audit.Request{ID: command.ID, Method: method, Path: clusterreplica.PathWrite}, func() string { return "authenticated" }), nil
}

func (d *rootMemberLogoutDomain) Execute(ctx context.Context, tx clusterreplica.CommandTx, _ clusterreplica.Command) (clusterreplica.Outcome, error) {
	concrete, ok := tx.State().(*sqlite.CommandTx)
	if !ok {
		return clusterreplica.Outcome{}, clusterreplica.ErrForbidden
	}
	if d.refused {
		return retainedRefusal(&api.Error{Status: http.StatusUnauthorized, Body: api.ErrorBody{Key: api.KeyUnauthenticated,
			Message: "the primary no longer accepts this credential; it is not valid on any node once each has installed the primary's state"}})
	}
	if d.principal.ID == "" {
		return clusterreplica.Outcome{}, clusterreplica.ErrForbidden
	}
	target := d.in.Session
	if target == "" {
		// POST /auth/logout: only a cookie names a session to revoke.
		if d.session == "" {
			body, err := json.Marshal(memberLogoutResult{})
			return clusterreplica.Outcome{Status: http.StatusOK, Body: body}, err
		}
		target = d.session
	} else {
		// DELETE /auth/sessions/{id}: the owner check of the primary's route.
		inventory, err := servingHTTPSecurity(ctx, concrete)
		if err != nil {
			return clusterreplica.Outcome{}, err
		}
		found := false
		for _, s := range inventory.Sessions {
			found = found || s.ID == target && servingHTTPSessionOwner(s.Account, d.principal)
		}
		for _, s := range inventory.ExternalSessions {
			found = found || s.ID == target && servingHTTPSessionOwner(s.Account, d.principal)
		}
		if !found {
			return retainedRefusal(&api.Error{Status: http.StatusNotFound, Body: api.ErrorBody{Key: api.KeyNotFound, Message: "resource not found"}})
		}
	}
	state, err := security.Load(ctx, concrete)
	if err != nil {
		return clusterreplica.Outcome{}, err
	}
	if err := state.Revoke(target); err != nil {
		return clusterreplica.Outcome{}, err
	}
	if err := state.Commit(ctx); err != nil {
		return clusterreplica.Outcome{}, err
	}
	body, err := json.Marshal(memberLogoutResult{Revoked: true})
	return clusterreplica.Outcome{Status: http.StatusOK, Body: body}, err
}
