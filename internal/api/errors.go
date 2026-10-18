package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/danielgtaylor/huma/v2"

	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/central"
	"github.com/robertherbaugh/kudurru/internal/objects"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

// Error keys of the envelope {error:{key,message,hint}, took_ms}
// (dev-docs/design-record.md §10).
const (
	KeyInvalid         = "invalid"
	KeyUnauthenticated = "unauthenticated"
	KeyForbidden       = "forbidden"
	KeyCSRF            = "csrf"
	KeyNotFound        = "not_found"
	KeyConflict        = "conflict"
	KeyManagedBy       = "managed_by"
	// KeyDefaultZone is the 409 of deleting the root zone "." (D148).
	KeyDefaultZone = "default_zone"
	// KeyZoneNotEmpty is the 409 of deleting a DNSZone that DNSRecordSets
	// still reference through the object routes (D148).
	KeyZoneNotEmpty   = "zone_not_empty"
	KeyPrecondition   = "precondition_failed"
	KeyUnprocessable  = "unprocessable"
	KeyTooLarge       = "too_large"
	KeyRateLimited    = "rate_limited"
	KeyReadOnly       = "read_only"
	KeyHostOnly       = "host_only"
	KeyEnvLocked      = "env_locked"
	KeyNotImplemented = "not_implemented"
	KeyUnavailable    = "unavailable"
	KeyInternal       = "internal"
	KeyNotAcceptable  = "not_acceptable"
	KeyMethod         = "method_not_allowed"
	// KeyPasswordChange is the 403 of a local user who must change the
	// password first (auth.Authorize answers the same key).
	KeyPasswordChange = "password_change_required"
)

// Missing names the permission a 403 lacked, so the UI can explain it.
type Missing struct {
	Resource string `json:"resource"`
	Verb     string `json:"verb"`
	// Scope is "cluster" or "<kind>:<name>" (zone:home-example-com).
	Scope string `json:"scope"`
}

// ErrorBody is the inner object of the error envelope.
type ErrorBody struct {
	// Key is the stable machine-readable error key.
	Key string `json:"key"`
	// Message is the human-readable explanation; for validation errors it
	// names the field path.
	Message string `json:"message"`
	// Hint tells the caller how to fix the request. It is a string (the
	// owner for managed_by, e.g. "file:objects.d/zones.yaml"; the
	// offending fields for invalid) except for 403 forbidden, where it is
	// {"missing":{resource,verb,scope}} so the UI can explain the denial.
	Hint any `json:"hint,omitempty"`
	// Missing repeats the permission a 403 lacked.
	Missing *Missing `json:"missing,omitempty"`
	// Fields are the JSON paths of a validation failure.
	Fields []string `json:"fields,omitempty"`
	// Objects lists every object a refusal is about when there are several:
	// the managed record sets of a zone delete (managed_by), the record sets
	// that keep a zone from being deleted alone (zone_not_empty).
	Objects []ObjectOwner `json:"objects,omitempty"`
}

// ObjectOwner names one object and its manager in ErrorBody.Objects.
type ObjectOwner struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	// ManagedBy is the object's single writer ("" when none is recorded).
	ManagedBy string `json:"managedBy,omitempty"`
}

// Error is every non-2xx response body of the API. It implements
// huma.StatusError, so handlers return it as an error.
type Error struct {
	Status int       `json:"-"`
	Body   ErrorBody `json:"error"`
	// TookMS is the server-side duration of the request.
	TookMS float64 `json:"took_ms"`

	headers http.Header
}

// Error implements error.
func (e *Error) Error() string { return e.Body.Key + ": " + e.Body.Message }

// GetStatus implements huma.StatusError.
func (e *Error) GetStatus() int { return e.Status }

// GetHeaders implements huma.HeadersError (Retry-After, WWW-Authenticate).
func (e *Error) GetHeaders() http.Header {
	if e.headers == nil {
		e.headers = http.Header{}
	}
	return e.headers
}

func (e *Error) setTook(ms float64) { e.TookMS = ms }

// newError builds an Error.
func newError(status int, key, message, hint string) *Error {
	e := &Error{Status: status, Body: ErrorBody{Key: key, Message: message}}
	if hint != "" {
		e.Body.Hint = hint
	}
	return e
}

// ForbiddenHint is the hint object of a 403 response.
type ForbiddenHint struct {
	Missing Missing `json:"missing"`
}

func errInvalid(format string, args ...any) *Error {
	return newError(http.StatusBadRequest, KeyInvalid, fmt.Sprintf(format, args...), "")
}

func errNotFound(what string) *Error {
	return newError(http.StatusNotFound, KeyNotFound, what+" not found", "")
}

func errNotImplemented(what string) *Error {
	return newError(http.StatusNotImplemented, KeyNotImplemented, what+" is not available on this node", "")
}

func errUnauthenticated() *Error {
	e := newError(http.StatusUnauthorized, KeyUnauthenticated, "authentication required",
		"send Authorization: Bearer kdr_<id>_<secret>, an OIDC or ServiceAccount token, or log in at /api/v1/auth/login")
	e.GetHeaders().Set("WWW-Authenticate", `Bearer realm="kudurru"`)
	return e
}

func errForbidden(m Missing, reason string) *Error {
	e := newError(http.StatusForbidden, KeyForbidden,
		fmt.Sprintf("permission %s.%s at scope %s is required; ask an administrator for a RoleBinding that grants it", m.Resource, m.Verb, m.Scope), "")
	if reason != "" {
		e.Body.Message += " (" + reason + ")"
	}
	e.Body.Hint = ForbiddenHint{Missing: m}
	e.Body.Missing = &m
	return e
}

func errConflict(message, hint string) *Error {
	return newError(http.StatusConflict, KeyConflict, message, hint)
}

func errManagedBy(key store.Key, owner string) *Error {
	return newError(http.StatusConflict, KeyManagedBy,
		fmt.Sprintf("%s is managed by %s; change it there", key, owner), owner)
}

func errPrecondition(want, have string) *Error {
	return newError(http.StatusPreconditionFailed, KeyPrecondition,
		fmt.Sprintf("If-Match %q does not match the current resourceVersion %q", want, have),
		"read the object again and retry with its current resourceVersion")
}

func errUnavailable(format string, args ...any) *Error {
	return newError(http.StatusServiceUnavailable, KeyUnavailable, fmt.Sprintf(format, args...), "")
}

// storeError maps store, validator and loader errors onto the envelope.
func storeError(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr
	}
	var mb *store.ErrManagedBy
	var ve *validator.Error
	switch {
	case errors.As(err, &mb):
		return errManagedBy(mb.Key, mb.Owner)
	case errors.As(err, &ve):
		e := newError(http.StatusBadRequest, KeyInvalid, ve.Error(), ve.Hint())
		e.Body.Fields = ve.Fields()
		return e
	case errors.Is(err, audit.ErrNotStored):
		return errAuditNotStored()
	case errors.Is(err, central.ErrUnavailable), errors.Is(err, central.ErrIncompatible):
		return newError(http.StatusServiceUnavailable, KeyUnavailable, err.Error(),
			"the central database is not usable from this node; every node keeps its own copy and sends it when the database is back (see the Diagnosis page)")
	case errors.Is(err, validator.ErrUnknownKind):
		return newError(http.StatusBadRequest, KeyInvalid, err.Error(), "")
	case errors.Is(err, store.ErrNotFound):
		return newError(http.StatusNotFound, KeyNotFound, err.Error(), "")
	case errors.Is(err, store.ErrConflict):
		return newError(http.StatusConflict, KeyConflict, "the object changed since it was read (resourceVersion mismatch)",
			"read the object again and retry with its current resourceVersion")
	case errors.Is(err, store.ErrTooLarge):
		return newError(http.StatusRequestEntityTooLarge, KeyTooLarge, err.Error(), "split it into several objects")
	case errors.Is(err, objects.ErrStaticReadOnly):
		return newError(http.StatusServiceUnavailable, KeyReadOnly, err.Error(),
			"objects are read-only through the API in the ha-static shape; edit objects.d on every node")
	case errors.Is(err, store.ErrClosed), errors.Is(err, context.Canceled):
		return newError(http.StatusServiceUnavailable, KeyUnavailable, err.Error(), "")
	case errors.Is(err, context.DeadlineExceeded):
		return newError(http.StatusGatewayTimeout, KeyUnavailable, err.Error(), "")
	}
	return newError(http.StatusInternalServerError, KeyInternal, err.Error(), "")
}

// writeJSON writes v as a JSON response with status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// statusKey is the default key for a status code huma itself produces.
func statusKey(status int) string {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return KeyInvalid
	case http.StatusUnauthorized:
		return KeyUnauthenticated
	case http.StatusForbidden:
		return KeyForbidden
	case http.StatusNotFound:
		return KeyNotFound
	case http.StatusMethodNotAllowed:
		return KeyMethod
	case http.StatusNotAcceptable, http.StatusUnsupportedMediaType:
		return KeyNotAcceptable
	case http.StatusConflict:
		return KeyConflict
	case http.StatusPreconditionFailed:
		return KeyPrecondition
	case http.StatusRequestEntityTooLarge:
		return KeyTooLarge
	case http.StatusTooManyRequests:
		return KeyRateLimited
	case http.StatusNotImplemented:
		return KeyNotImplemented
	case http.StatusServiceUnavailable:
		return KeyUnavailable
	}
	return KeyInternal
}

var installErrorsOnce sync.Once

// installHumaErrors makes huma's own errors (request validation, body
// limits, negotiation) use the kudurru envelope. huma's error constructor
// is a package variable; only this package uses huma, so replacing it once
// per process is safe.
func installHumaErrors() {
	installErrorsOnce.Do(func() {
		huma.NewErrorWithContext = func(_ huma.Context, status int, msg string, errs ...error) huma.StatusError {
			return huma.NewError(status, msg, errs...)
		}
		huma.NewError = func(status int, msg string, errs ...error) huma.StatusError {
			key := statusKey(status)
			if status == http.StatusUnprocessableEntity {
				// Request validation: the contract answers 400 with the
				// field path in the message; 422 stays for semantics.
				status = http.StatusBadRequest
			}
			var details []string
			var fields []string
			for _, e := range errs {
				if e == nil {
					continue
				}
				var d *huma.ErrorDetail
				if errors.As(e, &d) {
					s := d.Message
					if d.Location != "" {
						s = d.Location + ": " + d.Message
						fields = append(fields, d.Location)
					}
					details = append(details, s)
					continue
				}
				details = append(details, e.Error())
			}
			message := msg
			if len(details) > 0 {
				if message != "" {
					message += ": "
				}
				message += strings.Join(details, "; ")
			}
			if message == "" {
				message = http.StatusText(status)
			}
			out := newError(status, key, message, "")
			out.Body.Fields = fields
			return out
		}
	})
}
