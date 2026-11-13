package kube

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/robertherbaugh/kudurru/internal/store"
)

// Impersonation is the Kubernetes identity one write is sent as
// (auth.kubernetes.mode impersonate, D386): the Impersonate-User and
// Impersonate-Group headers of the request.
type Impersonation struct {
	User   string
	Groups []string
}

// ImpersonateFunc names the identity a write made with ctx is sent as. It
// returns false when the write runs as the driver's own identity: system
// writes without a requesting principal, and principals the caller does not
// impersonate.
type ImpersonateFunc func(ctx context.Context) (Impersonation, bool)

// Impersonation headers (k8s.io/api/authentication/v1). Every one of them is
// removed from an impersonated request before the two set here, so nothing
// else in the process can add a UID or extra to the identity.
const (
	impersonateUserHeader  = "Impersonate-User"
	impersonateGroupHeader = "Impersonate-Group"
	impersonateHeaderStem  = "Impersonate-"
)

type impersonationKey struct{}

// writeContext marks ctx for the impersonating transport when
// Options.Impersonate names an identity for it. Only the apiserver writes of
// Apply, Delete and a Force takeover use the marked context: the managedBy
// reads before them, status and finalizer updates, internal kinds (Secrets),
// Leases and informers stay on the driver's identity. A nil Impersonation
// means the write is not impersonated.
func (s *Store) writeContext(ctx context.Context) (context.Context, *Impersonation, error) {
	if s.opts.Impersonate == nil {
		return ctx, nil, nil
	}
	im, ok := s.opts.Impersonate(ctx)
	if !ok {
		return ctx, nil, nil
	}
	if im.User == "" {
		// The principal's credential ended during the request, or it names
		// nobody: an authentication failure, never a write as the driver.
		return nil, nil, fmt.Errorf("%w: the request's credential is no longer valid or names no user", store.ErrNoIdentity)
	}
	if err := im.validate(); err != nil {
		return nil, nil, fmt.Errorf("%w: %w", store.ErrForbidden, err)
	}
	im.Groups = append([]string(nil), im.Groups...)
	return context.WithValue(ctx, impersonationKey{}, &im), &im, nil
}

// validate refuses an identity the apiserver could not receive intact: an
// empty user, or a name that is not a valid header value. A write is never
// downgraded to the driver's own identity.
func (im Impersonation) validate() error {
	if im.User == "" {
		return errors.New("store/kube: impersonated write without a user name")
	}
	for _, v := range append([]string{im.User}, im.Groups...) {
		if v == "" || strings.TrimSpace(v) != v || strings.ContainsFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return fmt.Errorf("store/kube: cannot impersonate %q: not a valid Kubernetes user or group name", v)
		}
	}
	return nil
}

// impersonating is the rest.Config transport wrapper that sends a marked
// request as its Impersonation. Unmarked requests pass unchanged.
func impersonating(rt http.RoundTripper) http.RoundTripper {
	return &impersonatingTransport{next: rt}
}

type impersonatingTransport struct{ next http.RoundTripper }

// RoundTrip implements http.RoundTripper.
func (t *impersonatingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	im, ok := r.Context().Value(impersonationKey{}).(*Impersonation)
	if !ok {
		return t.next.RoundTrip(r)
	}
	r = r.Clone(r.Context())
	for name := range r.Header {
		if strings.HasPrefix(http.CanonicalHeaderKey(name), impersonateHeaderStem) {
			delete(r.Header, name)
		}
	}
	r.Header.Set(impersonateUserHeader, im.User)
	for _, g := range im.Groups {
		r.Header.Add(impersonateGroupHeader, g)
	}
	return t.next.RoundTrip(r)
}

// impersonationRefusal turns the apiserver's 403 for an impersonated write
// into a definite refusal (nothing was written). A refusal of the driver's
// own impersonate verb is store.ErrImpersonationDenied: a deployment fault,
// logged here with the apiserver's message, which names the driver's own
// identity and is therefore kept out of the returned error. Every other 403
// (Kubernetes RBAC for the principal, an admission webhook, a terminating
// namespace) is store.ErrForbidden with the apiserver's reason. It returns
// nil for other errors and for writes that were not impersonated.
func (s *Store) impersonationRefusal(im *Impersonation, op string, key store.Key, err error) error {
	if im == nil || !apierrors.IsForbidden(err) {
		return nil
	}
	if impersonateVerbDenied(im, err) {
		s.log.Error("store/kube: the apiserver refused kudurru the impersonate verb; grant it (auth.kubernetes.mode impersonate) or switch to sar",
			"op", op, "key", key.String(), "error", statusMessage(err))
		return fmt.Errorf("%w: %s %s could not be sent as %q", store.ErrImpersonationDenied, op, key, im.User)
	}
	return fmt.Errorf("%w: %s %s as %q: %s", store.ErrForbidden, op, key, im.User, statusMessage(err))
}

// impersonateVerbDenied reports whether a 403 came from the apiserver's
// impersonation check of the driver's own identity rather than from the
// principal's RBAC on the object or an admission webhook. Only the
// apiserver's own form counts (responsewriters.ForbiddenStatusError):
// "<resource> ["<name>"] is forbidden: User "<requester>" cannot <verb> ...",
// where the verb is impersonate (legacy impersonation), impersonate:<mode>
// or impersonate-on:<mode>:<verb> (constrained impersonation, Kubernetes
// 1.35 and later), and the requester is not the impersonated user. The
// requester is a quoted Go string, so a user name containing the phrase
// cannot end it early; a webhook's message starts with "admission webhook"
// and never has this form; and an RBAC refusal of the principal names the
// principal with a verb such as patch.
func impersonateVerbDenied(im *Impersonation, err error) bool {
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return false
	}
	m := apiserverImpersonateDenial.FindStringSubmatch(status.Status().Message)
	if m == nil {
		return false
	}
	requester, uerr := strconv.Unquote(m[1])
	return uerr == nil && (im == nil || requester != im.User)
}

// apiserverImpersonateDenial matches the start of the apiserver's 403 for a
// refused impersonate verb; group 1 is the quoted requesting user.
var apiserverImpersonateDenial = regexp.MustCompile(`^(?:forbidden|[a-z0-9.-]+(?: "(?:[^"\\]|\\.)*")? is forbidden): User ("(?:[^"\\]|\\.)*") cannot impersonate(?:[ :]|-on:)`)

func statusMessage(err error) string {
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		return status.Status().Message
	}
	return err.Error()
}
