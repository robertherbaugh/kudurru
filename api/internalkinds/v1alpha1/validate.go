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

	"k8s.io/apimachinery/pkg/util/validation"
)

// The internal kinds are validated in Go: there is no CRD schema for them.
// Names must be DNS subdomains (RFC 1123) so that the kube driver can embed
// them in Secret names; namespaces are DNS labels. Messages carry the field
// path so the API can return them as 400 "invalid".

// MaxPasswordHashLen bounds a stored PHC string.
const MaxPasswordHashLen = 512

var (
	tokenIDPattern    = regexp.MustCompile(`^[a-z0-9]{12}$`)
	secretHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func validateMeta(kind, namespace, name string) []error {
	var errs []error
	if name == "" {
		errs = append(errs, errors.New("metadata.name: required"))
	} else if msgs := validation.IsDNS1123Subdomain(name); len(msgs) > 0 {
		errs = append(errs, fmt.Errorf("metadata.name: %q: %s", name, strings.Join(msgs, "; ")))
	}
	if Namespaced(kind) {
		if namespace == "" {
			errs = append(errs, errors.New("metadata.namespace: required"))
		} else if msgs := validation.IsDNS1123Label(namespace); len(msgs) > 0 {
			errs = append(errs, fmt.Errorf("metadata.namespace: %q: %s", namespace, strings.Join(msgs, "; ")))
		}
	} else if namespace != "" {
		errs = append(errs, fmt.Errorf("metadata.namespace: %s is cluster-scoped", kind))
	}
	return errs
}

func joinErrs(kind string, errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%s: %w", kind, errors.Join(errs...))
}

// Validate implements Validator.
func (u *LocalUser) Validate() error {
	errs := validateMeta(KindLocalUser, u.Namespace, u.Name)
	if h := u.Spec.PasswordHash; h != "" && (len(h) > MaxPasswordHashLen || !strings.HasPrefix(h, "$argon2id$")) {
		errs = append(errs, errors.New("spec.passwordHash: must be an Argon2id PHC string"))
	}
	for i, g := range u.Spec.Groups {
		if g == "" {
			errs = append(errs, fmt.Errorf("spec.groups[%d]: empty group name", i))
		}
	}
	return joinErrs(KindLocalUser, errs)
}

// Validate implements Validator.
func (t *APIToken) Validate() error {
	errs := validateMeta(KindAPIToken, t.Namespace, t.Name)
	if t.Name != "" && !tokenIDPattern.MatchString(t.Name) {
		errs = append(errs, errors.New("metadata.name: a token id is 12 characters of [a-z0-9]"))
	}
	if !secretHashPattern.MatchString(t.Spec.SecretHash) {
		errs = append(errs, errors.New("spec.secretHash: must be 64 lowercase hex digits (sha256)"))
	}
	switch t.Spec.Owner.Kind {
	case OwnerUser:
		if t.Spec.Owner.Name == "" {
			errs = append(errs, errors.New("spec.owner.name: required"))
		}
	case OwnerServiceAccount:
		if t.Spec.Owner.Name == "" || t.Spec.Owner.Namespace == "" {
			errs = append(errs, errors.New("spec.owner: a ServiceAccount owner needs name and namespace"))
		}
	default:
		errs = append(errs, fmt.Errorf("spec.owner.kind: %q is not User or ServiceAccount", t.Spec.Owner.Kind))
	}
	return joinErrs(KindAPIToken, errs)
}

// Validate implements Validator.
func (a *ServiceAccount) Validate() error {
	return joinErrs(KindServiceAccount, validateMeta(KindServiceAccount, a.Namespace, a.Name))
}

// Validate implements Validator. Role names are checked against the
// permissions table by internal/rbac when the binding is loaded.
func (b *RoleBinding) Validate() error {
	errs := validateMeta(KindRoleBinding, b.Namespace, b.Name)
	if b.Spec.Role == "" {
		errs = append(errs, errors.New("spec.role: required"))
	}
	switch b.Spec.Scope.Kind {
	case "", "cluster":
		if b.Spec.Scope.Name != "" {
			errs = append(errs, errors.New("spec.scope.name: must be empty for cluster scope"))
		}
	case "zone", "zonegroup", "clientgroup":
		if b.Spec.Scope.Name == "" {
			errs = append(errs, fmt.Errorf("spec.scope.name: required for scope kind %s", b.Spec.Scope.Kind))
		}
	default:
		errs = append(errs, fmt.Errorf("spec.scope.kind: %q is not cluster, zone, zonegroup or clientgroup", b.Spec.Scope.Kind))
	}
	if len(b.Spec.Subjects) == 0 {
		errs = append(errs, errors.New("spec.subjects: at least one subject is required"))
	}
	for i, s := range b.Spec.Subjects {
		if s.Name == "" {
			errs = append(errs, fmt.Errorf("spec.subjects[%d].name: required", i))
		}
		switch s.Kind {
		case "User", "Group":
		case "ServiceAccount":
			if s.Namespace == "" {
				errs = append(errs, fmt.Errorf("spec.subjects[%d].namespace: required for a ServiceAccount", i))
			}
		default:
			errs = append(errs, fmt.Errorf("spec.subjects[%d].kind: %q is not User, Group or ServiceAccount", i, s.Kind))
		}
	}
	return joinErrs(KindRoleBinding, errs)
}

// Validate implements Validator. The OIDC-specific rules (https issuer,
// redirect path, provisioning) are checked by internal/auth/oidc when the
// provider is built, so a broken provider is reported, not stored silently.
func (p *IdentityProvider) Validate() error {
	errs := validateMeta(KindIdentityProvider, p.Namespace, p.Name)
	if msgs := validation.IsDNS1123Label(p.Name); p.Name != "" && len(msgs) > 0 {
		errs = append(errs, fmt.Errorf("metadata.name: an identity provider id is a DNS label: %s", strings.Join(msgs, "; ")))
	}
	if p.Spec.Issuer == "" {
		errs = append(errs, errors.New("spec.issuer: required"))
	}
	if p.Spec.ClientID == "" {
		errs = append(errs, errors.New("spec.clientID: required"))
	}
	if p.Spec.ClientSecretRef != nil && p.Spec.ClientSecretFile != "" {
		errs = append(errs, errors.New("spec: clientSecretRef and clientSecretFile are mutually exclusive"))
	}
	return joinErrs(KindIdentityProvider, errs)
}
