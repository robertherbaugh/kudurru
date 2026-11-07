package kube

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// Identity failures are redacted: no server response, URL or credential is
// retained in an error. Availability alone permits saved-state startup.
var (
	ErrClusterIdentityMismatch = errors.New("store/kube: cluster identity does not match store.kube.expectedClusterID")
	errIdentityUnavailable     = apierrors.NewServiceUnavailable("store/kube: cluster identity is unavailable")
	errIdentityUntrusted       = errors.New("store/kube: cluster identity could not be verified")
)

const maxIdentityResponse = 1 << 20

type clusterIdentity struct {
	expected string
	url      string
	client   *http.Client // authenticated transport before installing this guard
	timeout  time.Duration
	observed atomic.Pointer[string]
	group    singleflight.Group
}

// WithExpectedClusterID copies cfg and gates every request on an authenticated
// kube-system UID check. Empty preserves online compatibility. It performs no
// I/O; the probe uses the original transport, so it cannot call its own guard.
func WithExpectedClusterID(cfg *rest.Config, expected string, timeout time.Duration) (*rest.Config, error) {
	out, _, err := pinClusterIdentity(cfg, expected, timeout)
	return out, err
}

func pinClusterIdentity(cfg *rest.Config, expected string, timeout time.Duration) (*rest.Config, *clusterIdentity, error) {
	if cfg == nil {
		return nil, nil, errIdentityUntrusted
	}
	out := rest.CopyConfig(cfg)
	if expected == "" {
		return out, nil, nil
	}
	if timeout <= 0 {
		timeout = DefaultStartupTimeout
	}
	base, _, err := rest.DefaultServerURL(cfg.Host, "", schema.GroupVersion{}, rest.IsConfigTransportTLS(*cfg))
	if err != nil {
		return nil, nil, errIdentityUntrusted
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/api/v1/namespaces/kube-system"
	base.RawQuery, base.Fragment = "", ""
	client, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, nil, errIdentityUntrusted
	}
	probe := *client // never change a shared/default client
	probe.Timeout = timeout
	probe.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	identity := &clusterIdentity{expected: expected, url: base.String(), client: &probe, timeout: timeout}
	out.Wrap(func(rt http.RoundTripper) http.RoundTripper { return &identityTransport{next: rt, identity: identity} })
	return out, identity, nil
}

func (i *clusterIdentity) id() string {
	if v := i.observed.Load(); v != nil {
		return *v
	}
	return ""
}

func (i *clusterIdentity) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if id := i.id(); id != "" {
		if id != i.expected {
			return ErrClusterIdentityMismatch
		}
		return nil
	}
	result := i.group.DoChan("identity", func() (any, error) {
		if id := i.id(); id != "" {
			if id != i.expected {
				return nil, ErrClusterIdentityMismatch
			}
			return nil, nil
		}
		return nil, i.probe(ctx)
	})
	select {
	case <-ctx.Done():
		return ctx.Err()
	case r := <-result:
		return r.Err
	}
}

func (i *clusterIdentity) probe(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, i.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, i.url, nil)
	if err != nil {
		return errIdentityUntrusted
	}
	req.Header.Set("Accept", "application/json")
	res, err := i.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if IsUnavailable(err) {
			return errIdentityUnavailable
		}
		return errIdentityUntrusted
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode >= 500 || res.StatusCode == http.StatusTooManyRequests {
		return errIdentityUnavailable
	}
	if res.StatusCode != http.StatusOK {
		return errIdentityUntrusted
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, maxIdentityResponse+1))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errIdentityUntrusted
	}
	if len(b) > maxIdentityResponse {
		return errIdentityUntrusted
	}
	var ns struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Name string `json:"name"`
			UID  string `json:"uid"`
		} `json:"metadata"`
	}
	if json.Unmarshal(b, &ns) != nil || ns.APIVersion != "v1" || ns.Kind != "Namespace" || ns.Metadata.Name != "kube-system" || ns.Metadata.UID == "" {
		return errIdentityUntrusted
	}
	id := ns.Metadata.UID
	i.observed.Store(&id)
	if id != i.expected {
		return ErrClusterIdentityMismatch
	}
	return nil
}

type identityTransport struct {
	next     http.RoundTripper
	identity *clusterIdentity
}

func (t *identityTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := t.identity.check(r.Context()); err != nil {
		return nil, err
	}
	return t.next.RoundTrip(r)
}

// CheckClusterIdentity checks the configured pin without reading saved state.
// With no pin, normal online discovery remains compatible.
func (s *Store) CheckClusterIdentity(ctx context.Context) error {
	if s.identity == nil {
		return ctx.Err()
	}
	return s.identity.check(ctx)
}

// RESTConfig returns a copy carrying the same identity guard for auxiliary
// clients. Callers must not reconstruct an unguarded client from host config.
func (s *Store) RESTConfig() *rest.Config { return rest.CopyConfig(s.restConfig) }
