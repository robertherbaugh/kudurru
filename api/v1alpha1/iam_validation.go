package v1alpha1

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// FieldError is one rejected field: Field is the JSON path
// ("spec.subjects[0].name"), Message the reason without the path.
//
// +kubebuilder:object:generate=false
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
	if kind == KindServiceAccount || kind == KindRoleBinding {
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
// redirect path, provisioning) are checked by ee/oidc when the
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
	if r := p.Spec.ClientSecretRef; r == nil {
		errs.add("spec.clientSecretRef", "required")
	} else {
		count := 0
		for _, present := range []bool{r.SecretRef != nil, r.File != "", r.Env != ""} {
			if present {
				count++
			}
		}
		if count != 1 {
			errs.add("spec.clientSecretRef", "exactly one credential reference is required")
		}
		if r.SecretRef != nil && (len(validation.IsDNS1123Subdomain(r.SecretRef.Name)) != 0 || r.SecretRef.Key == "" || len(validation.IsConfigMapKey(r.SecretRef.Key)) != 0) {
			errs.add("spec.clientSecretRef.secretRef", "valid Secret name and key are required")
		}
		if r.Env != "" && !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(r.Env) {
			errs.add("spec.clientSecretRef.env", "invalid environment variable name")
		}
	}
	return errs.err(KindIdentityProvider, p.Namespace, p.Name)
}
