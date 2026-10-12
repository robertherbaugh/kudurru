package sqlite

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	internalkinds "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

// Internal kinds (LocalUser, APIToken, ServiceAccount, RoleBinding,
// IdentityProvider; api/internalkinds/v1alpha1) have no CRD, so the CRD
// validator does not know them. A kind the validator does not know whose
// objects implement Validate() error is validated by that method instead,
// and a rejection is converted to the *validator.Error every other write
// returns, so the API maps both to 400 "invalid".

type selfValidating interface{ Validate() error }

// selfValidated reports whether obj is validated by its own Validate
// method rather than by the CRD validator.
func (s *Store) selfValidated(obj store.Object) (selfValidating, bool) {
	if _, _, known := s.opts.Validator.GroupVersionKind(store.KindOf(obj)); known {
		return nil, false
	}
	v, ok := obj.(selfValidating)
	return v, ok
}

// validate runs the validator matching obj's kind (see Apply).
func (s *Store) validate(ctx context.Context, obj, old store.Object) error {
	if v, ok := s.selfValidated(obj); ok {
		return internalError(v.Validate())
	}
	return s.opts.Validator.Validate(ctx, obj, old)
}

// validateStatus runs the status validator matching obj's kind (see
// UpdateStatus). Internal kinds validate the whole merged object.
func (s *Store) validateStatus(ctx context.Context, obj, old store.Object) error {
	if v, ok := s.selfValidated(obj); ok {
		merged := old.DeepCopyObject().(store.Object)
		if err := copyStatus(merged, obj); err != nil {
			return err
		}
		if mv, ok := merged.(selfValidating); ok {
			return internalError(mv.Validate())
		}
		return internalError(v.Validate())
	}
	return s.opts.Validator.ValidateStatus(ctx, obj, old)
}

// internalError converts an internal kind's *ValidationError into a
// *validator.Error with one cause per field.
func internalError(err error) error {
	ve, ok := internalkinds.AsValidationError(err)
	if !ok {
		return err
	}
	out := &validator.Error{Group: v1alpha1.GroupVersion.Group, Kind: ve.Kind, Namespace: ve.Namespace, Name: ve.Name}
	for _, f := range ve.Fields {
		out.Causes = append(out.Causes, validator.Cause{Field: f.Field, Type: metav1.CauseTypeFieldValueInvalid, Message: f.Message})
	}
	return out
}
