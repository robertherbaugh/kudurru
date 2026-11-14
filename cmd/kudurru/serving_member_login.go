package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/loginlimit"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const memberLocalLogin = "auth.login"

// memberLogin is the version-1 payload of one forwarded local login. The
// password travels only over member mTLS and is never persisted: the dedup
// record keeps a fingerprint whose input includes a random nonce that is not
// stored anywhere, so it cannot serve as an offline password oracle. The
// follower keeps the session's CSRF secret; only its hash is forwarded.
type memberLogin struct {
	Username  string `json:"username"`
	Password  string `json:"password"`
	TOTPCode  string `json:"totpCode,omitempty"`
	ClientKey string `json:"clientKey"`
	SessionID string `json:"sessionID"`
	CSRFHash  string `json:"csrfHash"`
	Nonce     string `json:"nonce"`
}

// memberLoginResult is the primary's retained login result. It holds no secret.
type memberLoginResult struct {
	Committed bool   `json:"committed,omitempty"`
	SessionID string `json:"sessionID,omitempty"`
	// Refusal fields: a credential/factor refusal or lockout whose password
	// counters committed on the primary.
	Status       int    `json:"status,omitempty"`
	Key          string `json:"key,omitempty"`
	Message      string `json:"message,omitempty"`
	RetryAfterMS int64  `json:"retryAfterMS,omitempty"`
	Credentials  bool   `json:"credentials,omitempty"`
}

// forwardLogin handles a local password login on an enrolled follower. The
// primary checks the shared counters, password, factor and policy and enrolls
// the session in one command; this node then waits for its own pull to install
// that session before sealing the cookie. handled=false means run locally.
func (h *servingHTTP) forwardLogin(w http.ResponseWriter, r *http.Request, req api.LoginRequest) (bool, error) {
	token, err := servinghttp.SourceToken(r.Context())
	if err != nil || token.Authority().Mode != replication.Follower {
		return false, nil
	}
	s := h.opts.Runtime
	if s.memberForward == nil || s.member == nil || s.opts.Config.Cluster.Role == "static" {
		return true, &auth.Error{Status: http.StatusServiceUnavailable, Key: "unavailable", Message: "login on this follower requires its enrolled primary"}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	request, err := s.memberForward.Capture(ctx, token)
	if err != nil {
		return true, &auth.Error{Status: http.StatusServiceUnavailable, Key: "unavailable", Message: "the follower's source changed; retry the login"}
	}
	enrollment, prepared := security.NewForwardedEnrollment()
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return true, err
	}
	username := users.NormalizeUsername(req.Username)
	// #nosec G117 -- the password travels once over member mTLS and is never persisted
	payload, err := json.Marshal(memberLogin{Username: username, Password: req.Password, TOTPCode: req.TOTPCode, ClientKey: h.clientKey(r),
		SessionID: prepared.ID, CSRFHash: prepared.CSRFHash, Nonce: base64.RawURLEncoding.EncodeToString(nonce[:])})
	if err != nil {
		return true, err
	}
	issued := s.opts.Now().UTC()
	deadline, _ := ctx.Deadline()
	command := clusterreplica.Command{Format: clusterreplica.CommandFormat, Version: 1, ID: uuid.NewString(), ExpectedTerm: request.OriginalTerm(),
		Hops: 1, IssuedAt: issued, NotAfter: deadline, Operation: memberLocalLogin,
		Subject: clusterreplica.SubjectRef{Kind: auth.KindUser, ID: "local:" + username, Provider: "local", CredentialKind: "session", CredentialID: prepared.ID, IssuedAt: issued},
		Payload: payload}
	var result memberLoginResult
	sent, err := s.memberForward.Forward(ctx, request, command, func(_ context.Context, out clusterreplica.Outcome) error {
		d := json.NewDecoder(bytes.NewReader(out.Body))
		d.DisallowUnknownFields()
		if out.Status != http.StatusOK || d.Decode(&result) != nil {
			return clusterreplica.ErrProtocol
		}
		if result.Committed != (result.Status == 0) || result.Committed && result.SessionID != prepared.ID {
			return clusterreplica.ErrProtocol
		}
		return nil
	})
	if err != nil {
		if sent || errors.Is(err, clusterreplica.ErrOutcomeUnknown) {
			return true, &auth.Error{Status: http.StatusServiceUnavailable, Key: "completion_unknown", Message: "the login's outcome is unknown; log in again"}
		}
		return true, &auth.Error{Status: http.StatusServiceUnavailable, Key: "unavailable", Message: "the enrolled primary is unavailable; the login was not attempted"}
	}
	if !result.Committed {
		if result.Credentials {
			h.loginResult(r, username, servingauth.ErrCredentials)
		}
		return true, &auth.Error{Status: result.Status, Key: result.Key, Message: result.Message, RetryAfter: time.Duration(result.RetryAfterMS) * time.Millisecond}
	}
	// The primary committed the session; serve it once this node installs it.
	// The scheduled worker would install it too; an explicit pull shortens the
	// wait without replaying anything.
	_ = s.member.Pull(ctx)
	h.loginResult(r, username, nil)
	return true, h.sealEnrollmentWithin(w, r, enrollment, time.Until(deadline))
}

// rootMemberLoginDomain runs one forwarded local login on the primary with the
// same shared admission as its own login route.
type rootMemberLoginDomain struct {
	runtime *servingRuntime
	deps    servingauth.Dependencies
	in      memberLogin
}

func (d *rootMemberLoginDomain) bind(deps servingauth.Dependencies) { d.deps = deps }

// A login has no prior credential: its result is emitted only under the
// command and source scope, and contains no secret.
func (d *rootMemberLoginDomain) output(scope context.Context, write func(context.Context) error) error {
	return write(scope)
}

func (d *rootMemberLoginDomain) Authorize(ctx context.Context, tx clusterreplica.CommandTx, command clusterreplica.Command) (context.Context, error) {
	concrete, ok := tx.State().(*sqlite.CommandTx)
	if !ok || concrete == nil || command.Version != 1 || command.Operation != memberLocalLogin || len(command.Preconditions) != 0 || command.Credential != nil {
		return nil, clusterreplica.ErrProtocol
	}
	decoder := json.NewDecoder(bytes.NewReader(command.Payload))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&d.in) != nil || d.in.Username == "" || d.in.Username != users.NormalizeUsername(d.in.Username) || len(d.in.Username) > 253 ||
		d.in.Password == "" || len(d.in.Password) > users.MaxPasswordLen || len(d.in.ClientKey) > 256 || len(d.in.Nonce) < 43 {
		return nil, clusterreplica.ErrProtocol
	}
	claim := command.Subject
	if claim.Kind != auth.KindUser || claim.ID != "local:"+d.in.Username || claim.Provider != "local" || claim.CredentialKind != "session" || claim.CredentialID != d.in.SessionID {
		return nil, clusterreplica.ErrProtocol
	}
	return sqlite.WithActor(ctx, "User:"+d.in.Username), nil
}

func (d *rootMemberLoginDomain) Execute(ctx context.Context, tx clusterreplica.CommandTx, _ clusterreplica.Command) (clusterreplica.Outcome, error) {
	concrete, ok := tx.State().(*sqlite.CommandTx)
	if !ok || d.in.Username == "" {
		return clusterreplica.Outcome{}, clusterreplica.ErrForbidden
	}
	observed := d.runtime.opts.Now()
	proof := servingauth.Credential{Kind: servingauth.Password, Username: d.in.Username, Value: d.in.Password, TOTPCode: d.in.TOTPCode}
	id, refused, err := security.IssueForwardedLocalWithAdmission(ctx, concrete, d.deps, proof, d.in.ClientKey,
		security.ForwardedSession{ID: d.in.SessionID, CSRFHash: d.in.CSRFHash}, d.runtime.opts.Now)
	if err != nil {
		return clusterreplica.Outcome{}, err
	}
	var result memberLoginResult
	if refused != nil {
		// Only the password counters committed; the refusal is retained.
		var locked *loginlimit.Locked
		var typed *auth.Error
		switch {
		case errors.As(refused, &locked):
			result = memberLoginResult{Status: http.StatusTooManyRequests, Key: "rate_limited", Message: "too many failed attempts; try again later", RetryAfterMS: locked.RetryAfter.Milliseconds()}
		case errors.As(refused, &typed):
			result = memberLoginResult{Status: typed.Status, Key: typed.Key, Message: typed.Message, RetryAfterMS: typed.RetryAfter.Milliseconds(), Credentials: errors.Is(refused, servingauth.ErrCredentials)}
		default:
			result = memberLoginResult{Status: http.StatusUnauthorized, Key: api.KeyUnauthenticated, Message: "invalid username, password or factor", Credentials: errors.Is(refused, servingauth.ErrCredentials)}
		}
	} else {
		var current internalv1.LocalUser
		if err := concrete.Get(ctx, store.Key{Kind: "LocalUser", Name: d.in.Username}, &current); err != nil {
			return clusterreplica.Outcome{}, err
		}
		at := metav1.NewTime(observed)
		current.Status.LastLoginAt = &at
		if err := concrete.UpdateStatus(ctx, &current); err != nil {
			return clusterreplica.Outcome{}, err
		}
		if err := sessionClockCheck(ctx, concrete, id, observed, d.runtime.opts.Now); err != nil {
			return clusterreplica.Outcome{}, err
		}
		result = memberLoginResult{Committed: true, SessionID: id}
	}
	body, err := json.Marshal(result)
	return clusterreplica.Outcome{Status: http.StatusOK, Body: body}, err
}
