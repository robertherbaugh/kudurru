package validator

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// ErrorKey is the stable key of a validation failure in the API error
// envelope {error:{key,message,hint}}.
const ErrorKey = "invalid"

// CauseUnknownField is the cause type for a field that the CRD schema does
// not declare. The apiserver reports these as "strict decoding error: unknown
// field" when kubectl's default --validate=strict is in effect; typed Go
// objects can never carry one.
const CauseUnknownField metav1.CauseType = "FieldValueUnknown"

// ErrUnknownKind is returned (wrapped) for objects whose kind has no CRD.
var ErrUnknownKind = errors.New("validator: unknown kind")

// Cause is one field-level reason an object was rejected. Field is the JSON
// path ("spec.sources[0]"), Type is the Kubernetes cause type
// ("FieldValueInvalid", "FieldValueRequired", "FieldValueNotSupported", ...)
// and Message is the human text without the field prefix, exactly as the
// apiserver puts it in Status.details.causes. Detail and Value are only set
// by the local validator (the apiserver does not transmit them).
type Cause struct {
	Field   string           `json:"field"`
	Type    metav1.CauseType `json:"reason"`
	Message string           `json:"message"`
	Detail  string           `json:"detail,omitempty"`
	Value   any              `json:"-"`
}

// String renders the cause as the apiserver does: "<field>: <message>".
func (c Cause) String() string {
	if c.Field == "" {
		return c.Message
	}
	return c.Field + ": " + c.Message
}

// Error is the validation failure returned by Validator. Both store drivers
// return it: the sqlite driver from the local validator, the kube driver by
// converting the apiserver's 422 with FromStatusError, so the API maps one
// type to 400 {error:{key:"invalid",message,hint}} in both modes.
type Error struct {
	Group     string
	Kind      string
	Namespace string
	Name      string
	Causes    []Cause

	// msg is the apiserver-formatted message when known.
	msg string
}

func newError(gk schema.GroupKind, namespace, name string, errs field.ErrorList) *Error {
	e := &Error{Group: gk.Group, Kind: gk.Kind, Namespace: namespace, Name: name}
	// apierrors.NewInvalid is what the apiserver uses to build its 422, so
	// the message is byte-identical to the one the kube driver sees.
	e.msg = apierrors.NewInvalid(gk, name, errs).Error()
	for _, fe := range errs {
		e.Causes = append(e.Causes, Cause{
			Field:   fe.Field,
			Type:    metav1.CauseType(fe.Type),
			Message: fe.ErrorBody(),
			Detail:  fe.Detail,
			Value:   fe.BadValue,
		})
	}
	return e
}

// newUnknownFieldsError reports fields the schema does not declare, the
// apiserver's "strict decoding error" under fieldValidation=Strict.
func newUnknownFieldsError(gk schema.GroupKind, namespace, name string, paths []string) *Error {
	e := &Error{Group: gk.Group, Kind: gk.Kind, Namespace: namespace, Name: name}
	msgs := make([]string, 0, len(paths))
	for _, p := range paths {
		msgs = append(msgs, fmt.Sprintf("unknown field %q", p))
		e.Causes = append(e.Causes, Cause{Field: p, Type: CauseUnknownField, Message: "field not declared in schema"})
	}
	e.msg = fmt.Sprintf("%s %q is invalid: strict decoding error: %s", gk.String(), name, strings.Join(msgs, ", "))
	return e
}

// Error renders the message the apiserver returns for the same rejection:
// `DNSZone.kudurru.io "home" is invalid: spec.zoneName: Invalid value: ...`.
func (e *Error) Error() string {
	if e.msg != "" {
		return e.msg
	}
	var b strings.Builder
	b.WriteString(e.qualifiedKind())
	fmt.Fprintf(&b, " %q is invalid", e.Name)
	switch len(e.Causes) {
	case 0:
	case 1:
		b.WriteString(": ")
		b.WriteString(e.Causes[0].String())
	default:
		b.WriteString(": [")
		for i, c := range e.Causes {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(c.String())
		}
		b.WriteString("]")
	}
	return b.String()
}

func (e *Error) qualifiedKind() string {
	if e.Group == "" {
		return e.Kind
	}
	return e.Kind + "." + e.Group
}

// Key is the API error key (ErrorKey).
func (e *Error) Key() string { return ErrorKey }

// HTTPStatus is the status code the kudurru API uses for validation
// failures (400; the apiserver itself answers 422).
func (e *Error) HTTPStatus() int { return http.StatusBadRequest }

// Message is the envelope message; identical to Error().
func (e *Error) Message() string { return e.Error() }

// Hint names the offending fields so a UI or CLI user knows where to look.
func (e *Error) Hint() string {
	fields := e.Fields()
	if len(fields) == 0 {
		return "the object does not match the " + e.Kind + " schema"
	}
	return "fix the listed fields and retry: " + strings.Join(fields, ", ")
}

// Fields returns the JSON paths of the causes, in order and without
// duplicates. Object-level causes (no path, or the apiserver's "<nil>" for
// "some validation rules were not checked") are left out.
func (e *Error) Fields() []string {
	out := []string{}
	seen := map[string]bool{}
	for _, c := range e.Causes {
		if c.Field == "" || c.Field == "<nil>" || seen[c.Field] {
			continue
		}
		seen[c.Field] = true
		out = append(out, c.Field)
	}
	return out
}

// HasCause reports whether any cause is at field with the given type. An
// empty typ matches any type.
func (e *Error) HasCause(fieldPath string, typ metav1.CauseType) bool {
	for _, c := range e.Causes {
		if c.Field == fieldPath && (typ == "" || c.Type == typ) {
			return true
		}
	}
	return false
}

// Status returns the Kubernetes Status the apiserver would send for the same
// rejection (reason Invalid, code 422, one cause per field error).
func (e *Error) Status() metav1.Status {
	causes := make([]metav1.StatusCause, 0, len(e.Causes))
	for _, c := range e.Causes {
		causes = append(causes, metav1.StatusCause{Type: c.Type, Message: c.Message, Field: c.Field})
	}
	return metav1.Status{
		TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status:   metav1.StatusFailure,
		Code:     http.StatusUnprocessableEntity,
		Reason:   metav1.StatusReasonInvalid,
		Message:  e.Error(),
		Details: &metav1.StatusDetails{
			Group:  e.Group,
			Kind:   e.Kind,
			Name:   e.Name,
			Causes: causes,
		},
	}
}

// AsError unwraps err to a *Error.
func AsError(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// IsInvalid reports whether err is a validation failure, either from the
// local validator or from the apiserver (StatusReasonInvalid).
func IsInvalid(err error) bool {
	if _, ok := AsError(err); ok {
		return true
	}
	return apierrors.IsInvalid(err)
}

// FromStatusError converts an apiserver "Invalid" error (HTTP 422) into an
// *Error, so the kube driver surfaces exactly the type the sqlite driver
// returns. It returns false for any other error.
func FromStatusError(err error) (*Error, bool) {
	var se apierrors.APIStatus
	if !errors.As(err, &se) {
		return nil, false
	}
	st := se.Status()
	if st.Reason != metav1.StatusReasonInvalid || st.Details == nil {
		return nil, false
	}
	e := &Error{Group: st.Details.Group, Kind: st.Details.Kind, Name: st.Details.Name, msg: st.Message}
	for _, c := range st.Details.Causes {
		e.Causes = append(e.Causes, Cause{Field: c.Field, Type: c.Type, Message: c.Message})
	}
	return e, true
}
