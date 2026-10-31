package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/auth/totp"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"k8s.io/apimachinery/pkg/util/validation"
)

// registerTOTP adds only complete-runtime routes. Factor verification and replay
// mutation remain in security's concrete current commands, never the read View.
func (h *servingHTTP) registerTOTP() {
	for _, route := range []struct {
		method, path string
		handler      servingHTTPHandler
	}{
		{http.MethodGet, "/auth/totp", h.totpStatus},
		{http.MethodPost, "/auth/totp/setup", h.beginTOTP},
		{http.MethodPost, "/auth/totp/confirm", h.confirmTOTP},
		{http.MethodPost, "/auth/totp/disable", h.disableTOTP},
		{http.MethodPost, "/iam/users/{name}/totp/recover", h.recoverTOTP},
	} {
		h.route(route.method, route.path, func(w http.ResponseWriter, r *http.Request) error {
			w.Header().Set("Referrer-Policy", "no-referrer")
			return servingFactorError(route.handler(w, r))
		})
	}
}

func servingFactorError(err error) error {
	if errors.Is(err, security.ErrConflict) {
		return &auth.Error{Status: http.StatusConflict, Key: "conflict", Message: "authenticator state changed; begin setup again or refresh the target account"}
	}
	return err
}

func (h *servingHTTP) factorCSRF(r *http.Request, v *servingauth.View, proof servingauth.Credential) error {
	if err := h.opts.CrossOrigin.Check(r); err != nil {
		return &auth.Error{Status: http.StatusForbidden, Key: "csrf", Message: "cross-origin authenticator operation rejected"}
	}
	if proof.Kind == servingauth.Cookie {
		sess, err := v.CookieSession(proof.Value, h.now())
		if err != nil {
			return err
		}
		return auth.CheckCSRF(r, sess.CSRF, h.opts.CrossOrigin)
	}
	return nil
}

// factorOwner selects the authenticated local cookie owner from the ORIGINAL
// View. Each mutation independently verifies that raw cookie in CommandTx.
func (h *servingHTTP) factorOwner(r *http.Request) (*servingauth.View, auth.Principal, servingauth.Credential, error) {
	v, p, err := h.readPrincipal(r.Context())
	if err != nil {
		return nil, p, servingauth.Credential{}, err
	}
	proof, err := servinghttp.Credential(r.Context())
	if err != nil {
		return nil, p, proof, err
	}
	if proof.Kind != servingauth.Cookie || p.Kind != auth.KindUser || p.Provider != "local" || p.AccountUID == "" || p.MustChangePassword {
		return nil, p, proof, servingauth.ErrDenied
	}
	if err := h.factorCSRF(r, v, proof); err != nil {
		return nil, p, proof, err
	}
	return v, p, proof, nil
}

func (h *servingHTTP) totpStatus(w http.ResponseWriter, r *http.Request) error {
	v, p, _, err := h.factorOwner(r)
	if err != nil {
		return err
	}
	objects, err := v.Objects("LocalUser")
	if err != nil {
		return err
	}
	for _, object := range objects {
		u := object.(*internalv1.LocalUser)
		if u.Name == p.Name && string(u.UID) == p.AccountUID && !u.Spec.Disabled && u.DeletionTimestamp == nil {
			// Complete View preparation already checked the active seed/replay
			// pair. Only the boolean crosses the public projection boundary.
			return servingHTTPJSON(w, http.StatusOK, api.TOTPStatus{Enabled: u.Spec.TOTPSecret != ""})
		}
	}
	return servingauth.ErrCredentials
}

func (h *servingHTTP) factorCommand(r *http.Request, original auth.Principal, cookie servingauth.Credential, password, oldCode string, fn func(context.Context, *sqlite.CommandTx, servingauth.Dependencies, servingauth.Credential) error) error {
	return h.opts.Runtime.currentAuthCommand(r.Context(), func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		p, err := servingauth.AuthenticateCurrentWithClock(ctx, tx, deps, cookie, h.now)
		if err != nil {
			return err
		}
		if cookie.Kind != servingauth.Cookie || p.Kind != auth.KindUser || p.Provider != "local" || p.MustChangePassword || p.AccountUID == "" || p.AccountUID != original.AccountUID || p.Name != original.Name || p.ID != original.ID {
			return servingauth.ErrDenied
		}
		return fn(ctx, tx, deps, servingauth.Credential{Kind: servingauth.Password, Username: p.Name, Value: password, TOTPCode: oldCode})
	})
}

func (h *servingHTTP) factorAdmissionFor(r *http.Request, name, code string) error {
	if err := h.checkLoginLimit(r, name); err != nil {
		return err
	}
	return h.factorAdmission(name, code)
}

func (h *servingHTTP) beginTOTP(w http.ResponseWriter, r *http.Request) error {
	_, p, cookie, err := h.factorOwner(r)
	if err != nil {
		return err
	}
	var req api.TOTPProofRequest
	if err := servingFactorDecode(w, r, &req); err != nil {
		return err
	}
	if err := h.factorAdmissionFor(r, p.Name, req.TOTPCode); err != nil {
		return err
	}
	var setup security.FactorSetup
	err = h.factorCommand(r, p, cookie, req.CurrentPassword, req.TOTPCode, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies, proof servingauth.Credential) error {
		var err error
		setup, err = security.BeginTOTPWithClock(ctx, tx, deps, proof, h.now)
		return err
	})
	h.loginResult(r, p.Name, err)
	if err != nil {
		return err
	}
	if err := setup.ValidAt(h.now()); err != nil {
		return err
	}
	challenge, secret := setup.Disclosure()
	uri := url.URL{Scheme: "otpauth", Host: "totp", Path: "/kudurru:" + p.Name, RawQuery: url.Values{
		"secret": {secret}, "issuer": {"kudurru"}, "algorithm": {"SHA1"}, "digits": {"6"}, "period": {"30"},
	}.Encode()}
	return servingHTTPJSON(w, http.StatusOK, api.TOTPSetupResponse{Challenge: challenge, Secret: secret, OTPAuthURI: uri.String(), ExpiresAt: setup.ExpiresAt()})
}

func (h *servingHTTP) confirmTOTP(w http.ResponseWriter, r *http.Request) error {
	_, p, cookie, err := h.factorOwner(r)
	if err != nil {
		return err
	}
	var req api.TOTPConfirmRequest
	if err := servingFactorDecode(w, r, &req); err != nil {
		return err
	}
	if err := h.factorAdmissionFor(r, p.Name, req.Code); err != nil {
		return err
	}
	err = h.factorCommand(r, p, cookie, req.CurrentPassword, req.TOTPCode, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies, proof servingauth.Credential) error {
		return security.ConfirmTOTPWithClock(ctx, tx, deps, proof, req.Challenge, req.Code, h.now)
	})
	h.loginResult(r, p.Name, err)
	if err != nil {
		return err
	}
	h.opts.Runtime.publisher.Wake()
	h.opts.Cookie.Clear(w, r)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *servingHTTP) disableTOTP(w http.ResponseWriter, r *http.Request) error {
	_, p, cookie, err := h.factorOwner(r)
	if err != nil {
		return err
	}
	var req api.TOTPProofRequest
	if err := servingFactorDecode(w, r, &req); err != nil {
		return err
	}
	if err := h.factorAdmissionFor(r, p.Name, req.TOTPCode); err != nil {
		return err
	}
	err = h.factorCommand(r, p, cookie, req.CurrentPassword, req.TOTPCode, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies, proof servingauth.Credential) error {
		return security.DisableTOTPWithClock(ctx, tx, deps, proof, h.now)
	})
	h.loginResult(r, p.Name, err)
	if err != nil {
		return err
	}
	h.opts.Runtime.publisher.Wake()
	h.opts.Cookie.Clear(w, r)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *servingHTTP) recoverTOTP(w http.ResponseWriter, r *http.Request) error {
	v, p, err := h.permission(r.Context(), "users", "write")
	if err != nil {
		return err
	}
	proof, err := servinghttp.Credential(r.Context())
	if err != nil {
		return err
	}
	if err := h.factorCSRF(r, v, proof); err != nil {
		return err
	}
	var req api.TOTPRecoverRequest
	if err := servingFactorDecode(w, r, &req); err != nil {
		return err
	}
	name := r.PathValue("name")
	if len(name) > 253 || len(validation.IsDNS1123Subdomain(name)) != 0 {
		return servingHTTPInvalid("invalid recovery target")
	}
	target := totp.Account{Name: name, UID: req.UID}
	err = h.opts.Runtime.currentAuthCommand(r.Context(), func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		return security.RecoverTOTPWithClock(ctx, tx, deps, proof, target, h.now)
	})
	if err != nil {
		return err
	}
	h.opts.Runtime.publisher.Wake()
	if proof.Kind == servingauth.Cookie && p.Provider == "local" && p.Name == target.Name && p.AccountUID == target.UID {
		h.opts.Cookie.Clear(w, r)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// servingFactorDecode caps bytes before allocating strings and rejects all
// duplicate/unknown/case-aliased keys, non-string fields and trailing values.
func servingFactorDecode(w http.ResponseWriter, r *http.Request, out any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	raw, err := io.ReadAll(r.Body)
	defer clear(raw)
	if err != nil || !utf8.Valid(raw) {
		return servingHTTPInvalid("invalid authenticator request")
	}
	allowed := []string{"currentPassword", "totpCode"}
	switch out.(type) {
	case *api.TOTPConfirmRequest:
		allowed = append(allowed, "challenge", "code")
	case *api.TOTPRecoverRequest:
		allowed = []string{"uid"}
	case *api.TOTPProofRequest:
	default:
		return servingauth.ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return servingHTTPInvalid("invalid authenticator request")
	}
	fields := make(map[string]string, len(allowed))
	for d.More() {
		token, err := d.Token()
		key, ok := token.(string)
		if err != nil || !ok || !factorFieldAllowed(allowed, key) {
			return servingHTTPInvalid("invalid authenticator request")
		}
		if _, duplicate := fields[key]; duplicate {
			return servingHTTPInvalid("invalid authenticator request")
		}
		value, err := d.Token()
		text, ok := value.(string)
		if err != nil || !ok {
			return servingHTTPInvalid("invalid authenticator request")
		}
		fields[key] = text
	}
	if last, err := d.Token(); err != nil || last != json.Delim('}') {
		return servingHTTPInvalid("invalid authenticator request")
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return servingHTTPInvalid("invalid authenticator request")
	}
	if _, recoverRequest := out.(*api.TOTPRecoverRequest); recoverRequest {
		uid := fields["uid"]
		if uid == "" || len(uid) > 512 || !utf8.ValidString(uid) || strings.ContainsRune(uid, 0) {
			return servingHTTPInvalid("invalid recovery target UID")
		}
		*out.(*api.TOTPRecoverRequest) = api.TOTPRecoverRequest{UID: uid}
		return nil
	}
	password, oldCode := fields["currentPassword"], fields["totpCode"]
	if password == "" || len(password) > users.MaxPasswordLen || oldCode != "" && !factorCodeValid(oldCode) {
		return servingHTTPInvalid("current password and authenticator proof must fit their limits")
	}
	switch req := out.(type) {
	case *api.TOTPProofRequest:
		*req = api.TOTPProofRequest{CurrentPassword: password, TOTPCode: oldCode}
	case *api.TOTPConfirmRequest:
		challenge, code := fields["challenge"], fields["code"]
		if challenge == "" || len(challenge) > 8<<10 || !factorCodeValid(code) {
			return servingHTTPInvalid("invalid authenticator challenge or code")
		}
		*req = api.TOTPConfirmRequest{CurrentPassword: password, TOTPCode: oldCode, Challenge: challenge, Code: code}
	}
	return nil
}

func factorFieldAllowed(allowed []string, key string) bool {
	for _, field := range allowed {
		if field == key {
			return true
		}
	}
	return false
}
func factorCodeValid(code string) bool {
	if len(code) != totp.Digits {
		return false
	}
	for _, digit := range []byte(code) {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}
