package kube

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
// into store.ErrForbidden: Kubernetes RBAC refused the principal (or refused
// kudurru the impersonate verb), and nothing was written. It returns nil for
// other errors and for writes that were not impersonated.
func impersonationRefusal(im *Impersonation, op string, key store.Key, err error) error {
	if im == nil || !apierrors.IsForbidden(err) {
		return nil
	}
	return fmt.Errorf("%w: %s %s as %q: %s", store.ErrForbidden, op, key, im.User, statusMessage(err))
}

func statusMessage(err error) string {
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		return status.Status().Message
	}
	return err.Error()
}
