package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// The IAM writers of D396 run on the complete SQLite runtime only. Each one
// follows D294: the original raw proof is verified again inside the one
// command that also checks the exact reviewed target, stages the object with
// its revocation floor and deletes the credentials it ends, so that effect,
// floor and audit commit together. Nothing here retries, refreshes a review or
// adopts an object another manager owns.

func (s *servingRuntime) IAMObjectCommandsAvailable() bool { return s != nil }

func (h *servingHTTP) registerIAMWriters() {
	h.route(http.MethodPatch, "/iam/users/{name}", func(w http.ResponseWriter, r *http.Request) error {
		return servingLocalUserError(h.updateLocalUser(w, r))
	})
	h.route(http.MethodDelete, "/iam/users/{name}", func(w http.ResponseWriter, r *http.Request) error {
		return servingLocalUserError(h.deleteLocalUser(w, r))
	})
	sa := "/iam/serviceaccounts/{namespace}/{name}"
	h.route(http.MethodGet, sa, h.getServiceAccount)
	h.route(http.MethodPost, "/iam/serviceaccounts", func(w http.ResponseWriter, r *http.Request) error {
		return iamObjectError("ServiceAccount", h.mutateServiceAccount(w, r))
	})
	h.route(http.MethodPut, sa, func(w http.ResponseWriter, r *http.Request) error {
		return iamObjectError("ServiceAccount", h.mutateServiceAccount(w, r))
	})
	h.route(http.MethodDelete, sa, func(w http.ResponseWriter, r *http.Request) error {
		return iamObjectError("ServiceAccount", h.mutateServiceAccount(w, r))
	})
	h.route(http.MethodPost, sa+"/tokens", func(w http.ResponseWriter, r *http.Request) error {
		return iamObjectError("ServiceAccount", h.createServiceAccountToken(w, r))
	})
	idp := "/iam/identityproviders/{name}"
	h.route(http.MethodGet, idp, h.getIdentityProvider)
	h.route(http.MethodPost, "/iam/identityproviders", func(w http.ResponseWriter, r *http.Request) error {
		return iamObjectError("IdentityProvider", h.mutateIdentityProvider(w, r))
	})
	h.route(http.MethodPut, idp, func(w http.ResponseWriter, r *http.Request) error {
		return iamObjectError("IdentityProvider", h.mutateIdentityProvider(w, r))
	})
	h.route(http.MethodDelete, idp, func(w http.ResponseWriter, r *http.Request) error {
		return iamObjectError("IdentityProvider", h.mutateIdentityProvider(w, r))
	})
}

// iamObjectError maps store refusals to the reviewed-command answers. Messages
// are static: they never echo the request body.
func iamObjectError(kind string, err error) error {
	if errors.Is(err, store.ErrConflict) {
		return &auth.Error{Status: http.StatusConflict, Key: "conflict", Message: kind + " changed or already exists; review before making a new request"}
	}
	var managed *store.ErrManagedBy
	if errors.As(err, &managed) {
		return &auth.Error{Status: http.StatusConflict, Key: "managed_by", Message: kind + " belongs to another manager"}
	}
	var invalid *validator.Error
	if errors.As(err, &invalid) {
		return servingHTTPInvalid("invalid " + kind + " fields")
	}
	return err
}

// iamCommandPreflight checks the declared permission(s) against the original
// view and CSRF for cookies, before the body is read.
func (h *servingHTTP) iamCommandPreflight(w http.ResponseWriter, r *http.Request, attrs ...rbac.Attributes) ([]byte, error) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	var v *servingauth.View
	for _, a := range attrs {
		var err error
		if v, _, err = h.permission(r.Context(), a.Resource, a.Verb); err != nil {
			return nil, err
		}
	}
	proof, err := servinghttp.Credential(r.Context())
	if err != nil {
		return nil, err
	}
	if err := h.factorCSRF(r, v, proof); err != nil {
		return nil, err
	}
	if r.URL.RawQuery != "" {
		return nil, servingHTTPInvalid("IAM commands do not accept query parameters")
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		return nil, servingHTTPInvalid("invalid IAM command")
	}
	return raw, nil
}

// iamCommand verifies the original raw proof once inside the command for every
// attribute. The first verification clock sample is the mutation floor.
func (h *servingHTTP) iamCommand(r *http.Request, attrs []rbac.Attributes, fn func(context.Context, *sqlite.CommandTx, servingauth.Dependencies, auth.Principal, time.Time) error) error {
	proof, err := servinghttp.Credential(r.Context())
	if err != nil {
		return err
	}
	return h.opts.Runtime.currentAuthCommand(r.Context(), func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		var observed time.Time
		clock := func() time.Time {
			now := h.now()
			if observed.IsZero() {
				observed = now
			}
			return now
		}
		p, err := servingauth.ReauthorizeCurrentAllWithClock(ctx, tx, deps, proof, attrs, clock)
		if err != nil {
			return err
		}
		return fn(ctx, tx, deps, p, observed)
	})
}

// iamTarget narrows a declared permission to one object. The resource and
// verb stay in the servingHTTPCluster call, which the permission lints read.
func iamTarget(a rbac.Attributes, namespace, name string) rbac.Attributes {
	a.Namespace, a.Name = namespace, name
	return a
}

func validObjectReview(q api.ObjectReview) bool {
	return validRoleBindingReview(api.RoleBindingReview(q))
}

// deleteOwnedTokens deletes, in this transaction, every API token whose owner
// the predicate matches. The tokens can no longer authenticate after the
// floor in the same commit; deleting them keeps the inventory honest.
func deleteOwnedTokens(ctx context.Context, tx *sqlite.CommandTx, owned func(*internalv1.APIToken) bool) error {
	list, err := tx.List(ctx, "APIToken", store.ListOptions{Limit: servingauth.MaxObjects + 1})
	if err != nil {
		return err
	}
	if len(list.Items) > servingauth.MaxObjects || list.Continue != "" {
		return servingauth.ErrBounds
	}
	for _, object := range list.Items {
		token := object.(*internalv1.APIToken)
		if !owned(token) {
			continue
		}
		if err := tx.Delete(ctx, store.KeyOf(token), store.DeleteOptions{Manager: store.ManagerSystem, Force: true}); err != nil {
			return err
		}
	}
	return nil
}

// --- local accounts ---

// validLocalGroup admits a local group name that cannot collide with a
// provider-prefixed group ("<idp>:<group>").
func validLocalGroup(g string) bool {
	return g != "" && len(g) <= 253 && utf8.ValidString(g) && strings.TrimSpace(g) == g && !strings.ContainsRune(g, ':') && !strings.ContainsFunc(g, unicode.IsControl)
}

func (h *servingHTTP) updateLocalUser(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("name")
	raw, err := h.iamCommandPreflight(w, r, servingHTTPCluster("users", "write"))
	if err != nil {
		return err
	}
	if !servingLocalUserName(name) {
		return servingHTTPInvalid("invalid local account name")
	}
	var req api.UpdateLocalUserRequest
	if err := decodeReviewedCommandJSON(raw, &req, "invalid local account request"); err != nil {
		return err
	}
	review := api.ObjectReview{UID: req.UID, ResourceVersion: req.ResourceVersion}
	if !validObjectReview(review) || req.DisplayName == nil && req.Groups == nil && req.Disabled == nil {
		return servingHTTPInvalid("exact uid and resourceVersion and at least one changed field are required")
	}
	if d := req.DisplayName; d != nil && (len(*d) > 256 || !utf8.ValidString(*d) || strings.ContainsFunc(*d, unicode.IsControl)) {
		return servingHTTPInvalid("invalid display name")
	}
	if g := req.Groups; g != nil {
		if len(*g) > 64 {
			return servingHTTPInvalid("at most 64 groups are allowed")
		}
		for i, group := range *g {
			if !validLocalGroup(group) || slices.Contains((*g)[:i], group) {
				return servingHTTPInvalid("invalid or duplicate local group name")
			}
		}
	}
	var out api.IAMUser
	var revoked, self bool
	err = h.iamCommand(r, []rbac.Attributes{iamTarget(servingHTTPCluster("users", "write"), "", name)}, func(ctx context.Context, tx *sqlite.CommandTx, _ servingauth.Dependencies, p auth.Principal, observed time.Time) error {
		u, err := reviewedLocalUser(ctx, tx, name, review)
		if err != nil {
			return err
		}
		if req.DisplayName != nil {
			u.Spec.DisplayName = *req.DisplayName
		}
		if req.Groups != nil {
			groups := slices.Clone(*req.Groups)
			revoked = revoked || !slices.Equal(groups, u.Spec.Groups)
			u.Spec.Groups = groups
		}
		if req.Disabled != nil {
			revoked = revoked || *req.Disabled != u.Spec.Disabled
			u.Spec.Disabled = *req.Disabled
		}
		applied, err := security.Apply(ctx, tx, u, store.ApplyOptions{Manager: store.ManagerAPI, ExpectedResourceVersion: review.ResourceVersion}, observed)
		if err != nil {
			return err
		}
		if revoked {
			if err := deleteOwnedTokens(ctx, tx, func(t *internalv1.APIToken) bool { return servingHTTPOwnsLocalUserToken(t, name, review.UID) }); err != nil {
				return err
			}
		}
		self = revoked && p.Kind == auth.KindUser && p.Provider == "local" && p.Name == name && p.AccountUID == review.UID
		var final internalv1.LocalUser
		if err := tx.Get(ctx, store.KeyOf(applied), &final); err != nil {
			return err
		}
		out = servingIAMUser(&final)
		return nil
	})
	if err != nil {
		return err
	}
	h.opts.Runtime.publisher.Wake()
	if err := h.clearOwnCookie(w, r, self); err != nil {
		return err
	}
	return servingHTTPJSON(w, http.StatusOK, out)
}

func (h *servingHTTP) deleteLocalUser(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("name")
	raw, err := h.iamCommandPreflight(w, r, servingHTTPCluster("users", "delete"))
	if err != nil {
		return err
	}
	if !servingLocalUserName(name) {
		return servingHTTPInvalid("invalid local account name")
	}
	var review api.ObjectReview
	if err := decodeReviewedCommandJSON(raw, &review, "invalid local account request"); err != nil {
		return err
	}
	if !validObjectReview(review) {
		return servingHTTPInvalid("exact reviewed uid and resourceVersion required")
	}
	var self bool
	err = h.iamCommand(r, []rbac.Attributes{iamTarget(servingHTTPCluster("users", "delete"), "", name)}, func(ctx context.Context, tx *sqlite.CommandTx, _ servingauth.Dependencies, p auth.Principal, observed time.Time) error {
		u, err := reviewedLocalUser(ctx, tx, name, review)
		if err != nil {
			return err
		}
		if err := security.Delete(ctx, tx, u, store.DeleteOptions{Manager: store.ManagerAPI}, observed); err != nil {
			return err
		}
		self = p.Kind == auth.KindUser && p.Provider == "local" && p.Name == name && p.AccountUID == review.UID
		return deleteOwnedTokens(ctx, tx, func(t *internalv1.APIToken) bool { return servingHTTPOwnsLocalUserToken(t, name, review.UID) })
	})
	if err != nil {
		return err
	}
	h.opts.Runtime.publisher.Wake()
	if err := h.clearOwnCookie(w, r, self); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// clearOwnCookie drops the caller's cookie after a command ended its own
// session. The session is already refused by the committed floor.
func (h *servingHTTP) clearOwnCookie(w http.ResponseWriter, r *http.Request, self bool) error {
	if !self {
		return nil
	}
	proof, err := servinghttp.Credential(r.Context())
	if err != nil {
		return err
	}
	if proof.Kind == servingauth.Cookie {
		return h.clearCookie(w, r, "")
	}
	return nil
}

// reviewedLocalUser reads the exact reviewed API-managed ordinary account.
func reviewedLocalUser(ctx context.Context, tx *sqlite.CommandTx, name string, review api.ObjectReview) (*internalv1.LocalUser, error) {
	var u internalv1.LocalUser
	if err := tx.Get(ctx, store.Key{Kind: "LocalUser", Name: name}, &u); err != nil {
		return nil, err
	}
	if string(u.UID) != review.UID || u.ResourceVersion != review.ResourceVersion || u.DeletionTimestamp != nil {
		return nil, store.ErrConflict
	}
	if u.Spec.BuiltinAdmin {
		return nil, servingauth.ErrDenied
	}
	if owner := store.ManagedBy(&u); owner != store.ManagerAPI {
		return nil, &store.ErrManagedBy{Key: store.KeyOf(&u), Owner: owner}
	}
	return &u, nil
}

func servingIAMUser(u *internalv1.LocalUser) api.IAMUser {
	out := api.IAMUser{Name: u.Name, UID: string(u.UID), ResourceVersion: u.ResourceVersion, DisplayName: u.Spec.DisplayName, BuiltinAdmin: u.Spec.BuiltinAdmin, Disabled: u.Spec.Disabled, MustChangePassword: u.Spec.MustChangePassword, Groups: slices.Clone(u.Spec.Groups)}
	if out.Groups == nil {
		out.Groups = []string{}
	}
	if u.Status.LastLoginAt != nil {
		at := u.Status.LastLoginAt.Time
		out.LastLoginAt = &at
	}
	if u.Status.PasswordChangedAt != nil {
		at := u.Status.PasswordChangedAt.Time
		out.PasswordChangedAt = &at
	}
	return out
}

// --- service accounts ---

func serviceAccountPath(r *http.Request) (store.Key, error) {
	key := store.Key{Kind: publicv1.KindServiceAccount, Namespace: r.PathValue("namespace"), Name: r.PathValue("name")}
	if len(validation.IsDNS1123Label(key.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(key.Name)) != 0 {
		return store.Key{}, servingHTTPInvalid("invalid ServiceAccount target")
	}
	return key, nil
}

func (h *servingHTTP) getServiceAccount(w http.ResponseWriter, r *http.Request) error {
	v, _, err := h.permission(r.Context(), "serviceaccounts", "read")
	if err != nil {
		return err
	}
	key, err := serviceAccountPath(r)
	if err != nil {
		return err
	}
	if r.URL.RawQuery != "" {
		return servingHTTPInvalid("invalid ServiceAccount target")
	}
	objects, err := v.Objects(publicv1.KindServiceAccount)
	if err != nil {
		return err
	}
	for _, obj := range objects {
		if store.KeyOf(obj) == key {
			out := obj.(*publicv1.ServiceAccount).DeepCopy()
			out.APIVersion, out.Kind = publicv1.GroupVersion.String(), publicv1.KindServiceAccount
			return servingHTTPJSON(w, http.StatusOK, out)
		}
	}
	return store.ErrNotFound
}

func serviceAccountDesired(in api.ServiceAccountDesired) (*publicv1.ServiceAccount, error) {
	if in.APIVersion != publicv1.GroupVersion.String() || in.Kind != publicv1.KindServiceAccount {
		return nil, servingHTTPInvalid("expected a public v1alpha1 ServiceAccount")
	}
	if _, ok := in.Metadata.Annotations[publicv1.ManagedByAnnotation]; ok {
		return nil, servingHTTPInvalid("ServiceAccount manager is server-owned")
	}
	if len(in.Spec.Description) > 2048 || !utf8.ValidString(in.Spec.Description) {
		return nil, servingHTTPInvalid("invalid ServiceAccount fields")
	}
	obj := &publicv1.ServiceAccount{TypeMeta: metav1.TypeMeta{APIVersion: in.APIVersion, Kind: in.Kind}, ObjectMeta: metav1.ObjectMeta{Name: in.Metadata.Name, Namespace: in.Metadata.Namespace, Labels: in.Metadata.Labels, Annotations: in.Metadata.Annotations}, Spec: publicv1.ServiceAccountSpec{Description: in.Spec.Description, Disabled: in.Spec.Disabled}}
	if err := obj.Validate(); err != nil {
		return nil, servingHTTPInvalid("invalid ServiceAccount fields")
	}
	return obj, nil
}

func serviceAccountOwner(sa *publicv1.ServiceAccount) internalv1.TokenOwner {
	return internalv1.TokenOwner{Kind: internalv1.OwnerServiceAccount, Name: sa.Name, Namespace: sa.Namespace, UID: string(sa.UID), ID: "sa:" + sa.Namespace + "/" + sa.Name, Provider: "kudurru"}
}

func serviceAccountOwnsToken(sa *publicv1.ServiceAccount) func(*internalv1.APIToken) bool {
	return func(t *internalv1.APIToken) bool {
		o := t.Spec.Owner
		return o.Kind == internalv1.OwnerServiceAccount && o.Namespace == sa.Namespace && o.Name == sa.Name && o.UID == string(sa.UID)
	}
}

// reviewedServiceAccount reads the exact reviewed API-owned account. An
// account with private bootstrap authority (PrincipalState) is never changed
// or deleted here.
func reviewedServiceAccount(ctx context.Context, tx *sqlite.CommandTx, key store.Key, review api.ObjectReview) (*publicv1.ServiceAccount, error) {
	var sa publicv1.ServiceAccount
	if err := tx.Get(ctx, key, &sa); err != nil {
		return nil, err
	}
	if string(sa.UID) != review.UID || sa.ResourceVersion != review.ResourceVersion || sa.DeletionTimestamp != nil {
		return nil, store.ErrConflict
	}
	if err := serviceAccountAdministrable(ctx, tx, &sa); err != nil {
		return nil, err
	}
	return &sa, nil
}

// serviceAccountAdministrable admits only an API-owned account without private
// bootstrap authority (PrincipalState). Replace, delete and token issuance all
// apply it, so no API command changes, ends or mints a credential for an
// account another manager owns or one that carries bootstrap authority.
func serviceAccountAdministrable(ctx context.Context, tx *sqlite.CommandTx, sa *publicv1.ServiceAccount) error {
	if owner := store.ManagedBy(sa); owner != store.ManagerAPI {
		return &store.ErrManagedBy{Key: store.KeyOf(sa), Owner: owner}
	}
	var state internalv1.PrincipalState
	switch err := tx.Get(ctx, store.Key{Kind: internalv1.KindPrincipalState, Name: internalv1.PrincipalStateName(serviceAccountOwner(sa))}, &state); {
	case err == nil:
		return servingauth.ErrDenied
	case !errors.Is(err, store.ErrNotFound):
		return err
	}
	return nil
}

func (h *servingHTTP) mutateServiceAccount(w http.ResponseWriter, r *http.Request) error {
	// Literal resource/verb pairs, so the permission lints credit both verbs.
	perm := servingHTTPCluster("serviceaccounts", "write")
	if r.Method == http.MethodDelete {
		perm = servingHTTPCluster("serviceaccounts", "delete")
	}
	raw, err := h.iamCommandPreflight(w, r, perm)
	if err != nil {
		return err
	}
	var desired *publicv1.ServiceAccount
	var review api.ObjectReview
	var key store.Key
	switch r.Method {
	case http.MethodPost:
		var in api.ServiceAccountDesired
		if err := decodeReviewedCommandJSON(raw, &in, "invalid ServiceAccount command"); err != nil {
			return err
		}
		if desired, err = serviceAccountDesired(in); err != nil {
			return err
		}
		key = store.KeyOf(desired)
	case http.MethodPut:
		var in api.ReplaceServiceAccountCommand
		if err := decodeReviewedCommandJSON(raw, &in, "invalid ServiceAccount command"); err != nil {
			return err
		}
		review = in.ObjectReview
		if desired, err = serviceAccountDesired(in.ServiceAccount); err != nil {
			return err
		}
		if key, err = serviceAccountPath(r); err != nil {
			return err
		}
		if store.KeyOf(desired) != key {
			return servingHTTPInvalid("ServiceAccount body and path must match")
		}
	case http.MethodDelete:
		if err := decodeReviewedCommandJSON(raw, &review, "invalid ServiceAccount command"); err != nil {
			return err
		}
		if key, err = serviceAccountPath(r); err != nil {
			return err
		}
	default:
		return servingHTTPInvalid("invalid ServiceAccount operation")
	}
	if r.Method != http.MethodPost && !validObjectReview(review) {
		return servingHTTPInvalid("exact reviewed ServiceAccount UID and resourceVersion required")
	}
	var out store.Object
	err = h.iamCommand(r, []rbac.Attributes{iamTarget(perm, key.Namespace, key.Name)}, func(ctx context.Context, tx *sqlite.CommandTx, _ servingauth.Dependencies, _ auth.Principal, observed time.Time) error {
		if r.Method == http.MethodPost {
			var err error
			out, err = security.Apply(ctx, tx, desired, store.ApplyOptions{Manager: store.ManagerAPI, CreateOnly: true}, observed)
			return err
		}
		old, err := reviewedServiceAccount(ctx, tx, key, review)
		if err != nil {
			return err
		}
		if r.Method == http.MethodDelete {
			if err := security.Delete(ctx, tx, old, store.DeleteOptions{Manager: store.ManagerAPI}, observed); err != nil {
				return err
			}
			return deleteOwnedTokens(ctx, tx, serviceAccountOwnsToken(old))
		}
		disabling := desired.Spec.Disabled && !old.Spec.Disabled
		next := old.DeepCopy()
		next.Spec.Description, next.Spec.Disabled = desired.Spec.Description, desired.Spec.Disabled
		next.Labels, next.Annotations = desired.Labels, desired.Annotations
		if out, err = security.Apply(ctx, tx, next, store.ApplyOptions{Manager: store.ManagerAPI, ExpectedResourceVersion: review.ResourceVersion}, observed); err != nil {
			return err
		}
		if disabling {
			return deleteOwnedTokens(ctx, tx, serviceAccountOwnsToken(old))
		}
		return nil
	})
	if err != nil {
		return err
	}
	h.opts.Runtime.publisher.Wake()
	if r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	status := http.StatusOK
	if r.Method == http.MethodPost {
		status = http.StatusCreated
	}
	sa := out.(*publicv1.ServiceAccount).DeepCopy()
	sa.APIVersion, sa.Kind = publicv1.GroupVersion.String(), publicv1.KindServiceAccount
	return servingHTTPJSON(w, status, sa)
}

// createServiceAccountToken issues one token for an exact account. The wire
// secret exists only in this function and its answer: the store keeps its
// hash, and no log, audit row or error carries it.
func (h *servingHTTP) createServiceAccountToken(w http.ResponseWriter, r *http.Request) error {
	raw, err := h.iamCommandPreflight(w, r, servingHTTPCluster("serviceaccounts", "write"), servingHTTPCluster("tokens", "write"))
	if err != nil {
		return err
	}
	key, err := serviceAccountPath(r)
	if err != nil {
		return err
	}
	var req api.CreateServiceAccountTokenRequest
	if err := decodeReviewedCommandJSON(raw, &req, "invalid service account token request"); err != nil {
		return err
	}
	if req.UID == "" || len(req.UID) > 128 || strings.TrimSpace(req.UID) != req.UID || len(req.Description) > 4096 || !utf8.ValidString(req.Description) || len(req.Scopes) > servingauth.MaxGrants || len(req.Scopes) > 64 {
		return servingHTTPInvalid("exact account uid and bounded description and scopes are required")
	}
	for _, wire := range req.Scopes {
		g, err := rbac.ParseGrant(wire)
		if err != nil || !internalv1.ValidGrant(wire) || h.permissions.Roles[g.Role] == nil {
			return servingHTTPInvalid("invalid token scope")
		}
	}
	life := time.Duration(0)
	if req.ExpiresIn != "" {
		if life, err = time.ParseDuration(req.ExpiresIn); err != nil || life <= 0 {
			return servingHTTPInvalid("expiresIn must be a positive Go duration")
		}
	}
	scopes := slices.Clone(req.Scopes)
	slices.Sort(scopes)
	scopes = slices.Compact(scopes)
	attrs := []rbac.Attributes{iamTarget(servingHTTPCluster("serviceaccounts", "write"), key.Namespace, key.Name), servingHTTPCluster("tokens", "write")}
	var created *internalv1.APIToken
	var wire string
	err = h.iamCommand(r, attrs, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies, p auth.Principal, _ time.Time) error {
		var sa publicv1.ServiceAccount
		if err := tx.Get(ctx, key, &sa); err != nil {
			return err
		}
		if string(sa.UID) != req.UID || sa.DeletionTimestamp != nil {
			return store.ErrConflict
		}
		if err := serviceAccountAdministrable(ctx, tx, &sa); err != nil {
			return err
		}
		if sa.Spec.Disabled {
			return &auth.Error{Status: http.StatusConflict, Key: "disabled", Message: "the ServiceAccount is disabled; enable it before issuing a token"}
		}
		var settings publicv1.Kudurru
		if err := tx.Get(ctx, store.Key{Kind: "Kudurru", Name: "default"}, &settings); err != nil {
			return err
		}
		doc, err := servingSettingsDocument(&settings)
		if err != nil {
			return err
		}
		cfg, _, err := config.ApplyClusterDocument(h.opts.Runtime.opts.Config, doc)
		if err != nil {
			return err
		}
		duration := life
		if maxLife := cfg.Auth.Tokens.MaxLifetime.Std(); maxLife > 0 {
			if duration > maxLife {
				return servingHTTPInvalid("expiresIn exceeds auth.tokens.maxLifetime")
			}
			if duration == 0 {
				duration = maxLife
			}
		}
		now := h.now()
		var expires time.Time
		if duration > 0 {
			expires = now.Add(duration).Truncate(time.Second)
		}
		// As in createToken, a token never outlives the API token that
		// issued it. A session caller is bounded by maxLifetime only.
		if p.TokenID != "" {
			var parent internalv1.APIToken
			if err := tx.Get(ctx, store.Key{Kind: "APIToken", Name: p.TokenID}, &parent); err != nil {
				return err
			}
			if !servingHTTPOwnsToken(p, &parent) {
				return servingauth.ErrCredentials
			}
			if parent.Spec.ExpiresAt != nil && (expires.IsZero() || parent.Spec.ExpiresAt.Time.Before(expires)) {
				expires = parent.Spec.ExpiresAt.Time
			}
		}
		if !expires.IsZero() && !expires.After(now) {
			return servingauth.ErrCredentials
		}
		var id string
		wire, id = tokens.Generate()
		t := &internalv1.APIToken{ObjectMeta: metav1.ObjectMeta{Name: id, CreationTimestamp: metav1.NewTime(now)}, Spec: internalv1.APITokenSpec{Owner: serviceAccountOwner(&sa), Description: req.Description, SecretHash: tokens.Hash(wire), Scopes: scopes}}
		if !expires.IsZero() {
			e := metav1.NewTime(expires)
			t.Spec.ExpiresAt = &e
		}
		if err := t.Validate(); err != nil {
			return servingHTTPInvalid("invalid token policy")
		}
		out, err := tx.Apply(ctx, t, store.ApplyOptions{Manager: store.ManagerSystem, CreateOnly: true})
		if err != nil {
			return err
		}
		created = out.(*internalv1.APIToken)
		// Never return a token born revoked: it must authenticate from its
		// persisted form against every current floor of this transaction.
		issued, err := servingauth.AuthenticateCurrentWithClock(ctx, tx, deps, servingauth.Credential{Kind: servingauth.APIToken, Value: wire}, h.now)
		if err != nil {
			return err
		}
		if issued.AccountUID != string(sa.UID) || issued.Kind != auth.KindServiceAccount {
			return servingauth.ErrCredentials
		}
		return tx.RequireCommitCheck(func(ctx context.Context) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if at := h.now(); at.Before(now) || !expires.IsZero() && !at.Before(expires) {
				return servingauth.ErrCredentials
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	h.opts.Runtime.publisher.Wake()
	return servingHTTPJSON(w, http.StatusCreated, api.CreatedToken{TokenInfo: servingHTTPTokenInfo(created), Token: wire})
}

// --- identity providers ---

func identityProviderPath(r *http.Request) (store.Key, error) {
	key := store.Key{Kind: publicv1.KindIdentityProvider, Name: r.PathValue("name")}
	if len(validation.IsDNS1123Label(key.Name)) != 0 {
		return store.Key{}, servingHTTPInvalid("invalid IdentityProvider target")
	}
	return key, nil
}

func (h *servingHTTP) getIdentityProvider(w http.ResponseWriter, r *http.Request) error {
	v, _, err := h.permission(r.Context(), "identityproviders", "read")
	if err != nil {
		return err
	}
	key, err := identityProviderPath(r)
	if err != nil {
		return err
	}
	if r.URL.RawQuery != "" {
		return servingHTTPInvalid("invalid IdentityProvider target")
	}
	objects, err := v.Objects(publicv1.KindIdentityProvider)
	if err != nil {
		return err
	}
	for _, obj := range objects {
		if store.KeyOf(obj) == key {
			out := obj.(*publicv1.IdentityProvider).DeepCopy()
			out.APIVersion, out.Kind = publicv1.GroupVersion.String(), publicv1.KindIdentityProvider
			return servingHTTPJSON(w, http.StatusOK, out)
		}
	}
	return store.ErrNotFound
}

func identityProviderDesired(in api.IdentityProviderDesired) (*publicv1.IdentityProvider, error) {
	if in.APIVersion != publicv1.GroupVersion.String() || in.Kind != publicv1.KindIdentityProvider {
		return nil, servingHTTPInvalid("expected a public v1alpha1 IdentityProvider")
	}
	if _, ok := in.Metadata.Annotations[publicv1.ManagedByAnnotation]; ok {
		return nil, servingHTTPInvalid("IdentityProvider manager is server-owned")
	}
	obj := &publicv1.IdentityProvider{TypeMeta: metav1.TypeMeta{APIVersion: in.APIVersion, Kind: in.Kind}, ObjectMeta: metav1.ObjectMeta{Name: in.Metadata.Name, Labels: in.Metadata.Labels, Annotations: in.Metadata.Annotations}, Spec: *in.Spec.DeepCopy()}
	if err := obj.Validate(); err != nil {
		return nil, servingHTTPInvalid("invalid IdentityProvider fields")
	}
	return obj, nil
}

// configProviderID reports whether the node configuration defines a provider
// with this id. A configuration entry always wins, so an object with its id is
// never created through the API.
func (h *servingHTTP) configProviderID(id string) bool {
	for _, m := range h.opts.Runtime.opts.Config.Auth.IdentityProviders {
		if v, ok := m["id"].(string); ok && v == id {
			return true
		}
	}
	return false
}

func (h *servingHTTP) mutateIdentityProvider(w http.ResponseWriter, r *http.Request) error {
	// Literal resource/verb pairs, so the permission lints credit both verbs.
	perm := servingHTTPCluster("identityproviders", "write")
	if r.Method == http.MethodDelete {
		perm = servingHTTPCluster("identityproviders", "delete")
	}
	raw, err := h.iamCommandPreflight(w, r, perm)
	if err != nil {
		return err
	}
	var desired *publicv1.IdentityProvider
	var review api.ObjectReview
	var key store.Key
	switch r.Method {
	case http.MethodPost:
		var in api.IdentityProviderDesired
		if err := decodeReviewedCommandJSON(raw, &in, "invalid IdentityProvider command"); err != nil {
			return err
		}
		if desired, err = identityProviderDesired(in); err != nil {
			return err
		}
		key = store.KeyOf(desired)
		if h.configProviderID(key.Name) {
			return &auth.Error{Status: http.StatusConflict, Key: "conflict", Message: "the node configuration defines a provider with this id"}
		}
	case http.MethodPut:
		var in api.ReplaceIdentityProviderCommand
		if err := decodeReviewedCommandJSON(raw, &in, "invalid IdentityProvider command"); err != nil {
			return err
		}
		review = in.ObjectReview
		if desired, err = identityProviderDesired(in.IdentityProvider); err != nil {
			return err
		}
		if key, err = identityProviderPath(r); err != nil {
			return err
		}
		if store.KeyOf(desired) != key {
			return servingHTTPInvalid("IdentityProvider body and path must match")
		}
	case http.MethodDelete:
		if err := decodeReviewedCommandJSON(raw, &review, "invalid IdentityProvider command"); err != nil {
			return err
		}
		if key, err = identityProviderPath(r); err != nil {
			return err
		}
	default:
		return servingHTTPInvalid("invalid IdentityProvider operation")
	}
	if r.Method != http.MethodPost && !validObjectReview(review) {
		return servingHTTPInvalid("exact reviewed IdentityProvider UID and resourceVersion required")
	}
	var out store.Object
	err = h.iamCommand(r, []rbac.Attributes{iamTarget(perm, "", key.Name)}, func(ctx context.Context, tx *sqlite.CommandTx, _ servingauth.Dependencies, _ auth.Principal, observed time.Time) error {
		if r.Method == http.MethodPost {
			var err error
			out, err = security.Apply(ctx, tx, desired, store.ApplyOptions{Manager: store.ManagerAPI, CreateOnly: true}, observed)
			return err
		}
		var old publicv1.IdentityProvider
		if err := tx.Get(ctx, key, &old); err != nil {
			return err
		}
		if string(old.UID) != review.UID || old.ResourceVersion != review.ResourceVersion || old.DeletionTimestamp != nil {
			return store.ErrConflict
		}
		if owner := store.ManagedBy(&old); owner != store.ManagerAPI {
			return &store.ErrManagedBy{Key: key, Owner: owner}
		}
		if r.Method == http.MethodDelete {
			return security.Delete(ctx, tx, &old, store.DeleteOptions{Manager: store.ManagerAPI}, observed)
		}
		next := old.DeepCopy()
		next.Spec = desired.Spec
		next.Labels, next.Annotations = desired.Labels, desired.Annotations
		var err error
		out, err = security.Apply(ctx, tx, next, store.ApplyOptions{Manager: store.ManagerAPI, ExpectedResourceVersion: review.ResourceVersion}, observed)
		return err
	})
	if err != nil {
		return err
	}
	h.opts.Runtime.publisher.Wake()
	if r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	status := http.StatusOK
	if r.Method == http.MethodPost {
		status = http.StatusCreated
	}
	idp := out.(*publicv1.IdentityProvider).DeepCopy()
	idp.APIVersion, idp.Kind = publicv1.GroupVersion.String(), publicv1.KindIdentityProvider
	return servingHTTPJSON(w, status, idp)
}
