package main

import (
	"context"
	"net/http"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	// Expensive new-password preparation precedes dependency/SQLite locks.
	hash, err := h.opts.Hasher.Hash(r.Context(), req.NewPassword)
	if err != nil {
		return err
	}
	proof, err := servinghttp.Credential(r.Context())
	if err != nil {
		return err
	}
	var enrollment security.Enrollment
	var updated auth.Principal
	err = h.opts.Runtime.currentAuthCommand(r.Context(), func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		var authenticatedAt time.Time
		clock := func() time.Time {
			now := h.now()
			if authenticatedAt.IsZero() {
				authenticatedAt = now.UTC()
			}
			return now
		}
		p, err := servingauth.AuthenticateCurrentWithClock(ctx, tx, deps, proof, clock)
		if err != nil {
			return err
		}
		if p.Kind != auth.KindUser || p.Provider != "local" || p.AccountUID != original.AccountUID || p.ID != original.ID {
			return servingauth.ErrCredentials
		}
		var user internalv1.LocalUser
		if err = tx.Get(ctx, store.Key{Kind: "LocalUser", Name: p.Name}, &user); err != nil {
			return err
		}
		if string(user.UID) != p.AccountUID || user.Spec.Disabled {
			return servingauth.ErrCredentials
		}
		// Self-service may update explicitly supported local account owners,
		// preserving ownership. File/controller accounts need their own policy;
		// a password proof never grants manager adoption or Force.
		manager := store.ManagedBy(&user)
		if manager != store.ManagerSystem && manager != store.ManagerAPI {
			return &auth.Error{Status: http.StatusConflict, Key: "managed_by", Message: "this account's password is managed outside self-service"}
		}
		if proof.Kind == servingauth.Cookie {
			// IssueLocal below checks the new password and consumes the factor ONCE.
			// Current cookie possession and the old password are required independently.
			ok, err := deps.Hasher.Verify(ctx, user.Spec.PasswordHash, req.CurrentPassword)
			if err != nil || !ok {
				return servingauth.ErrCredentials
			}
		} else {
			verified, err := servingauth.AuthenticateCurrentWithClock(ctx, tx, deps, servingauth.Credential{Kind: servingauth.Password, Username: p.Name, Value: req.CurrentPassword, TOTPCode: req.TOTPCode}, h.now)
			if err != nil {
				return err
			}
			if verified.AccountUID != p.AccountUID {
				return servingauth.ErrCredentials
			}
		}
		// Reuse the successful proof's time. A later transient clock regression
		// must not put the revocation floor before an existing valid session.
		floor := authenticatedAt
		user.Spec.PasswordHash, user.Spec.MustChangePassword = hash, false
		out, err := security.Apply(ctx, tx, &user, store.ApplyOptions{Manager: manager, ExpectedResourceVersion: user.ResourceVersion}, floor)
		if err != nil {
			return err
		}
		current := out.(*internalv1.LocalUser)
		changed := metav1.NewTime(floor)
		current.Status.PasswordChangedAt = &changed
		if err = tx.UpdateStatus(ctx, current); err != nil {
			return err
		}
		if req.RevokeTokens {
			list, err := tx.List(ctx, "APIToken", store.ListOptions{Limit: servingauth.MaxObjects + 1})
			if err != nil {
				return err
			}
			if len(list.Items) > servingauth.MaxObjects || list.Continue != "" {
				return servingauth.ErrBounds
			}
			for _, o := range list.Items {
				t := o.(*internalv1.APIToken)
				if servingHTTPOwnsLocalUserToken(t, p.Name, p.AccountUID) {
					if err = tx.Delete(ctx, store.KeyOf(t), store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
						if store.IsManagedBy(err) {
							return &auth.Error{Status: http.StatusConflict, Key: "managed_by", Message: "a personal token is managed outside self-service"}
						}
						return err
					}
				}
			}
		}
		updated = p
		updated.MustChangePassword = false
		if proof.Kind == servingauth.Cookie {
			issued := h.now()
			if !issued.After(floor) {
				return servingauth.ErrCredentials
			}
			enrollment, err = security.IssueLocal(ctx, tx, deps, servingauth.Credential{Kind: servingauth.Password, Username: p.Name, Value: req.NewPassword, TOTPCode: req.TOTPCode}, issued)
			if err != nil {
				return err
			}
			return h.enrollmentClock(ctx, tx, enrollment, issued)
		}
		return nil
	})
	h.loginResult(r, original.Name, err)
	if err != nil {
		return err
	}
	h.opts.Runtime.publisher.Wake()
	if proof.Kind == servingauth.Cookie {
		return h.sealEnrollment(w, r, enrollment)
	}
	// The response is prepared from the postcommit account, never the old lease's
	// must-change flag. Password change does not broaden the original token scopes.
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
						if string(u.UID) == updated.AccountUID && u.Spec.PasswordHash == hash {
							out, e := h.sessionInfo(v, updated, nil)
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

// servingHTTPOwnsLocalUserToken matches both provider spellings accepted by the
// local token verifier. Account floors alone cannot revoke personal tokens.
func servingHTTPOwnsLocalUserToken(token *internalv1.APIToken, name, uid string) bool {
	owner := token.Spec.Owner
	return owner.Kind == internalv1.OwnerUser && owner.Namespace == "" && owner.Name == name && owner.UID == uid &&
		owner.ID == "local:"+name && (owner.Provider == "local" || owner.Provider == "")
}
