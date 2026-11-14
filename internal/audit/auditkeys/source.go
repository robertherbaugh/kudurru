package auditkeys

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/rest"
)

// DefaultSecret is operator-provisioned in the existing AuditLog namespace.
// #nosec G101 -- public Kubernetes resource name, not authentication material
const DefaultSecret = "kudurru-audit-keys"
const maxSecretResponse = 256 << 10

// Source performs bounded uncached GETs. It never watches, mutates, derives or
// repairs a Secret; a later operation can recover after a startup outage.
type Source struct {
	client          *http.Client
	endpoint        *url.URL
	namespace, name string
	cluster         func() string
	mu              sync.Mutex
	original        string
}

// NewSource reuses the caller's authenticated, cluster-pinned transport.
func NewSource(cfg *rest.Config, namespace, name string, cluster func() string) (*Source, error) {
	if cfg == nil || cluster == nil || len(validation.IsDNS1123Label(namespace)) != 0 || len(validation.IsDNS1123Subdomain(name)) != 0 {
		return nil, ErrUnavailable
	}
	base, _, err := rest.DefaultServerUrlFor(cfg)
	if err != nil || base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Scheme != "http" && base.Scheme != "https") {
		return nil, ErrUnavailable
	}
	client, err := rest.HTTPClientFor(rest.CopyConfig(cfg))
	if err != nil {
		return nil, ErrUnavailable
	}
	bounded := *client
	bounded.Timeout = 3 * time.Second
	if client.Timeout > 0 && client.Timeout < bounded.Timeout {
		bounded.Timeout = client.Timeout
	}
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Source{client: &bounded, endpoint: base.JoinPath("api", "v1", "namespaces", namespace, "secrets", name), namespace: namespace, name: name, cluster: cluster}, nil
}

// Load captures one original ring and a nonsecret revision used by the verifier
// cache. No URL, response/error text or private data escapes this boundary.
func (s *Source) Load(ctx context.Context) (*Ring, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if s == nil {
		return nil, "", ErrUnavailable
	}
	cluster := s.cluster()
	s.mu.Lock()
	if s.original == "" && cluster != "" {
		s.original = cluster
	}
	matches := cluster != "" && cluster == s.original
	s.mu.Unlock()
	if !matches {
		return nil, "", ErrUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.endpoint.String(), nil)
	if err != nil {
		return nil, "", ErrUnavailable
	}
	response, err := s.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		return nil, "", ErrUnavailable
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, "", ErrUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxSecretResponse+1))
	if ctx.Err() != nil {
		return nil, "", ctx.Err()
	}
	if err != nil || len(body) > maxSecretResponse || !uniqueJSON(body) {
		return nil, "", ErrUnavailable
	}
	var secret struct {
		Metadata struct {
			UID       string `json:"uid"`
			Revision  string `json:"resourceVersion"`
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Data map[string][]byte `json:"data"`
	}
	if json.Unmarshal(body, &secret) != nil || secret.Metadata.UID == "" || secret.Metadata.Revision == "" || secret.Metadata.Name != s.name || secret.Metadata.Namespace != s.namespace {
		return nil, "", ErrUnavailable
	}
	ring, err := Parse(secret.Data["ring.json"])
	if err != nil || !ring.ScopeMatches(cluster, s.namespace) || s.cluster() != cluster {
		return nil, "", ErrUnavailable
	}
	sum := sha256.New()
	for _, v := range []string{secret.Metadata.UID, secret.Metadata.Revision, string(secret.Data["ring.json"])} {
		_, _ = sum.Write([]byte(v))
		_, _ = sum.Write([]byte{0})
	}
	return ring, hex.EncodeToString(sum.Sum(nil)), nil
}

// Current checks only the original revision; it never adopts its replacement.
func (s *Source) Current(ctx context.Context, identity string) error {
	_, current, err := s.Load(ctx)
	if err != nil {
		return err
	}
	if current != identity {
		return ErrUnavailable
	}
	return nil
}
