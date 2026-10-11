/*
Copyright 2026 The kudurru Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// The internal kinds are validated in Go: there is no CRD schema for them.
// Names must be DNS subdomains (RFC 1123) so that the kube driver can embed
// them in Secret names; namespaces are DNS labels. Every rejected field is
// reported with its path so the API can return it as 400 "invalid".

// MaxPasswordHashLen bounds a stored PHC string.
const MaxPasswordHashLen = 512

var (
	tokenIDPattern    = regexp.MustCompile(`^[a-z0-9]{12}$`)
	secretHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	grantPattern      = regexp.MustCompile(`^[a-z0-9-]+(@[a-z]+:[^@:\s]+)?$`)
)

// FieldError is one rejected field: Field is the JSON path
// ("spec.subjects[0].name"), Message the reason without the path.
type FieldError struct {
	Field   string
	Message string
}

func (f FieldError) String() string { return f.Field + ": " + f.Message }

// ValidationError is returned by Validate. Store drivers convert it into
// their *validator.Error (one cause per field) so the API maps it to 400
// "invalid" exactly like a CRD schema rejection.
//
// +kubebuilder:object:generate=false
type ValidationError struct {
	Kind, Namespace, Name string
	Fields                []FieldError
}

func (e *ValidationError) Error() string {
	msgs := make([]string, 0, len(e.Fields))
	for _, f := range e.Fields {
		msgs = append(msgs, f.String())
	}
	return fmt.Sprintf("%s %q is invalid: %s", e.Kind, e.Name, strings.Join(msgs, "; "))
}

// FieldCauses returns one metav1.StatusCause per rejected field. Store
// drivers match it structurally to build their *validator.Error.
func (e *ValidationError) FieldCauses() []metav1.StatusCause {
	out := make([]metav1.StatusCause, 0, len(e.Fields))
	for _, f := range e.Fields {
		out = append(out, metav1.StatusCause{Type: metav1.CauseTypeFieldValueInvalid, Field: f.Field, Message: f.Message})
	}
	return out
}

// AsValidationError returns the *ValidationError in err's chain.
func AsValidationError(err error) (*ValidationError, bool) {
	var v *ValidationError
	ok := errors.As(err, &v)
	return v, ok
}

type fieldErrs []FieldError

func (f *fieldErrs) add(field, format string, args ...any) {
	*f = append(*f, FieldError{Field: field, Message: fmt.Sprintf(format, args...)})
}

func (f fieldErrs) err(kind, namespace, name string) error {
	if len(f) == 0 {
		return nil
	}
	return &ValidationError{Kind: kind, Namespace: namespace, Name: name, Fields: f}
}

func validateMeta(kind, namespace, name string) fieldErrs {
	var errs fieldErrs
	if name == "" {
		errs.add("metadata.name", "required")
	} else if msgs := validation.IsDNS1123Subdomain(name); len(msgs) > 0 {
		errs.add("metadata.name", "%q: %s", name, strings.Join(msgs, "; "))
	}
	if Namespaced(kind) {
		if namespace == "" {
			errs.add("metadata.namespace", "required")
		} else if msgs := validation.IsDNS1123Label(namespace); len(msgs) > 0 {
			errs.add("metadata.namespace", "%q: %s", namespace, strings.Join(msgs, "; "))
		}
	} else if namespace != "" {
		errs.add("metadata.namespace", "%s is cluster-scoped", kind)
	}
	return errs
}

// ValidGrant reports whether g is a grant in wire form: "role" or
// "role@kind:name".
func ValidGrant(g string) bool { return grantPattern.MatchString(g) }

// Validate implements Validator.
func (u *LocalUser) Validate() error {
	errs := validateMeta(KindLocalUser, u.Namespace, u.Name)
	if h := u.Spec.PasswordHash; h != "" && (len(h) > MaxPasswordHashLen || !strings.HasPrefix(h, "$argon2id$")) {
		errs.add("spec.passwordHash", "must be an Argon2id PHC string")
	}
	for i, g := range u.Spec.Groups {
		if g == "" {
			errs.add(fmt.Sprintf("spec.groups[%d]", i), "empty group name")
		}
	}
	return errs.err(KindLocalUser, u.Namespace, u.Name)
}

// Validate implements Validator.
func (t *APIToken) Validate() error {
	errs := validateMeta(KindAPIToken, t.Namespace, t.Name)
	if t.Name != "" && !tokenIDPattern.MatchString(t.Name) {
		errs.add("metadata.name", "a token id is 12 characters of [a-z0-9]")
	}
	if !secretHashPattern.MatchString(t.Spec.SecretHash) {
		errs.add("spec.secretHash", "must be a lowercase hex sha256")
	}
	switch t.Spec.Owner.Kind {
	case OwnerUser:
		if t.Spec.Owner.Name == "" {
			errs.add("spec.owner.name", "required")
		}
	case OwnerServiceAccount:
		if t.Spec.Owner.Name == "" || t.Spec.Owner.Namespace == "" {
			errs.add("spec.owner", "a ServiceAccount owner needs name and namespace")
		}
	default:
		errs.add("spec.owner.kind", "%q is not User or ServiceAccount", t.Spec.Owner.Kind)
	}
	for i, g := range t.Spec.Grants {
		if !ValidGrant(g) {
			errs.add(fmt.Sprintf("spec.grants[%d]", i), "%q is not role or role@kind:name", g)
		}
	}
	for i, g := range t.Spec.Scopes {
		if !ValidGrant(g) {
			errs.add(fmt.Sprintf("spec.scopes[%d]", i), "%q is not role or role@kind:name", g)
		}
	}
	return errs.err(KindAPIToken, t.Namespace, t.Name)
}

// Validate implements Validator.
func (a *ServiceAccount) Validate() error {
	return validateMeta(KindServiceAccount, a.Namespace, a.Name).err(KindServiceAccount, a.Namespace, a.Name)
}

// Validate implements Validator. Role names are checked against the
// permissions table by internal/rbac when the binding is loaded.
func (b *RoleBinding) Validate() error {
	errs := validateMeta(KindRoleBinding, b.Namespace, b.Name)
	if b.Spec.Role == "" {
		errs.add("spec.role", "required")
	}
	switch b.Spec.Scope.Kind {
	case "", "cluster":
		if b.Spec.Scope.Name != "" {
			errs.add("spec.scope.name", "must be empty for cluster scope")
		}
	case "zone", "zonegroup", "clientgroup":
		if b.Spec.Scope.Name == "" {
			errs.add("spec.scope.name", "required for scope kind %s", b.Spec.Scope.Kind)
		}
	default:
		errs.add("spec.scope.kind", "%q is not cluster, zone, zonegroup or clientgroup", b.Spec.Scope.Kind)
	}
	if len(b.Spec.Subjects) == 0 {
		errs.add("spec.subjects", "at least one subject is required")
	}
	for i, s := range b.Spec.Subjects {
		if s.Name == "" {
			errs.add(fmt.Sprintf("spec.subjects[%d].name", i), "required")
		}
		switch s.Kind {
		case "User", "Group":
		case "ServiceAccount":
			if s.Namespace == "" {
				errs.add(fmt.Sprintf("spec.subjects[%d].namespace", i), "required for a ServiceAccount")
			}
		default:
			errs.add(fmt.Sprintf("spec.subjects[%d].kind", i), "%q is not User, Group or ServiceAccount", s.Kind)
		}
	}
	return errs.err(KindRoleBinding, b.Namespace, b.Name)
}

// Validate implements Validator. The OIDC-specific rules (https issuer,
// redirect path, provisioning) are checked by internal/auth/oidc when the
// provider is built, so a broken provider is reported, not stored silently.
func (p *IdentityProvider) Validate() error {
	errs := validateMeta(KindIdentityProvider, p.Namespace, p.Name)
	if msgs := validation.IsDNS1123Label(p.Name); p.Name != "" && len(msgs) > 0 {
		errs.add("metadata.name", "an identity provider id is a DNS label: %s", strings.Join(msgs, "; "))
	}
	if p.Spec.Issuer == "" {
		errs.add("spec.issuer", "required")
	}
	if p.Spec.ClientID == "" {
		errs.add("spec.clientID", "required")
	}
	if p.Spec.ClientSecretRef != nil && p.Spec.ClientSecretFile != "" {
		errs.add("spec", "clientSecretRef and clientSecretFile are mutually exclusive")
	}
	return errs.err(KindIdentityProvider, p.Namespace, p.Name)
}
