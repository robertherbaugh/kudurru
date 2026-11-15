// Package k8s authenticates Kubernetes ServiceAccount tokens for the kube
// store driver (dev-docs/design-record.md §5.5, D21, D55): projected tokens with
// audience "kudurru" are verified offline against the cluster's OIDC
// discovery document and JWKS (read through the apiserver with the pod's
// own credentials), with TokenReview as the fallback when the keys cannot
// be fetched. Both paths require projected JWTs with at most ten minutes from
// issuance to original expiry. Offline verification does not detect deletion;
// this lifetime bounds authority after pod/ServiceAccount deletion. Verdicts
// are cached by sha256(token) for at most two minutes, never past expiry. The principal is
// system:serviceaccount:<namespace>:<name> with its groups; authorization
// is SubjectAccessReview (rbac.SARAuthorizer) or kudurru bindings.
package k8s

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	authenticationv1client "k8s.io/client-go/kubernetes/typed/authentication/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"

	"github.com/robertherbaugh/kudurru/internal/auth"
)

// DefaultAudience is the audience projected tokens must carry (§5.5).
const DefaultAudience = "kudurru"

// Cache bounds: verdicts at most 2 minutes (§5.5), rejections 10 seconds
// (so a flood of junk tokens cannot turn into a flood of TokenReviews).
const (
	MaxCacheTTL      = 2 * time.Minute
	negativeCacheTTL = 10 * time.Second
	maxCacheEntries  = 10_000
	discoveryRetry   = 10 * time.Second
	apiTimeout       = 10 * time.Second
)

// ServiceAccountPrefix prefixes the username of every ServiceAccount.
const ServiceAccountPrefix = "system:serviceaccount:"

// Options configures an Authenticator.
type Options struct {
	// Config is the pod's own apiserver configuration (in-cluster). It
	// reads /.well-known/openid-configuration and /openid/v1/jwks and
	// creates TokenReviews. Required.
	Config *rest.Config
	// Audience required in tokens; default "kudurru".
	Audience string
	// CacheTTL is the verdict cache lifetime; default and maximum 2m.
	CacheTTL time.Duration
	// DisableJWKS forces the TokenReview path (tests, clusters that do not
	// serve discovery).
	DisableJWKS bool
	// Logger; default discards.
	Logger *slog.Logger
	// Now is the clock; default time.Now.
	Now func() time.Time
}

// Authenticator is the Kubernetes branch of the bearer JWT step
// (auth.JWTVerifier). It is safe for concurrent use.
type Authenticator struct {
	opts    Options
	reviews authenticationv1client.TokenReviewsGetter
	hc      *http.Client
	host    string

	mu       sync.Mutex
	verifier *gooidc.IDTokenVerifier
	issuer   string
	lastFail time.Time
	lastErr  error

	cacheMu sync.Mutex
	cache   map[[32]byte]entry
}

type entry struct {
	p         auth.Principal
	err       error
	from, exp time.Time
}

// New validates opts; nothing is fetched until the first token.
func New(opts Options) (*Authenticator, error) {
	if opts.Config == nil {
		return nil, errors.New("k8s: Options.Config is required")
	}
	if opts.Audience == "" {
		opts.Audience = DefaultAudience
	}
	if opts.CacheTTL <= 0 || opts.CacheTTL > MaxCacheTTL {
		opts.CacheTTL = MaxCacheTTL
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	clientConfig := rest.CopyConfig(opts.Config)
	if clientConfig.Timeout <= 0 || clientConfig.Timeout > apiTimeout {
		clientConfig.Timeout = apiTimeout
	}
	hc, err := rest.HTTPClientFor(clientConfig)
	if err != nil {
		return nil, fmt.Errorf("k8s: http client: %w", err)
	}
	reviews, err := authenticationv1client.NewForConfigAndClient(clientConfig, hc)
	if err != nil {
		return nil, fmt.Errorf("k8s: authentication client: %w", err)
	}
	return &Authenticator{opts: opts, reviews: reviews, hc: hc, host: strings.TrimSuffix(opts.Config.Host, "/"), cache: map[[32]byte]entry{}}, nil
}

// Issuer returns the cluster's service-account issuer once discovered.
func (a *Authenticator) Issuer(ctx context.Context) (string, error) {
	_, iss, err := a.discover(ctx)
	return iss, err
}

// discover reads the issuer from the apiserver's discovery document and
// builds a JWKS verifier on /openid/v1/jwks, fetched through the apiserver
// (the jwks_uri of the document may name an address the pod cannot reach).
// Failures are retried no more than every 10 s.
func (a *Authenticator) discover(ctx context.Context) (*gooidc.IDTokenVerifier, string, error) {
	if a.opts.DisableJWKS {
		return nil, "", errors.New("k8s: JWKS verification disabled")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.verifier != nil {
		return a.verifier, a.issuer, nil
	}
	now := a.opts.Now()
	if !a.lastFail.IsZero() && now.Sub(a.lastFail) < discoveryRetry {
		return nil, "", a.lastErr
	}
	iss, err := a.fetchIssuer(ctx)
	if err != nil {
		a.lastFail, a.lastErr = now, err
		a.opts.Logger.WarnContext(ctx, "kubernetes service-account issuer discovery failed; using TokenReview")
		return nil, "", err
	}
	keyCtx := gooidc.ClientContext(context.WithoutCancel(ctx), a.hc)
	keys := gooidc.NewRemoteKeySet(keyCtx, a.host+"/openid/v1/jwks")
	a.verifier = gooidc.NewVerifier(iss, keys, &gooidc.Config{ClientID: a.opts.Audience, Now: a.opts.Now,
		SupportedSigningAlgs: []string{gooidc.RS256, gooidc.ES256, gooidc.RS384, gooidc.RS512, gooidc.ES384, gooidc.ES512}})
	a.issuer = iss
	a.lastFail, a.lastErr = time.Time{}, nil
	return a.verifier, iss, nil
}

func (a *Authenticator) fetchIssuer(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.host+"/.well-known/openid-configuration", nil)
	if err != nil {
		return "", err
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("k8s: discovery: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("k8s: discovery: HTTP %d", resp.StatusCode)
	}
	var doc struct {
		Issuer string `json:"issuer"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil || doc.Issuer == "" {
		return "", fmt.Errorf("k8s: discovery document without issuer: %v", err)
	}
	return doc.Issuer, nil
}

// AcceptsIssuer implements auth.JWTVerifier: tokens whose iss is the
// cluster's service-account issuer.
func (a *Authenticator) AcceptsIssuer(ctx context.Context, iss string) bool {
	_, own, err := a.discover(ctx)
	return err == nil && iss == own
}

// VerifyJWT implements auth.JWTVerifier: JWKS verification when the
// issuer was discovered, TokenReview otherwise.
func (a *Authenticator) VerifyJWT(ctx context.Context, raw string) (auth.Principal, error) {
	return a.verify(ctx, raw, true)
}

// Fallback returns the verifier for bearer JWTs no other verifier owns:
// while discovery is failing it asks TokenReview (the token may be the
// cluster's); once the issuer is known, a token from another issuer is
// rejected without an apiserver round trip.
func (a *Authenticator) Fallback() auth.JWTVerifier { return fallback{a} }

type fallback struct{ a *Authenticator }

func (f fallback) AcceptsIssuer(context.Context, string) bool { return true }

func (f fallback) VerifyJWT(ctx context.Context, raw string) (auth.Principal, error) {
	if _, _, err := f.a.discover(ctx); err == nil {
		return auth.Principal{}, auth.Unauthenticated(auth.ErrUnknownIssuer)
	}
	return f.a.verify(ctx, raw, false)
}

func (a *Authenticator) verify(ctx context.Context, raw string, allowJWKS bool) (auth.Principal, error) {
	now := a.opts.Now()
	claims, bounds, err := projectedBounds(raw, now)
	if err != nil || ctx.Err() != nil {
		return auth.Principal{}, auth.Unauthenticated(errToken)
	}
	key := sha256.Sum256([]byte(raw))
	a.cacheMu.Lock()
	e, ok := a.cache[key]
	a.cacheMu.Unlock()
	if ok && now.Before(e.from) {
		return auth.Principal{}, auth.Unauthenticated(errToken)
	}
	if ok && now.Before(e.exp) {
		finished := a.opts.Now()
		if finished.Before(now) || !bounds.validAt(finished) || ctx.Err() != nil {
			return auth.Principal{}, auth.Unauthenticated(errToken)
		}
		if e.err != nil {
			return auth.Principal{}, e.err
		}
		return detached(e.p), nil
	}
	var p auth.Principal
	v, _, derr := a.discover(ctx)
	if allowJWKS && derr == nil {
		p, _, err = a.verifyJWKS(ctx, v, raw)
	} else {
		p, _, err = a.tokenReview(ctx, raw)
	}
	if err == nil && (p.Name != claims.Sub || p.UID != claims.K8s.ServiceAccount.UID) {
		err = errToken
	}
	finished := a.opts.Now()
	if err == nil && (finished.Before(now) || !bounds.validAt(finished) || ctx.Err() != nil) {
		err = errToken
	}
	ttl := minTime(now.Add(a.opts.CacheTTL), bounds.expiresAt)
	if err != nil {
		// Neither JWT-library nor apiserver errors may disclose raw proof or
		// response data through the stack's internal-cause logging.
		err = auth.Unauthenticated(errUnavailable)
		p = auth.Principal{}
		ttl = minTime(now.Add(negativeCacheTTL), bounds.expiresAt)
	} else {
		p.CredentialIssuedAt = bounds.issuedAt
		p = auth.WithCredentialTimeBounds(p, now, bounds.issuedAt, bounds.expiresAt, a.opts.Now)
	}
	a.cacheMu.Lock()
	if len(a.cache) >= maxCacheEntries {
		for k, old := range a.cache {
			if !now.Before(old.exp) {
				delete(a.cache, k)
			}
		}
		if len(a.cache) >= maxCacheEntries {
			clear(a.cache)
		}
	}
	a.cache[key] = entry{p: detached(p), err: err, from: now, exp: ttl}
	a.cacheMu.Unlock()
	return p, err
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func detached(p auth.Principal) auth.Principal {
	p.Groups = slices.Clone(p.Groups)
	return p
}

// saClaims is the kubernetes.io claim of a projected token.
type saClaims struct {
	Sub string `json:"sub"`
	K8s struct {
		Namespace      string `json:"namespace"`
		ServiceAccount struct {
			Name string `json:"name"`
			UID  string `json:"uid"`
		} `json:"serviceaccount"`
	} `json:"kubernetes.io"`
}

func (a *Authenticator) verifyJWKS(ctx context.Context, v *gooidc.IDTokenVerifier, raw string) (auth.Principal, time.Time, error) {
	idt, err := v.Verify(ctx, raw)
	if err != nil {
		return auth.Principal{}, time.Time{}, err
	}
	var c saClaims
	if err := idt.Claims(&c); err != nil {
		return auth.Principal{}, time.Time{}, err
	}
	ns, name := c.K8s.Namespace, c.K8s.ServiceAccount.Name
	if ns == "" || name == "" || c.Sub != ServiceAccountPrefix+ns+":"+name {
		return auth.Principal{}, time.Time{}, errors.New("k8s: not a ServiceAccount token")
	}
	groups := []string{"system:serviceaccounts", "system:serviceaccounts:" + ns, "system:authenticated"}
	return Principal(c.Sub, c.K8s.ServiceAccount.UID, groups), idt.Expiry, nil
}

func (a *Authenticator) tokenReview(ctx context.Context, raw string) (auth.Principal, time.Time, error) {
	// client-go throttles before installing its REST request timeout. Bound
	// that wait too, preserving any shorter caller or client deadline.
	ctx, cancel := context.WithTimeout(ctx, a.hc.Timeout)
	defer cancel()
	tr, err := a.reviews.TokenReviews().Create(ctx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: raw, Audiences: []string{a.opts.Audience}},
	}, metav1.CreateOptions{})
	if err != nil {
		return auth.Principal{}, time.Time{}, errUnavailable
	}
	st := tr.Status
	if !st.Authenticated || st.Error != "" {
		return auth.Principal{}, time.Time{}, errToken
	}
	audOK := false
	for _, aud := range st.Audiences {
		if aud == a.opts.Audience {
			audOK = true
		}
	}
	if !audOK {
		return auth.Principal{}, time.Time{}, errToken
	}
	if !strings.HasPrefix(st.User.Username, ServiceAccountPrefix) {
		return auth.Principal{}, time.Time{}, errors.New("k8s: only ServiceAccount tokens are accepted")
	}
	return Principal(st.User.Username, st.User.UID, st.User.Groups), time.Time{}, nil
}

// Principal builds the principal of a Kubernetes ServiceAccount from its
// username system:serviceaccount:<namespace>:<name>. ServiceAccount is
// "<namespace>/<name>", so kudurru RoleBinding subjects of kind
// ServiceAccount match it as well as SubjectAccessReview.
func Principal(username, uid string, groups []string) auth.Principal {
	nsName := strings.TrimPrefix(username, ServiceAccountPrefix)
	ns, name, _ := strings.Cut(nsName, ":")
	return auth.Principal{
		Kind: auth.KindKubernetesServiceAccount, ID: "k8s:" + username, Name: username,
		Groups: append([]string(nil), groups...), Provider: "kubernetes", AuthMethod: auth.MethodK8s,
		UID: uid, ServiceAccount: ns + "/" + name,
	}
}

// SecretResolver reads keys of Secrets in namespace (an IdentityProvider's
// clientSecretRef); it matches oidc.SecretResolver.
func SecretResolver(secrets corev1client.SecretsGetter, namespace string) func(ctx context.Context, name, key string) (string, error) {
	return func(ctx context.Context, name, key string) (string, error) {
		s, err := secrets.Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		v, ok := s.Data[key]
		if !ok {
			return "", fmt.Errorf("secret %s/%s has no key %q", namespace, name, key)
		}
		return string(v), nil
	}
}
