package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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
	"k8s.io/apimachinery/pkg/util/validation"
)

func (h *servingHTTP) registerLocalUsers() {
	h.route(http.MethodPost, "/iam/users", func(w http.ResponseWriter, r *http.Request) error {
		return servingLocalUserError(h.createLocalUser(w, r))
	})
	h.route(http.MethodPost, "/iam/users/{name}/password", func(w http.ResponseWriter, r *http.Request) error {
		return servingLocalUserError(h.resetLocalUserPassword(w, r))
	})
}

func servingLocalUserError(err error) error {
	if errors.Is(err, store.ErrConflict) {
		return &auth.Error{Status: http.StatusConflict, Key: "conflict", Message: "local account changed or already exists; refresh before making a new request"}
	}
	var managed *store.ErrManagedBy
	if errors.As(err, &managed) {
		return &auth.Error{Status: http.StatusConflict, Key: "managed_by", Message: "local account or credential belongs to another manager"}
	}
	return err
}

func (h *servingHTTP) localUserPreflight(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("Referrer-Policy", "no-referrer")
	v, _, err := h.permission(r.Context(), "users", "write")
	if err != nil {
		return err
	}
	proof, err := servinghttp.Credential(r.Context())
	if err != nil {
		return err
	}
	return h.factorCSRF(r, v, proof)
}

func (h *servingHTTP) createLocalUser(w http.ResponseWriter, r *http.Request) error {
	if err := h.localUserPreflight(w, r); err != nil {
		return err
	}
	var req api.CreateLocalUserRequest
	if err := servingLocalUserDecode(w, r, &req); err != nil {
		return err
	}
	// Hash only after bounded decoding/preflight, before every command pin.
	hash, err := h.opts.Hasher.Hash(r.Context(), req.Password)
	if err != nil {
		return servingLocalUserHashError(err)
	}
	req.Password = ""
	var created api.IAMUser
	err = h.localUserCommand(r, req.Name, func(ctx context.Context, tx *sqlite.CommandTx, _ auth.Principal, observed time.Time) error {
		var err error
		created, err = stageServingLocalUserCreate(ctx, tx, req, hash, observed)
		return err
	})
	if err != nil {
		return err
	}
	h.opts.Runtime.publisher.Wake()
	return servingHTTPJSON(w, http.StatusCreated, created)
}

func (h *servingHTTP) resetLocalUserPassword(w http.ResponseWriter, r *http.Request) error {
	if err := h.localUserPreflight(w, r); err != nil {
		return err
	}
	name := r.PathValue("name")
	if !servingLocalUserName(name) {
		return servingHTTPInvalid("invalid local account name")
	}
	var req api.ResetLocalUserPasswordRequest
	if err := servingLocalUserDecode(w, r, &req); err != nil {
		return err
	}
	hash, err := h.opts.Hasher.Hash(r.Context(), req.NewPassword)
	if err != nil {
		return servingLocalUserHashError(err)
	}
	req.NewPassword = ""
	var self bool
	err = h.localUserCommand(r, name, func(ctx context.Context, tx *sqlite.CommandTx, p auth.Principal, observed time.Time) error {
		if err := stageServingLocalUserReset(ctx, tx, name, req, hash, observed); err != nil {
			return err
		}
		self = p.Kind == auth.KindUser && p.Provider == "local" && p.Name == name && p.AccountUID == req.UID
		return nil
	})
	if err != nil {
		return err
	}
	h.opts.Runtime.publisher.Wake()
	proof, err := servinghttp.Credential(r.Context())
	if err != nil {
		return err
	}
	if self && proof.Kind == servingauth.Cookie {
		h.opts.Cookie.Clear(w, r)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// localUserCommand verifies the original raw proof once. The first actual
// verification clock sample is also the mutation floor; later reads (including
// the installed final check) remain actual time. Intentional self-revocation is
// never followed by a second authentication against the newly staged state.
func (h *servingHTTP) localUserCommand(r *http.Request, name string, fn func(context.Context, *sqlite.CommandTx, auth.Principal, time.Time) error) error {
	proof, err := servinghttp.Credential(r.Context())
	if err != nil {
		return err
	}
	return h.opts.Runtime.currentAuthCommand(r.Context(), func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		var observed time.Time
		clock := func() time.Time {
			now := h.now()
			if observed.IsZero() {
				observed = now.UTC()
			}
			return now
		}
		attrs := servingHTTPCluster("users", "write")
		attrs.Name = name
		p, _, err := servingauth.ReauthorizeCurrentWithClock(ctx, tx, deps, proof, attrs, clock)
		if err != nil {
			return err
		}
		return fn(ctx, tx, p, observed)
	})
}

func stageServingLocalUserCreate(ctx context.Context, tx *sqlite.CommandTx, req api.CreateLocalUserRequest, hash string, observed time.Time) (api.IAMUser, error) {
	u := &internalv1.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: req.Name}, Spec: internalv1.LocalUserSpec{DisplayName: req.DisplayName, PasswordHash: hash, MustChangePassword: true, Groups: []string{}}}
	out, err := security.Apply(ctx, tx, u, store.ApplyOptions{Manager: store.ManagerAPI, CreateOnly: true}, observed)
	if err != nil {
		return api.IAMUser{}, err
	}
	u = out.(*internalv1.LocalUser)
	at := metav1.NewTime(observed)
	u.Status.PasswordChangedAt = &at
	if err := tx.UpdateStatus(ctx, u); err != nil {
		return api.IAMUser{}, err
	}
	// UpdateStatus does not refresh u's RV. Return the final actual staged row.
	if err := tx.Get(ctx, store.KeyOf(u), u); err != nil {
		return api.IAMUser{}, err
	}
	created := api.IAMUser{Name: u.Name, UID: string(u.UID), ResourceVersion: u.ResourceVersion, DisplayName: u.Spec.DisplayName, MustChangePassword: u.Spec.MustChangePassword, Groups: []string{}}
	if u.Status.PasswordChangedAt != nil {
		stamp := u.Status.PasswordChangedAt.Time
		created.PasswordChangedAt = &stamp
	}
	return created, nil
}

func stageServingLocalUserReset(ctx context.Context, tx *sqlite.CommandTx, name string, req api.ResetLocalUserPasswordRequest, hash string, observed time.Time) error {
	var u internalv1.LocalUser
	if err := tx.Get(ctx, store.Key{Kind: "LocalUser", Name: name}, &u); err != nil {
		return err
	}
	if string(u.UID) != req.UID || u.ResourceVersion != req.ResourceVersion || u.DeletionTimestamp != nil {
		return store.ErrConflict
	}
	if u.Spec.BuiltinAdmin {
		return servingauth.ErrDenied
	}
	if owner := store.ManagedBy(&u); owner != store.ManagerAPI {
		return &store.ErrManagedBy{Key: store.KeyOf(&u), Owner: owner}
	}
	u.Spec.PasswordHash, u.Spec.MustChangePassword = hash, true
	out, err := security.Apply(ctx, tx, &u, store.ApplyOptions{Manager: store.ManagerAPI, ExpectedResourceVersion: req.ResourceVersion}, observed)
	if err != nil {
		return err
	}
	u = *out.(*internalv1.LocalUser)
	at := metav1.NewTime(observed)
	u.Status.PasswordChangedAt = &at
	if err := tx.UpdateStatus(ctx, &u); err != nil {
		return err
	}
	list, err := tx.List(ctx, "APIToken", store.ListOptions{Limit: servingauth.MaxObjects + 1})
	if err != nil {
		return err
	}
	if len(list.Items) > servingauth.MaxObjects || list.Continue != "" {
		return servingauth.ErrBounds
	}
	for _, object := range list.Items {
		token := object.(*internalv1.APIToken)
		// Empty provider is the existing verifier's legacy local spelling; both
		// valid local spellings must be revoked without matching other owners.
		if !servingHTTPOwnsLocalUserToken(token, name, req.UID) {
			continue
		}
		if err := tx.Delete(ctx, store.KeyOf(token), store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
	}
	return nil
}

func servingLocalUserName(name string) bool {
	return name != "" && len(name) <= 253 && name == users.NormalizeUsername(name) && len(validation.IsDNS1123Subdomain(name)) == 0
}

// servingLocalUserDecode admits only the exact small string DTO, never an
// internal LocalUser document, ownership flags, or reflected validation errors.
func servingLocalUserDecode(w http.ResponseWriter, r *http.Request, out any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	raw, err := io.ReadAll(r.Body)
	defer clear(raw)
	invalid := func() error { return servingHTTPInvalid("invalid local account request") }
	if err != nil || !utf8.Valid(raw) {
		return invalid()
	}
	var allowed []string
	switch out.(type) {
	case *api.CreateLocalUserRequest:
		allowed = []string{"name", "displayName", "password"}
	case *api.ResetLocalUserPasswordRequest:
		allowed = []string{"uid", "resourceVersion", "newPassword"}
	default:
		return servingauth.ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	if token, err := d.Token(); err != nil || token != json.Delim('{') {
		return invalid()
	}
	fields := make(map[string]string, len(allowed))
	for d.More() {
		token, err := d.Token()
		key, ok := token.(string)
		if err != nil || !ok || !factorFieldAllowed(allowed, key) {
			return invalid()
		}
		if _, duplicate := fields[key]; duplicate {
			return invalid()
		}
		value, err := d.Token()
		text, ok := value.(string)
		if err != nil || !ok {
			return invalid()
		}
		fields[key] = text
	}
	if last, err := d.Token(); err != nil || last != json.Delim('}') {
		return invalid()
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return invalid()
	}
	switch req := out.(type) {
	case *api.CreateLocalUserRequest:
		name, display := fields["name"], fields["displayName"]
		if !servingLocalUserName(name) || len(display) > 256 || strings.ContainsFunc(display, unicode.IsControl) || users.CheckNewPassword(fields["password"]) != nil {
			return invalid()
		}
		*req = api.CreateLocalUserRequest{Name: name, DisplayName: display, Password: fields["password"]}
	case *api.ResetLocalUserPasswordRequest:
		uid, rv := fields["uid"], fields["resourceVersion"]
		if uid == "" || len(uid) > 512 || strings.ContainsFunc(uid, unicode.IsControl) || rv == "" || len(rv) > 64 || strings.ContainsFunc(rv, unicode.IsControl) || users.CheckNewPassword(fields["newPassword"]) != nil {
			return invalid()
		}
		*req = api.ResetLocalUserPasswordRequest{UID: uid, ResourceVersion: rv, NewPassword: fields["newPassword"]}
	}
	return nil
}

func servingLocalUserHashError(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return &auth.Error{Status: http.StatusServiceUnavailable, Key: "unavailable", Message: "password preparation unavailable"}
}
