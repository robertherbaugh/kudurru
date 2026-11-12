package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/loginlimit"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// The self-service commands that an enrolled follower forwards (D368): the
// caller's own password change and the creation and revocation of personal
// API tokens.
const (
	memberPassword = "auth.password"
	memberTokens   = "auth.tokens"
	// memberSelfAnswerWait bounds how long a follower waits, after a
	// committed command, for its own view to serve the result before it
	// answers anyway.
	memberSelfAnswerWait = 5 * time.Second
)

// memberPasswordChange is the version-2 payload of one forwarded password
// change. The follower checked the new password's rules and hashed it; only
// the hash travels. The current password and factor code travel once over
// member mTLS for the primary's shared counters and are never persisted: the
// dedup fingerprint includes a random nonce that is stored nowhere, as for a
// forwarded login (D363). A cookie caller's new session is prepared by the
// follower, which keeps its CSRF secret; a token caller sends no session.
type memberPasswordChange struct {
	CurrentPassword string `json:"currentPassword"`
	TOTPCode        string `json:"totpCode,omitempty"`
	PasswordHash    string `json:"passwordHash"`
	RevokeTokens    bool   `json:"revokeTokens,omitempty"`
	ClientKey       string `json:"clientKey"`
	SessionID       string `json:"sessionID,omitempty"`
	CSRFHash        string `json:"csrfHash,omitempty"`
	Nonce           string `json:"nonce"`
}

// memberSelfResult is the primary's retained result of a forwarded password
// change. It holds no secret. A refusal carries the public answer of the
// primary's own route and, for a failed password or factor, Credentials, so
// that the follower charges its own local limiter as the route would.
type memberSelfResult struct {
	Committed    bool   `json:"committed,omitempty"`
	SessionID    string `json:"sessionID,omitempty"`
	Status       int    `json:"status,omitempty"`
	Key          string `json:"key,omitempty"`
	Message      string `json:"message,omitempty"`
	Hint         string `json:"hint,omitempty"`
	RetryAfterMS int64  `json:"retryAfterMS,omitempty"`
	Credentials  bool   `json:"credentials,omitempty"`
}

// memberTokenCommand is the version-2 payload of one forwarded personal token
// creation or revocation. Scope is the own-token scope that the follower
// admitted; the primary authorizes it again. A creation carries the route's
// request body and the follower-generated token ID and secret hash: the
// secret never leaves the follower, because a retained outcome may not be
// sensitive (plainCodec).
type memberTokenCommand struct {
	Action     string                  `json:"action"`
	Scope      rbac.Scope              `json:"scope"`
	ID         string                  `json:"id"`
	Request    *api.CreateTokenRequest `json:"request,omitempty"`
	SecretHash string                  `json:"secretHash,omitempty"`
}

const (
	memberTokenCreate = "create"
	memberTokenDelete = "delete"
)

// memberTokenResult is the primary's retained result of a token command.
type memberTokenResult struct {
	Token   *api.TokenInfo `json:"token,omitempty"`
	Deleted bool           `json:"deleted,omitempty"`
}

// sendMemberSelf captures the request's follower source once and sends one
// version-2 self-service command with the raw credential proof. accept sees the
// primary's outcome; nothing is retried. sent reports whether the primary may
// have received the command.
func (h *servingHTTP) sendMemberSelf(ctx context.Context, token sqlite.GenerationToken, principal auth.Principal, raw servingauth.Credential, operation string, payload any, accept func(clusterreplica.Outcome) error) (bool, error) {
	s := h.opts.Runtime
	request, err := s.memberForward.Capture(ctx, token)
	if err != nil {
		return false, memberCaptureError(ctx, err)
	}
	proof, err := clusterreplica.NewCredentialProof(raw)
	if err != nil {
		return false, err
	}
	reference, err := proof.Reference()
	if err != nil {
		return false, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return false, err
	}
	kind := map[servingauth.CredentialKind]string{servingauth.APIToken: "api-token", servingauth.Cookie: "session", servingauth.OIDCJWT: "oidc"}[raw.Kind]
	deadline, _ := ctx.Deadline()
	command := clusterreplica.Command{Format: clusterreplica.CommandFormat, Version: 2, ID: uuid.NewString(),
		ExpectedTerm: request.OriginalTerm(), Hops: 1, IssuedAt: s.opts.Now().UTC(), NotAfter: deadline, Operation: operation,
		Subject:    clusterreplica.SubjectRef{Kind: principal.Kind, ID: principal.ID, Provider: principal.Provider, CredentialKind: kind, CredentialID: reference, IssuedAt: principal.CredentialIssuedAt},
		Credential: proof, Payload: body}
	return s.memberForward.Forward(ctx, request, command, func(_ context.Context, out clusterreplica.Outcome) error { return accept(out) })
}

// memberSelfUnavailable answers a self-service command that was not sent.
func memberSelfUnavailable(err error, what string) error {
	message := "this follower could not send the request to its primary"
	var e *api.Error
	if errors.As(err, &e) && e.Body.Message != "" {
		message = e.Body.Message
	}
	return &auth.Error{Status: http.StatusServiceUnavailable, Key: api.KeyUnavailable, Message: message + "; " + what}
}

// memberSelfError keeps an unsent command apart from an unknown outcome.
// Clean primary refusals are retained results and never reach it.
func memberSelfError(err error, sent bool, notDone, unknown string) error {
	switch {
	case errors.Is(err, clusterreplica.ErrForbidden), errors.Is(err, servingauth.ErrDenied):
		return &auth.Error{Status: http.StatusForbidden, Key: api.KeyForbidden, Message: "the primary refused this credential; " + notDone}
	case errors.Is(err, clusterreplica.ErrUnauthorized):
		return &auth.Error{Status: http.StatusUnauthorized, Key: api.KeyUnauthenticated, Message: "the primary did not accept this credential; " + notDone}
	case !sent && !errors.Is(err, clusterreplica.ErrOutcomeUnknown):
		return &auth.Error{Status: http.StatusServiceUnavailable, Key: api.KeyUnavailable, Message: "the enrolled primary is unavailable; " + notDone}
	default:
		return &auth.Error{Status: http.StatusServiceUnavailable, Key: "completion_unknown", Message: unknown + "; do not replay this request"}
	}
}

// memberSelfForward reports whether this request is a follower's and whether
// the follower can forward it. err is the answer when it cannot.
func (h *servingHTTP) memberSelfForward(ctx context.Context, what string) (sqlite.GenerationToken, bool, error) {
	token, forward, err := memberForwardSource(ctx)
	if !forward {
		return sqlite.GenerationToken{}, false, nil
	}
	if err != nil {
		return token, true, memberSelfUnavailable(err, what)
	}
	s := h.opts.Runtime
	if s.memberForward == nil || s.member == nil || s.opts.Config.Cluster.Role == "static" {
		return token, true, &auth.Error{Status: http.StatusServiceUnavailable, Key: api.KeyUnavailable, Message: "this follower has no enrolled primary to send the request to; " + what}
	}
	return token, true, nil
}

// waitMemberView pulls once and then waits, within memberSelfAnswerWait and
// the request's own deadline, until ready accepts this node's published auth
// view. A failed pull or a late publication only ends the wait: the command
// has committed on the primary, and the scheduled pull installs it.
func (h *servingHTTP) waitMemberView(ctx context.Context, ready func(*servingauth.View) bool) {
	s := h.opts.Runtime
	_ = s.member.Pull(ctx)
	ctx, cancel := context.WithTimeout(ctx, memberSelfAnswerWait)
	defer cancel()
	for {
		changed := s.publisher.Subscribe()
		if lease, err := s.publisher.CaptureHTTP(ctx); err == nil {
			value, ok := lease.Bundle().Component("auth")
			view, typed := value.(*servingauth.View)
			done := ok && typed && ready(view)
			lease.Release()
			if done {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		}
	}
}

// forwardPassword changes the caller's own local password on an enrolled
// follower. The route has already checked the request body and this node's
// own login and factor limits. The follower hashes the new password, captures
// its source and sends one auth.password command; the primary verifies the
// credential, the current password and the factor against its shared counters
// and commits the change, and for a cookie caller a new session that the
// follower prepared. The follower answers once its own view serves the result.
// handled=false means this node runs its own command.
func (h *servingHTTP) forwardPassword(w http.ResponseWriter, r *http.Request, original auth.Principal, req api.PasswordChangeRequest) (bool, error) {
	const notDone = "the password was not changed"
	ctx := r.Context()
	token, forward, err := h.memberSelfForward(ctx, notDone)
	if !forward || err != nil {
		return forward, err
	}
	raw, err := servinghttp.Credential(ctx)
	if err != nil {
		return true, err
	}
	if raw.Kind != servingauth.Cookie && raw.Kind != servingauth.APIToken {
		return true, servingauth.ErrInvalid
	}
	// The route's bound (memberOwnForwardRoute) is at most 30 seconds, the
	// hash's own limit. Hash outside every lock.
	hash, err := h.opts.Hasher.Hash(ctx, req.NewPassword)
	if err != nil {
		return true, memberSelfUnavailable(err, notDone)
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return true, err
	}
	payload := memberPasswordChange{CurrentPassword: req.CurrentPassword, TOTPCode: req.TOTPCode, PasswordHash: hash, RevokeTokens: req.RevokeTokens,
		ClientKey: h.clientKey(r), Nonce: base64.RawURLEncoding.EncodeToString(nonce[:])}
	var enrollment security.Enrollment
	if raw.Kind == servingauth.Cookie {
		var prepared security.ForwardedSession
		enrollment, prepared = security.NewForwardedEnrollment()
		payload.SessionID, payload.CSRFHash = prepared.ID, prepared.CSRFHash
	}
	var result memberSelfResult
	sent, err := h.sendMemberSelf(ctx, token, original, raw, memberPassword, payload, func(out clusterreplica.Outcome) error {
		d := json.NewDecoder(bytes.NewReader(out.Body))
		d.DisallowUnknownFields()
		if out.Status != http.StatusOK || d.Decode(&result) != nil || d.More() {
			return clusterreplica.ErrProtocol
		}
		if result.Committed != (result.Status == 0) || result.Committed && result.SessionID != payload.SessionID || !result.Committed && (result.Status < 400 || result.Status >= 500) {
			return clusterreplica.ErrProtocol
		}
		return nil
	})
	if err != nil {
		return true, memberSelfError(err, sent, notDone, "the password change's outcome is unknown; sign in with the new password, or with the current one if it was not changed")
	}
	if !result.Committed {
		if result.Credentials {
			h.loginResult(r, original.Name, servingauth.ErrCredentials)
		}
		return true, &auth.Error{Status: result.Status, Key: result.Key, Message: result.Message, Hint: result.Hint, RetryAfter: time.Duration(result.RetryAfterMS) * time.Millisecond}
	}
	h.loginResult(r, original.Name, nil)
	deadline, _ := ctx.Deadline()
	if raw.Kind == servingauth.Cookie {
		// The scheduled worker would install the session too; the explicit
		// pull shortens the wait and replays nothing.
		_ = h.opts.Runtime.member.Pull(ctx)
		return true, h.sealEnrollmentWithin(w, r, enrollment, time.Until(deadline))
	}
	// A token caller keeps its token (unless it revoked them all): answer
	// from this node's own view once it carries the new password.
	return true, h.answerChangedPassword(w, r, original.AccountUID, func(stored string) bool { return stored == hash }, func(v *servingauth.View) (auth.Principal, error) {
		p, err := v.Authenticate(r.Context(), raw, h.opts.Runtime.opts.Now())
		if err != nil && req.RevokeTokens {
			// The change revoked the calling token too. Answer for the account
			// as the primary's own route does.
			p, err = original, nil
			p.MustChangePassword = false
		}
		return p, err
	})
}

// answerChangedPassword answers a token caller's committed password change
// from the first published view whose account carries the new password hash.
func (h *servingHTTP) answerChangedPassword(w http.ResponseWriter, r *http.Request, accountUID string, matches func(string) bool, principal func(*servingauth.View) (auth.Principal, error)) error {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	for {
		changed := h.opts.Runtime.publisher.Subscribe()
		lease, err := h.opts.Runtime.publisher.CaptureHTTP(ctx)
		if err == nil {
			value, ok := lease.Bundle().Component("auth")
			v, typed := value.(*servingauth.View)
			if ok && typed {
				objects, e := v.Objects("LocalUser")
				if e == nil {
					for _, o := range objects {
						u := o.(*internalv1.LocalUser)
						if string(u.UID) == accountUID && matches(u.Spec.PasswordHash) {
							p, e := principal(v)
							var out api.SessionInfo
							if e == nil {
								out, e = h.sessionInfo(v, p, nil)
							}
							lease.Release()
							if e != nil {
								return e
							}
							return servingHTTPJSON(w, 200, out)
						}
					}
				}
			}
			lease.Release()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

// forwardTokenCreate creates a personal API token on an enrolled follower.
// The follower generates the secret and keeps it; the primary runs the route's
// own body with the token ID and the secret's hash. The follower answers with
// the secret once its own view authenticates the token, so that the caller can
// use it here at once. handled=false means this node runs its own command.
func (h *servingHTTP) forwardTokenCreate(w http.ResponseWriter, r *http.Request, attrs rbac.Attributes, req api.CreateTokenRequest) (bool, error) {
	const notDone = "no token was created"
	ctx := r.Context()
	token, forward, err := h.memberSelfForward(ctx, notDone)
	if !forward || err != nil {
		return forward, err
	}
	_, principal, err := h.readPrincipal(ctx)
	if err != nil {
		return true, err
	}
	raw, err := servinghttp.Credential(ctx)
	if err != nil {
		return true, err
	}
	wire, id := tokens.Generate()
	var result memberTokenResult
	var refusal *auth.Error
	sent, err := h.sendMemberSelf(ctx, token, principal, raw, memberTokens, memberTokenCommand{Action: memberTokenCreate, Scope: attrs.Scope, ID: id, Request: &req, SecretHash: tokens.Hash(wire)}, func(out clusterreplica.Outcome) error {
		var ok bool
		refusal, ok = relayedAuthRefusal(out)
		if ok {
			return nil
		}
		if err := decodeMemberTokenResult(out, &result); err != nil || result.Token == nil || result.Token.ID != id || result.Deleted {
			return clusterreplica.ErrProtocol
		}
		return nil
	})
	if err != nil {
		return true, memberSelfError(err, sent, notDone, "the token creation's outcome is unknown; list your tokens and revoke one you do not hold")
	}
	if refusal != nil {
		return true, refusal
	}
	h.waitMemberView(ctx, func(v *servingauth.View) bool {
		_, err := v.Authenticate(ctx, servingauth.Credential{Kind: servingauth.APIToken, Value: wire}, h.opts.Runtime.opts.Now())
		return err == nil
	})
	return true, servingHTTPJSON(w, http.StatusCreated, api.CreatedToken{TokenInfo: *result.Token, Token: wire})
}

// forwardTokenDelete revokes one API token on an enrolled follower through the
// primary's own route body (an own token, or another one with tokens.delete;
// otherwise 404). handled=false means this node runs its own command.
func (h *servingHTTP) forwardTokenDelete(w http.ResponseWriter, r *http.Request, attrs rbac.Attributes, id string) (bool, error) {
	const notDone = "the token was not revoked"
	ctx := r.Context()
	token, forward, err := h.memberSelfForward(ctx, notDone)
	if !forward || err != nil {
		return forward, err
	}
	if id == "" || len(id) > 253 {
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
	var refusal *auth.Error
	sent, err := h.sendMemberSelf(ctx, token, principal, raw, memberTokens, memberTokenCommand{Action: memberTokenDelete, Scope: attrs.Scope, ID: id}, func(out clusterreplica.Outcome) error {
		var ok bool
		refusal, ok = relayedAuthRefusal(out)
		if ok {
			return nil
		}
		var result memberTokenResult
		if err := decodeMemberTokenResult(out, &result); err != nil || !result.Deleted || result.Token != nil {
			return clusterreplica.ErrProtocol
		}
		return nil
	})
	if err != nil {
		return true, memberSelfError(err, sent, notDone, "the token revocation's outcome is unknown; list the tokens to see whether it is still there")
	}
	if refusal != nil {
		return true, refusal
	}
	// This node refuses the token once it installs the primary's state.
	h.waitMemberView(ctx, func(v *servingauth.View) bool {
		objects, err := v.Objects("APIToken")
		if err != nil {
			return false
		}
		for _, o := range objects {
			if o.(*internalv1.APIToken).Name == id {
				return false
			}
		}
		return true
	})
	w.WriteHeader(http.StatusNoContent)
	return true, nil
}

// relayedAuthRefusal decodes a retained public refusal of the primary's route.
func relayedAuthRefusal(out clusterreplica.Outcome) (*auth.Error, bool) {
	if out.Status == http.StatusOK {
		return nil, false
	}
	var relayed api.Error
	if out.Status < 400 || out.Status >= 500 || json.Unmarshal(out.Body, &relayed) != nil || relayed.Body.Key == "" {
		return nil, false
	}
	hint, _ := relayed.Body.Hint.(string)
	return &auth.Error{Status: out.Status, Key: relayed.Body.Key, Message: relayed.Body.Message, Hint: hint}, true
}

func decodeMemberTokenResult(out clusterreplica.Outcome, result *memberTokenResult) error {
	d := json.NewDecoder(bytes.NewReader(out.Body))
	d.DisallowUnknownFields()
	if out.Status != http.StatusOK || d.Decode(result) != nil || d.More() {
		return clusterreplica.ErrProtocol
	}
	return nil
}

// memberSelfRefusal classifies err from a self-service route body that the
// primary runs for a follower. A clean client refusal (4xx), returned before
// the body staged anything, becomes the retained answer of the primary's own
// route; every other error rolls the whole command back.
func memberSelfRefusal(err error) (*auth.Error, bool) {
	if err == nil {
		return nil, false
	}
	answer := servingHTTPAuthError(err)
	if answer.Status < 400 || answer.Status >= 500 {
		return nil, false
	}
	return answer, true
}

func authRefusalOutcome(e *auth.Error) (clusterreplica.Outcome, error) {
	return retainedRefusal(&api.Error{Status: e.Status, Body: api.ErrorBody{Key: e.Key, Message: e.Message, Hint: e.Hint}})
}

// selfCredentialRefusal is the primary's answer when this transaction does not
// accept or authorize a credential that the follower's view admitted: a clean
// retained refusal, except that a command whose dedup record already exists is
// aborted, so that a credential that no longer verifies never recovers the
// retained outcome of its own earlier command (as for auth.logout).
func selfCredentialRefusal(ctx context.Context, tx clusterreplica.CommandTx, command clusterreplica.Command, err error) (*auth.Error, error) {
	answer, clean := memberSelfRefusal(err)
	if !clean || !errors.Is(err, servingauth.ErrCredentials) && !errors.Is(err, servingauth.ErrDenied) {
		return nil, err
	}
	if _, found, dedupErr := tx.GetDedup(ctx, command.ID); dedupErr != nil || found {
		return nil, errors.Join(clusterreplica.ErrUnauthorized, dedupErr)
	}
	return answer, nil
}

func memberSubjectMatches(principal auth.Principal, claim clusterreplica.SubjectRef, reference string) bool {
	return principal.Kind == claim.Kind && principal.ID == claim.ID && principal.Provider == claim.Provider && reference == claim.CredentialID && principal.CredentialIssuedAt.Equal(claim.IssuedAt)
}

func memberActorContext(ctx context.Context, principal auth.Principal, command clusterreplica.Command, method, path string) context.Context {
	actor := audit.Actor{Kind: string(principal.Kind), ID: principal.ID, Name: principal.Name, Groups: principal.Groups, Auth: principal.AuthMethod}
	ctx = auth.WithPrincipal(ctx, principal)
	ctx = sqlite.WithActor(ctx, actor.String())
	return audit.WithRequest(ctx, actor, audit.Request{ID: command.ID, Method: method, Path: clusterreplica.PathWrite}, func() string { return path })
}

// rootMemberPasswordDomain changes a local user's own password on the primary
// for a follower, with the same admission as the primary's own route.
type rootMemberPasswordDomain struct {
	runtime   *servingRuntime
	deps      servingauth.Dependencies
	in        memberPasswordChange
	raw       servingauth.Credential
	principal auth.Principal
	// refused is the retained answer when this transaction does not accept
	// the caller's credential.
	refused *auth.Error
}

func (d *rootMemberPasswordDomain) bind(deps servingauth.Dependencies) { d.deps = deps }

// A password change ends a cookie caller's credential (the account's floor
// moves), so its result is emitted only under the command and source scope. It
// contains no secret.
func (d *rootMemberPasswordDomain) output(scope context.Context, write func(context.Context) error) error {
	return write(scope)
}

func (d *rootMemberPasswordDomain) Authorize(ctx context.Context, tx clusterreplica.CommandTx, command clusterreplica.Command) (context.Context, error) {
	concrete, ok := tx.State().(*sqlite.CommandTx)
	if !ok || concrete == nil || command.Version != 2 || command.Operation != memberPassword || len(command.Preconditions) != 0 {
		return nil, clusterreplica.ErrProtocol
	}
	decoder := json.NewDecoder(bytes.NewReader(command.Payload))
	decoder.DisallowUnknownFields()
	in := &d.in
	if decoder.Decode(in) != nil || decoder.More() || in.CurrentPassword == "" || len(in.CurrentPassword) > users.MaxPasswordLen || len(in.TOTPCode) > 64 ||
		!users.ValidPasswordHash(in.PasswordHash) || len(in.ClientKey) > 256 || len(in.Nonce) < 43 || len(in.Nonce) > 256 || len(in.SessionID) > 256 || len(in.CSRFHash) > 128 {
		return nil, clusterreplica.ErrProtocol
	}
	raw, err := command.Credential.Raw()
	if err != nil {
		return nil, err
	}
	if raw.Kind != servingauth.Cookie && raw.Kind != servingauth.APIToken || (raw.Kind == servingauth.Cookie) != (in.SessionID != "") || (in.SessionID == "") != (in.CSRFHash == "") {
		return nil, clusterreplica.ErrProtocol
	}
	reference, err := command.Credential.Reference()
	if err != nil {
		return nil, err
	}
	principal, err := servingauth.AuthenticateCurrentWithClock(ctx, concrete, d.deps, raw, d.runtime.opts.Now)
	if err != nil {
		refused, abort := selfCredentialRefusal(ctx, tx, command, err)
		if abort != nil {
			return nil, abort
		}
		d.refused = refused
		return sqlite.WithActor(ctx, "anonymous"), nil
	}
	if !memberSubjectMatches(principal, command.Subject, reference) {
		return nil, clusterreplica.ErrForbidden
	}
	if principal.Kind != auth.KindUser || principal.Provider != "local" {
		return nil, clusterreplica.ErrProtocol
	}
	d.raw, d.principal = raw, principal
	return memberActorContext(ctx, principal, command, http.MethodPost, "authenticated"), nil
}

func (d *rootMemberPasswordDomain) Execute(ctx context.Context, tx clusterreplica.CommandTx, _ clusterreplica.Command) (clusterreplica.Outcome, error) {
	concrete, ok := tx.State().(*sqlite.CommandTx)
	if !ok {
		return clusterreplica.Outcome{}, clusterreplica.ErrForbidden
	}
	var result memberSelfResult
	switch {
	case d.refused != nil:
		result = memberSelfResult{Status: d.refused.Status, Key: d.refused.Key, Message: d.refused.Message, Hint: authHint(d.refused)}
	case d.principal.ID == "":
		return clusterreplica.Outcome{}, clusterreplica.ErrForbidden
	default:
		change, err := security.PrepareForwardedPasswordChange(ctx, d.in.CurrentPassword, d.in.PasswordHash, d.in.TOTPCode, d.in.RevokeTokens)
		if err != nil {
			return clusterreplica.Outcome{}, err
		}
		changed, refused, err := security.ChangeForwardedLocalPasswordWithAdmission(ctx, concrete, d.deps, d.raw, d.principal, change, d.in.ClientKey,
			security.ForwardedSession{ID: d.in.SessionID, CSRFHash: d.in.CSRFHash}, d.runtime.opts.Now)
		if err != nil {
			return clusterreplica.Outcome{}, err
		}
		if refused != nil {
			// Only the shared password counters committed; the refusal is
			// the primary route's own answer, retained.
			var locked *loginlimit.Locked
			if errors.As(refused, &locked) {
				result = memberSelfResult{Status: http.StatusTooManyRequests, Key: "rate_limited", Message: "too many failed attempts; try again later", RetryAfterMS: locked.RetryAfter.Milliseconds()}
				break
			}
			answer := servingHTTPAuthError(refused)
			if answer.Status < 400 || answer.Status >= 500 {
				return clusterreplica.Outcome{}, refused
			}
			result = memberSelfResult{Status: answer.Status, Key: answer.Key, Message: answer.Message, Hint: authHint(answer), RetryAfterMS: answer.RetryAfter.Milliseconds(), Credentials: errors.Is(refused, servingauth.ErrCredentials)}
			break
		}
		if d.raw.Kind == servingauth.Cookie {
			if changed.Enrollment.ID() != d.in.SessionID {
				return clusterreplica.Outcome{}, clusterreplica.ErrProtocol
			}
			if err := sessionClockCheck(ctx, concrete, d.in.SessionID, changed.IssuedAt, d.runtime.opts.Now); err != nil {
				return clusterreplica.Outcome{}, err
			}
		}
		result = memberSelfResult{Committed: true, SessionID: d.in.SessionID}
	}
	body, err := json.Marshal(result)
	return clusterreplica.Outcome{Status: http.StatusOK, Body: body}, err
}

// rootMemberTokenDomain creates or revokes one API token on the primary for a
// follower, with the bodies of the primary's own routes.
type rootMemberTokenDomain struct {
	runtime  *servingRuntime
	deps     servingauth.Dependencies
	in       memberTokenCommand
	attrs    rbac.Attributes
	intent   tokenIntent
	raw      servingauth.Credential
	lifetime *servingauth.CredentialLifetime
	refused  *auth.Error
}

func (d *rootMemberTokenDomain) bind(deps servingauth.Dependencies) { d.deps = deps }

// A creation is emitted under the verified credential's lifetime. A revocation
// may revoke the very token that sent it, so it is emitted under the command
// and source scope only, as a sign-out is. Neither result contains a secret.
func (d *rootMemberTokenDomain) output(scope context.Context, write func(context.Context) error) error {
	if d.in.Action == memberTokenCreate && d.refused == nil {
		return credentialOutput(scope, d.lifetime, write)
	}
	return write(scope)
}

func (d *rootMemberTokenDomain) Authorize(ctx context.Context, tx clusterreplica.CommandTx, command clusterreplica.Command) (context.Context, error) {
	concrete, ok := tx.State().(*sqlite.CommandTx)
	if !ok || concrete == nil || command.Version != 2 || command.Operation != memberTokens || len(command.Preconditions) != 0 {
		return nil, clusterreplica.ErrProtocol
	}
	decoder := json.NewDecoder(bytes.NewReader(command.Payload))
	decoder.DisallowUnknownFields()
	in := &d.in
	if decoder.Decode(in) != nil || decoder.More() || in.ID == "" || len(in.ID) > 253 || in.Scope.Kind == "" || len(in.Scope.Name) > 253 {
		return nil, clusterreplica.ErrProtocol
	}
	method := http.MethodDelete
	switch in.Action {
	case memberTokenCreate:
		if in.Request == nil || !validTokenHash(in.SecretHash) {
			return nil, clusterreplica.ErrProtocol
		}
		if _, ok := tokens.Parse(tokens.Prefix + in.ID + "_" + base64.RawURLEncoding.EncodeToString(make([]byte, 32))); !ok {
			return nil, clusterreplica.ErrProtocol
		}
		method = http.MethodPost
	case memberTokenDelete:
		if in.Request != nil || in.SecretHash != "" {
			return nil, clusterreplica.ErrProtocol
		}
	default:
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
	// The own-token permission at the scope that the follower admitted; the
	// route body below repeats it with the creation's exact checks.
	d.attrs = rbac.Attributes{Resource: "tokens", Verb: "own", Scope: in.Scope}
	principal, decision, lifetime, err := servingauth.ReauthorizeCurrentForOutput(ctx, concrete, d.deps, raw, d.attrs, d.runtime.opts.Now)
	if err != nil {
		refused, abort := selfCredentialRefusal(ctx, tx, command, err)
		if abort != nil {
			return nil, abort
		}
		d.refused = refused
		return sqlite.WithActor(ctx, "anonymous"), nil
	}
	if !memberSubjectMatches(principal, command.Subject, reference) {
		return nil, clusterreplica.ErrForbidden
	}
	d.raw, d.lifetime = raw, lifetime
	return memberActorContext(ctx, principal, command, method, decision.Path), nil
}

func (d *rootMemberTokenDomain) Execute(ctx context.Context, tx clusterreplica.CommandTx, _ clusterreplica.Command) (clusterreplica.Outcome, error) {
	concrete, ok := tx.State().(*sqlite.CommandTx)
	if !ok {
		return clusterreplica.Outcome{}, clusterreplica.ErrForbidden
	}
	if d.refused != nil {
		return authRefusalOutcome(d.refused)
	}
	if d.lifetime == nil {
		return clusterreplica.Outcome{}, clusterreplica.ErrForbidden
	}
	var result memberTokenResult
	switch d.in.Action {
	case memberTokenCreate:
		permissions, err := rbac.LoadPermissions()
		if err != nil {
			return clusterreplica.Outcome{}, err
		}
		intent, err := tokenRequestIntent(permissions, *d.in.Request)
		if err != nil {
			return refusedSelfOutcome(err)
		}
		intent.ID, intent.SecretHash = d.in.ID, d.in.SecretHash
		created, staged, err := d.runtime.stageToken(ctx, concrete, d.deps, d.raw, d.attrs, intent)
		if err != nil {
			if staged {
				return clusterreplica.Outcome{}, err
			}
			return refusedSelfOutcome(err)
		}
		info := servingHTTPTokenInfo(created)
		result.Token = &info
	case memberTokenDelete:
		staged, err := d.runtime.deleteTokenCurrent(ctx, concrete, d.deps, d.raw, d.attrs, d.in.ID)
		if err != nil {
			if staged {
				return clusterreplica.Outcome{}, err
			}
			return refusedSelfOutcome(err)
		}
		result.Deleted = true
	default:
		return clusterreplica.Outcome{}, clusterreplica.ErrProtocol
	}
	body, err := json.Marshal(result)
	return clusterreplica.Outcome{Status: http.StatusOK, Body: body}, err
}

// refusedSelfOutcome retains a clean refusal that a route body returned before
// it staged anything, and rolls back for every other error.
func refusedSelfOutcome(err error) (clusterreplica.Outcome, error) {
	if errors.Is(err, store.ErrNotFound) {
		return authRefusalOutcome(servingHTTPMissing())
	}
	answer, clean := memberSelfRefusal(err)
	if !clean {
		return clusterreplica.Outcome{}, err
	}
	return authRefusalOutcome(answer)
}

func authHint(e *auth.Error) string {
	hint, _ := e.Hint.(string)
	return hint
}

func validTokenHash(hash string) bool {
	decoded, err := hex.DecodeString(hash)
	return err == nil && len(decoded) == 32 && hex.EncodeToString(decoded) == hash
}
