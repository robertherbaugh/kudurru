/*
Copyright 2026 Robert Herbaugh.

SPDX-License-Identifier: LicenseRef-Kudurru-Community
See the LICENSE file in the repository root.
*/

package v1alpha1

import (
	"fmt"
	"regexp"
	"strings"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
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

// FieldError aliases the common identity field error.
type FieldError = public.FieldError

// ValidationError aliases the common identity validation error.
type ValidationError = public.ValidationError

// AsValidationError extracts a common identity validation error.
func AsValidationError(err error) (*ValidationError, bool) { return public.AsValidationError(err) }

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
