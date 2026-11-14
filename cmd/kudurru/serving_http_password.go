package main

import (
	"context"
	"errors"
	"net/http"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/loginlimit"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func (h *servingHTTP) password(w http.ResponseWriter, r *http.Request) error {
	_, original, err := h.readPrincipal(r.Context())
	if err != nil {
		return err
	}
	if original.Kind != auth.KindUser || original.Provider != "local" {
		return &auth.Error{Status: 400, Key: "not_local_user", Message: "only local accounts have a kudurru password"}
	}
	var req api.PasswordChangeRequest
	if err := servingHTTPDecode(w, r, &req); err != nil {
		return err
	}
	if err := users.CheckNewPassword(req.NewPassword); err != nil {
		return servingHTTPInvalid("newPassword does not meet password length limits")
	}
	if req.CurrentPassword == req.NewPassword {
		return servingHTTPInvalid("newPassword must differ from the current password")
	}
	if req.CurrentPassword == "" || len(req.CurrentPassword) > users.MaxPasswordLen {
		return servingHTTPInvalid("currentPassword is required and must fit password limits")
	}
	if err := h.checkLoginLimit(r, original.Name); err != nil {
		return err
	}
	if err := h.factorAdmission(original.Name, req.TOTPCode); err != nil {
		return err
	}
	if handled, err := h.forwardPassword(w, r, original, req); handled {
		return err
	}
	// Hash outside the locks, retaining this original bound through completion.
	originalCtx, cancelOperation := context.WithTimeout(r.Context(), loginlimit.Lifetime)
	defer cancelOperation()
	r = r.WithContext(originalCtx)
	change, err := security.PrepareLocalPasswordChange(originalCtx, h.opts.Hasher, req.CurrentPassword, req.NewPassword, req.TOTPCode, req.RevokeTokens)
	if err != nil {
		return err
	}
	proof, err := servinghttp.Credential(r.Context())
	if err != nil {
		return err
	}
	var enrollment security.Enrollment
	var updated auth.Principal
	var refused error
	err = h.opts.Runtime.currentAuthCommand(originalCtx, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		var result security.PasswordChangeResult
		var err error
		result, refused, err = security.ChangeLocalPasswordWithAdmission(ctx, tx, deps, proof, original, change, h.clientKey(r), h.now)
		if err != nil {
			return err
		}
		if refused != nil {
			return nil
		}
		enrollment, updated = result.Enrollment, result.Principal
		if proof.Kind == servingauth.Cookie {
			return h.enrollmentClock(ctx, tx, enrollment, result.IssuedAt)
		}
		return nil
	})
	if err != nil {
		return err
	}
	h.opts.Runtime.publisher.Wake()
	if refused != nil {
		var locked *loginlimit.Locked
		if errors.As(refused, &locked) {
			return &auth.Error{Status: http.StatusTooManyRequests, Key: "rate_limited", Message: "too many failed attempts; try again later", RetryAfter: locked.RetryAfter}
		}
		h.loginResult(r, original.Name, refused)
		return refused
	}
	h.loginResult(r, original.Name, nil)
	if proof.Kind == servingauth.Cookie {
		return h.sealEnrollment(w, r, enrollment)
	}
	// The response is prepared from the postcommit account, never the old lease's
	// must-change flag. Password change does not broaden the original token scopes.
	return h.answerChangedPassword(w, r, updated.AccountUID, change.MatchesPasswordHash, func(*servingauth.View) (auth.Principal, error) { return updated, nil })
}

// servingHTTPOwnsLocalUserToken matches both provider spellings accepted by the
// local token verifier. Account floors alone cannot revoke personal tokens.
func servingHTTPOwnsLocalUserToken(token *internalv1.APIToken, name, uid string) bool {
	return security.LocalUserOwnsToken(token, name, uid)
}
