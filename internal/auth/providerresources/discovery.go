package providerresources

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/oidccore"
	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
)

type fetchCache struct {
	retain    bool
	url, etag string
	body      []byte
	until     time.Time
}
type metadata struct {
	issuer, authorization, token, jwksURL string
	algorithms                            []string
	browser                               bool
	discovery, keys                       fetchCache
}
type acquisition struct {
	descriptor servingauth.ProviderDescriptor
	binding    providerregistry.AuthorityBinding
	metadata   metadata
	verifier   *oidccore.Verifier
	until      time.Time
	reason     string
	transient  bool
}

func (o *Owner) acquire(ctx context.Context, d Desired, source servingauth.ProviderDescriptor, prior entry) acquisition {
	out := acquisition{descriptor: source, reason: "host-policy"}
	if !d.Host.allowed {
		return out
	}
	r := d.Host.objects
	if source.Ref.Origin == servingauth.ProviderConfig {
		if d.Host.configSpecs[source.Ref.ID] != source.SpecIdentity {
			return out
		}
		r = d.Host.configs[source.Ref.ID]
	}
	if r == nil || source.Spec.ClientSecretRef == nil {
		out.reason = "credentials"
		return out
	}
	v, err := r.Resolve(ctx, *source.Spec.ClientSecretRef)
	if err != nil || len(v.Bytes) > MaxCredentialBytes {
		clear(v.Bytes)
		out.reason = "credentials"
		return out
	}
	defer clear(v.Bytes)
	// No credential bytes survive acquisition or enter the serving resource.
	if len(bytes.TrimSpace(v.Bytes)) == 0 {
		out.reason = "credentials"
		return out
	}
	out.binding = providerregistry.AuthorityBinding{Profile: "provider-authority/v1", FingerprintKeyID: d.Host.fingerprintKey, CredentialFingerprint: v.Fingerprint, CredentialPolicy: d.Host.credentialPolicy, NetworkPolicy: d.Host.network.identity, EntitlementIdentity: d.Host.entitlement}
	client, closeClient := d.Host.network.client(o.opts.OperationTimeout)
	defer closeClient()
	discoveryURL := strings.TrimRight(source.Issuer, "/") + "/.well-known/openid-configuration"
	discovery, transient, err := o.fetch(ctx, client, discoveryURL, prior.metadata.discovery, MaxDiscoveryBytes)
	if err != nil {
		out.reason = "discovery"
		out.transient = transient && prior.resource.Availability == servingauth.ProviderReady && bindingPrefixMatches(source, d, out.binding)
		return out
	}
	m, err := parseMetadata(discovery.body, source.Issuer, d.Host.network)
	if err != nil {
		out.reason = "discovery"
		return out
	}
	m.discovery = discovery
	out.binding.MetadataBinding = hash(struct {
		Profile, Issuer, Authorization, Token, JWKS string
		Algorithms                                  []string
		Browser                                     bool
	}{"provider-metadata/v2-browser", m.issuer, m.authorization, m.token, m.jwksURL, m.algorithms, m.browser})
	keyCache := prior.metadata.keys
	if keyCache.url != m.jwksURL {
		keyCache = fetchCache{}
	}
	keys, transient, err := o.fetch(ctx, client, m.jwksURL, keyCache, oidccore.MaxJWKSBytes)
	if err != nil {
		out.reason = "keys"
		out.transient = transient && sameBinding(d, source, out.binding)
		return out
	}
	verifier, err := oidccore.NewVerifier(source.Issuer, m.algorithms, keys.body)
	if err != nil {
		out.reason = "keys"
		return out
	}
	m.keys = keys
	if !m.discovery.retain {
		m.discovery.body = nil
		m.discovery.etag = ""
	}
	if !m.keys.retain {
		m.keys.body = nil
		m.keys.etag = ""
	}
	out.metadata = m
	out.verifier = verifier
	out.reason = ""
	out.until = discovery.until
	if keys.until.Before(out.until) {
		out.until = keys.until
	}
	return out
}

// Compare actual newly observed material before allowing a transient old cut.
func bindingPrefixMatches(source servingauth.ProviderDescriptor, d Desired, b providerregistry.AuthorityBinding) bool {
	for _, a := range d.Authorities {
		if a.Active && a.Provider == source.Ref && a.SpecIdentity == source.SpecIdentity && a.Binding.Profile == b.Profile && a.Binding.FingerprintKeyID == b.FingerprintKeyID && a.Binding.CredentialFingerprint == b.CredentialFingerprint && a.Binding.CredentialPolicy == b.CredentialPolicy && a.Binding.NetworkPolicy == b.NetworkPolicy && a.Binding.EntitlementIdentity == b.EntitlementIdentity {
			return true
		}
	}
	return false
}
func sameBinding(d Desired, source servingauth.ProviderDescriptor, b providerregistry.AuthorityBinding) bool {
	for _, a := range d.Authorities {
		if a.Active && a.Provider == source.Ref && a.SpecIdentity == source.SpecIdentity && a.Binding == b {
			return true
		}
	}
	return false
}

func parseMetadata(raw []byte, issuer string, n *networkPolicy) (metadata, error) {
	fields, err := boundedObject(raw, MaxDiscoveryBytes)
	if err != nil {
		return metadata{}, err
	}
	got, err := jsonString(fields, "issuer", true)
	if err != nil || got != issuer {
		return metadata{}, ErrInvalid
	}
	keys, err := jsonString(fields, "jwks_uri", true)
	if err != nil {
		return metadata{}, err
	}
	if _, err = n.endpoint(keys); err != nil {
		return metadata{}, err
	}
	auth, err := jsonString(fields, "authorization_endpoint", false)
	if err != nil {
		return metadata{}, err
	}
	if auth != "" {
		if _, err = n.endpoint(auth); err != nil {
			return metadata{}, err
		}
	}
	token, err := jsonString(fields, "token_endpoint", false)
	if err != nil {
		return metadata{}, err
	}
	if token != "" {
		if _, err = n.endpoint(token); err != nil {
			return metadata{}, err
		}
	}
	algorithms := []string{"RS256"}
	if raw, ok := fields["id_token_signing_alg_values_supported"]; ok {
		algorithms, err = boundedStrings(raw, 64, 16)
		if err != nil || len(algorithms) == 0 {
			return metadata{}, ErrInvalid
		}
	}
	accepted := []string{}
	for _, a := range n.algorithms {
		if slices.Contains(algorithms, a) {
			accepted = append(accepted, a)
		}
	}
	if len(accepted) == 0 {
		return metadata{}, ErrInvalid
	}
	browser := auth != "" && token != ""
	for _, requirement := range []struct{ field, value string }{
		{"token_endpoint_auth_methods_supported", "client_secret_basic"},
		{"code_challenge_methods_supported", "S256"},
		{"grant_types_supported", "authorization_code"},
		{"response_types_supported", "code"},
	} {
		if raw, ok := fields[requirement.field]; ok {
			values, err := boundedStrings(raw, 64, 128)
			if err != nil {
				return metadata{}, err
			}
			browser = browser && slices.Contains(values, requirement.value)
		}
	}
	return metadata{issuer: got, authorization: auth, token: token, jwksURL: keys, algorithms: accepted, browser: browser}, nil
}

func (o *Owner) fetch(ctx context.Context, client *http.Client, url string, cache fetchCache, limit int) (fetchCache, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fetchCache{}, false, ErrInvalid
	}
	if cache.url == url && cache.etag != "" && len(cache.body) > 0 {
		req.Header.Set("If-None-Match", cache.etag)
	}
	response, err := client.Do(req)
	if err != nil {
		return fetchCache{}, false, ErrUnavailable
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode >= 500 {
		return fetchCache{}, true, ErrUnavailable
	}
	if response.StatusCode == http.StatusNotModified {
		if cache.url != url || cache.etag == "" || len(cache.body) == 0 {
			return fetchCache{}, false, ErrInvalid
		}
		if etag := response.Header.Get("ETag"); etag != "" && etag != cache.etag {
			return fetchCache{}, false, ErrInvalid
		}
		cache.retain = !strings.Contains(strings.ToLower(response.Header.Get("Cache-Control")), "no-store")
		cache.until = o.opts.Now().Add(o.freshness(response.Header))
		return cache, false, nil
	}
	if response.StatusCode != http.StatusOK {
		return fetchCache{}, false, ErrUnavailable
	}
	if response.ContentLength > int64(limit) {
		return fetchCache{}, false, ErrBounds
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(limit)+1))
	if err != nil {
		return fetchCache{}, true, ErrUnavailable
	}
	if len(body) > limit {
		return fetchCache{}, false, ErrBounds
	}
	if _, err := boundedObject(body, limit); err != nil {
		return fetchCache{}, false, err
	}
	etag := response.Header.Get("ETag")
	if len(etag) > 512 || !safe(etag, 512, true) {
		return fetchCache{}, false, ErrBounds
	}
	return fetchCache{retain: !strings.Contains(strings.ToLower(response.Header.Get("Cache-Control")), "no-store"), url: url, etag: etag, body: body, until: o.opts.Now().Add(o.freshness(response.Header))}, false, nil
}
func (o *Owner) freshness(header http.Header) time.Duration {
	ttl := o.opts.RefreshInterval
	for _, field := range strings.Split(header.Get("Cache-Control"), ",") {
		name, value, _ := strings.Cut(strings.TrimSpace(field), "=")
		name = strings.ToLower(name)
		if name == "no-store" || name == "no-cache" {
			if o.opts.RetryInterval < ttl {
				ttl = o.opts.RetryInterval
			}
		}
		if name == "max-age" {
			seconds, err := strconv.ParseInt(strings.Trim(value, `"`), 10, 64)
			if err != nil || seconds < 0 {
				return 0
			}
			if seconds < int64(ttl/time.Second) {
				ttl = time.Duration(seconds) * time.Second
			}
		}
	}
	if age, err := strconv.ParseInt(header.Get("Age"), 10, 64); err == nil && age >= 0 {
		if age > int64(ttl/time.Second) {
			ttl = 0
		} else {
			ttl -= time.Duration(age) * time.Second
		}
	}
	if ttl > o.opts.MaxFreshness {
		ttl = o.opts.MaxFreshness
	}
	if ttl < 0 {
		return 0
	}
	return ttl
}
