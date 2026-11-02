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

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

func (s *servingRuntime) RoleBindingCommandsAvailable() bool { return s != nil }

func (h *servingHTTP) registerRoleBindings() {
	path := "/iam/rolebindings/{namespace}/{name}"
	h.route(http.MethodGet, path, h.getRoleBinding)
	h.route(http.MethodPost, "/iam/rolebindings", func(w http.ResponseWriter, r *http.Request) error { return roleBindingError(h.mutateRoleBinding(w, r)) })
	h.route(http.MethodPut, path, func(w http.ResponseWriter, r *http.Request) error { return roleBindingError(h.mutateRoleBinding(w, r)) })
	h.route(http.MethodDelete, path, func(w http.ResponseWriter, r *http.Request) error { return roleBindingError(h.mutateRoleBinding(w, r)) })
}

func roleBindingPath(r *http.Request) (store.Key, error) {
	key := store.Key{Kind: publicv1.KindRoleBinding, Namespace: r.PathValue("namespace"), Name: r.PathValue("name")}
	if r.URL.RawQuery != "" || len(validation.IsDNS1123Label(key.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(key.Name)) != 0 {
		return store.Key{}, servingHTTPInvalid("invalid RoleBinding target")
	}
	return key, nil
}

func (h *servingHTTP) getRoleBinding(w http.ResponseWriter, r *http.Request) error {
	v, _, err := h.permission(r.Context(), "rolebindings", "read")
	if err != nil {
		return err
	}
	key, err := roleBindingPath(r)
	if err != nil {
		return err
	}
	objects, err := v.Objects(publicv1.KindRoleBinding)
	if err != nil {
		return err
	}
	for _, obj := range objects {
		if store.KeyOf(obj) == key {
			if _, _, err := h.permission(r.Context(), "rolebindings", "read"); err != nil {
				return err
			}
			return servingHTTPJSON(w, http.StatusOK, obj)
		}
	}
	return store.ErrNotFound
}

// decodeRoleBindingJSON rejects duplicate keys at every nesting level before
// strict typed decoding. Review fields cannot be overridden by a second key.
func decodeRoleBindingJSON(raw []byte, out any) error {
	invalid := func() error { return servingHTTPInvalid("invalid RoleBinding command") }
	if len(raw) > 64<<10 || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		return invalid()
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	var value func(int) error
	value = func(depth int) error {
		if depth > 32 {
			return invalid()
		}
		tok, err := d.Token()
		if err != nil {
			return invalid()
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, err := d.Token()
				key, ok := k.(string)
				if err != nil || !ok || seen[key] {
					return invalid()
				}
				seen[key] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
			if end, err := d.Token(); err != nil || end != json.Delim('}') {
				return invalid()
			}
		case '[':
			for d.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
			if end, err := d.Token(); err != nil || end != json.Delim(']') {
				return invalid()
			}
		default:
			return invalid()
		}
		return nil
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return invalid()
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return invalid()
	}
	return nil
}

func roleBindingDesired(in api.RoleBindingDesired) (*publicv1.RoleBinding, error) {
	if in.APIVersion != publicv1.GroupVersion.String() || in.Kind != publicv1.KindRoleBinding {
		return nil, servingHTTPInvalid("expected a public v1alpha1 RoleBinding")
	}
	// Manager metadata is server-owned, even if a caller supplies the same value.
	if _, ok := in.Metadata.Annotations[publicv1.ManagedByAnnotation]; ok {
		return nil, servingHTTPInvalid("RoleBinding manager is server-owned")
	}
	obj := &publicv1.RoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: in.APIVersion, Kind: in.Kind}, ObjectMeta: metav1.ObjectMeta{Name: in.Metadata.Name, Namespace: in.Metadata.Namespace, Labels: in.Metadata.Labels, Annotations: in.Metadata.Annotations}, Spec: publicv1.RoleBindingSpec{Role: in.Spec.Role, Scope: publicv1.BindingScope{Kind: in.Spec.Scope.Kind, Name: in.Spec.Scope.Name}}}
	for _, s := range in.Spec.Subjects {
		obj.Spec.Subjects = append(obj.Spec.Subjects, publicv1.BindingSubject{Kind: s.Kind, Name: s.Name, Namespace: s.Namespace})
	}
	if err := obj.Validate(); err != nil {
		return nil, servingHTTPInvalid("invalid RoleBinding fields")
	}
	perms, err := rbac.LoadPermissions()
	if err != nil {
		return nil, err
	}
	if _, err := rbac.NewBindingAuthorizer(perms, []rbac.Binding{rbac.FromRoleBinding(obj)}); err != nil {
		return nil, servingHTTPInvalid("invalid RoleBinding role or scope")
	}
	return obj, nil
}

func validRoleBindingReview(q api.RoleBindingReview) bool {
	return q.UID != "" && len(q.UID) <= 128 && q.ResourceVersion != "" && len(q.ResourceVersion) <= 128 && strings.TrimSpace(q.UID) == q.UID && strings.TrimSpace(q.ResourceVersion) == q.ResourceVersion
}

func (h *servingHTTP) mutateRoleBinding(w http.ResponseWriter, r *http.Request) error {
	verb := "write"
	if r.Method == http.MethodDelete {
		verb = "delete"
	}
	v, _, err := h.permission(r.Context(), "rolebindings", verb)
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
	if r.URL.RawQuery != "" {
		return servingHTTPInvalid("RoleBinding commands do not accept query parameters")
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		return servingHTTPInvalid("invalid RoleBinding command")
	}
	var desired *publicv1.RoleBinding
	var review api.RoleBindingReview
	var key store.Key
	switch r.Method {
	case http.MethodPost:
		var in api.RoleBindingDesired
		if err := decodeRoleBindingJSON(raw, &in); err != nil {
			return err
		}
		desired, err = roleBindingDesired(in)
		if err != nil {
			return err
		}
		key = store.KeyOf(desired)
	case http.MethodPut:
		var in api.ReplaceRoleBindingCommand
		if err := decodeRoleBindingJSON(raw, &in); err != nil {
			return err
		}
		review = in.RoleBindingReview
		desired, err = roleBindingDesired(in.Binding)
		if err != nil {
			return err
		}
		key, err = roleBindingPath(r)
		if err != nil {
			return err
		}
		if store.KeyOf(desired) != key {
			return servingHTTPInvalid("RoleBinding body and path must match")
		}
	case http.MethodDelete:
		if err := decodeRoleBindingJSON(raw, &review); err != nil {
			return err
		}
		key, err = roleBindingPath(r)
		if err != nil {
			return err
		}
	default:
		return servingHTTPInvalid("invalid RoleBinding operation")
	}
	if r.Method != http.MethodPost && !validRoleBindingReview(review) {
		return servingHTTPInvalid("exact reviewed RoleBinding UID and resourceVersion required")
	}
	var out store.Object
	err = h.roleBindingCommand(r, key, verb, func(ctx context.Context, tx *sqlite.CommandTx, observed time.Time) error {
		var err error
		out, err = stageRoleBindingMutation(ctx, tx, r.Method, key, desired, review, observed)
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
	return servingHTTPJSON(w, status, out)
}

// roleBindingCommand retains the original proof and its installed final validity
// checks. Intentional self-revocation is not reopened against staged state.
func (h *servingHTTP) roleBindingCommand(r *http.Request, key store.Key, verb string, fn func(context.Context, *sqlite.CommandTx, time.Time) error) error {
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
		attrs := servingHTTPCluster("rolebindings", verb)
		attrs.Namespace = key.Namespace
		attrs.Name = key.Name
		if _, _, err := servingauth.ReauthorizeCurrentWithClock(ctx, tx, deps, proof, attrs, clock); err != nil {
			return err
		}
		return fn(ctx, tx, observed)
	})
}

func stageRoleBindingMutation(ctx context.Context, tx *sqlite.CommandTx, method string, key store.Key, desired *publicv1.RoleBinding, review api.RoleBindingReview, observed time.Time) (store.Object, error) {
	if method != http.MethodPost {
		var old publicv1.RoleBinding
		if err := tx.Get(ctx, key, &old); err != nil {
			return nil, err
		}
		if string(old.UID) != review.UID || old.ResourceVersion != review.ResourceVersion || old.DeletionTimestamp != nil {
			return nil, store.ErrConflict
		}
		if owner := store.ManagedBy(&old); owner != store.ManagerAPI {
			return nil, &store.ErrManagedBy{Key: key, Owner: owner}
		}
		if method == http.MethodDelete {
			return nil, security.Delete(ctx, tx, &old, store.DeleteOptions{Manager: store.ManagerAPI}, observed)
		}
		// Retain server lifecycle/identity metadata from the exact reviewed row.
		old.Spec = desired.Spec
		old.Labels = desired.Labels
		old.Annotations = desired.Annotations
		desired = &old
	}
	return security.Apply(ctx, tx, desired, store.ApplyOptions{Manager: store.ManagerAPI, CreateOnly: method == http.MethodPost, ExpectedResourceVersion: review.ResourceVersion}, observed)
}

func roleBindingError(err error) error {
	if errors.Is(err, store.ErrConflict) {
		return &auth.Error{Status: http.StatusConflict, Key: "conflict", Message: "RoleBinding changed or already exists; review before making a new request"}
	}
	var managed *store.ErrManagedBy
	if errors.As(err, &managed) {
		return &auth.Error{Status: http.StatusConflict, Key: "managed_by", Message: "RoleBinding belongs to another manager"}
	}
	var invalid *validator.Error
	if errors.As(err, &invalid) {
		return servingHTTPInvalid("invalid RoleBinding fields")
	}
	return err
}
