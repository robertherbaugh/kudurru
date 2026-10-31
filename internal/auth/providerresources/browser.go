package providerresources

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/robertherbaugh/kudurru/internal/auth/browseroidc"
	"github.com/robertherbaugh/kudurru/internal/auth/oidccore"
	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/credentials"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
)

type browserSnapshot struct {
	host        *HostCut
	authorities []providerregistry.AuthorityRecord
}

func captureBrowser(d Desired) *browserSnapshot {
	return &browserSnapshot{host: d.Host, authorities: slices.Clone(d.Authorities)}
}

// BrowserInfo is detached nonsecret metadata for an exact captured capability.
type BrowserInfo struct {
	Provider                                         servingauth.ProviderRef
	SpecIdentity, ResourceIdentity, Issuer, ClientID string
	RedirectURI, AuthorizationURL, EndSessionURL     string
	Scopes                                           []string
}

// Browser borrows its original lease. It never implies writer admission and must
// not outlive the complete HTTP lease. Exchange is explicit I/O outside ALL gates.
type Browser struct {
	lease     *Lease
	info      BrowserInfo
	resolver  *credentials.Resolver
	ref       servingauth.ProviderDescriptor
	binding   providerregistry.AuthorityBinding
	network   *networkPolicy
	tokenURL  string
	current   func() bool
	attempted *atomic.Bool
}

func (*Browser) String() string { return "private captured browser capability" }

// GoString redacts credentials and network capabilities.
func (b *Browser) GoString() string { return b.String() }

// MarshalJSON prevents implicit capability serialization.
func (*Browser) MarshalJSON() ([]byte, error) { return []byte(`"private browser capability"`), nil }

// Browser selects ONLY an exact ready resource from this retained cut. It does
// no discovery, credential resolution, I/O, source selection, or writer admission.
func (l *Lease) Browser(ref servingauth.ProviderRef, resourceID string) (*Browser, error) {
	if l == nil || l.owner == nil || !l.Current() {
		return nil, ErrStale
	}
	cut := l.snapshot.browser
	if cut == nil || cut.host == nil || !cut.host.allowed {
		return nil, ErrUnavailable
	}
	for _, e := range l.snapshot.entries {
		if e.descriptor.Ref != ref {
			continue
		}
		id, err := servingauth.ProviderResourceIdentity(e.descriptor, e.resource)
		if err != nil || id != resourceID || e.resource.Availability != servingauth.ProviderReady {
			return nil, ErrStale
		}
		if !e.metadata.browser || e.descriptor.Spec.RedirectURL == "" {
			return nil, ErrUnavailable
		}
		resolver := cut.host.objects
		if ref.Origin == servingauth.ProviderConfig {
			if cut.host.configSpecs[ref.ID] != e.descriptor.SpecIdentity {
				return nil, ErrStale
			}
			resolver = cut.host.configs[ref.ID]
		}
		if resolver == nil || e.descriptor.Spec.ClientSecretRef == nil {
			return nil, ErrUnavailable
		}
		for _, a := range cut.authorities {
			if a.Active && a.Provider == ref && a.SpecIdentity == e.descriptor.SpecIdentity && hash(a.Binding) == e.resource.TrustIdentity {
				return &Browser{lease: l, resolver: resolver, ref: e.descriptor, binding: a.Binding, network: cut.host.network, tokenURL: e.metadata.token, attempted: &atomic.Bool{},
					info: BrowserInfo{Provider: ref, SpecIdentity: e.descriptor.SpecIdentity, ResourceIdentity: id, Issuer: e.descriptor.Issuer,
						ClientID: e.descriptor.Spec.ClientID, RedirectURI: e.descriptor.Spec.RedirectURL, AuthorizationURL: e.metadata.authorization, EndSessionURL: e.metadata.endSession, Scopes: slices.Clone(e.descriptor.Spec.Scopes)}}, nil
			}
		}
		return nil, ErrStale
	}
	return nil, ErrUnavailable
}

// BindCurrent narrows a capability with the retained host's lock-free current
// predicate. It cannot broaden resource validity. All derived handles share one
// exchange latch; nil denies. Coordinator owns the exact host join. Selecting a
// new handle is not durable single consumption of a code or browser attempt.
func (b *Browser) BindCurrent(current func() bool) (*Browser, error) {
	if b == nil || current == nil || !b.Current() || !current() {
		return nil, ErrStale
	}
	return &Browser{lease: b.lease, info: b.Info(), resolver: b.resolver, ref: b.ref, binding: b.binding, network: b.network, tokenURL: b.tokenURL,
		current: func() bool { return b.Current() && current() }, attempted: b.attempted}, nil
}

// Info returns only detached nonsecret metadata, including a detached scope list.
func (b *Browser) Info() BrowserInfo {
	if b == nil {
		return BrowserInfo{}
	}
	i := b.info
	i.Scopes = slices.Clone(i.Scopes)
	return i
}

// Current is memory/clock only, never writer admission or command authorization.
func (b *Browser) Current() bool {
	return b != nil && b.lease.Current() && (b.current == nil || b.current())
}

// BrowserTokens is transient raw proof; refresh tokens and diagnostics are never
// retained. Only explicit LoginProof can expose it to current verification.
type BrowserTokens struct {
	info                 BrowserInfo
	idToken, accessToken string
}

func (BrowserTokens) String() string { return "private browser token proof" }

// GoString redacts returned token material.
func (t BrowserTokens) GoString() string { return t.String() }

// MarshalJSON prevents token persistence through generic serialization.
func (BrowserTokens) MarshalJSON() ([]byte, error) { return []byte(`"private token proof"`), nil }

// LoginProof binds raw exchanged proof to this exact source/resource. The caller
// supplies the nonce from authenticated preauth, consumed in the SAME command.
// This is raw evidence, not an authenticated principal or cached JWT verdict.
func (t BrowserTokens) LoginProof(expectedNonce string) servingauth.ExternalLoginProof {
	if t.idToken == "" || !safe(expectedNonce, oidccore.MaxStringBytes, false) {
		return servingauth.ExternalLoginProof{}
	}
	return servingauth.ExternalLoginProof{Provider: t.info.Provider, SpecIdentity: t.info.SpecIdentity, ResourceIdentity: t.info.ResourceIdentity,
		RawIDToken: t.idToken, Mode: oidccore.BrowserIDToken, Browser: oidccore.BrowserProof{Nonce: expectedNonce, AccessToken: t.accessToken}}
}

// A global nonblocking budget also bounds simultaneous browser calls across
// owners. No unbounded queue or goroutine is created for admission.
var browserOperations = make(chan struct{}, 4)

// Exchange sends at most ONE authorization-code request with S256 and explicit
// client_secret_basic. It must run outside every publication/owner/Store gate.
// Current is checked before/after resolution and I/O. Changes never select a
// successor. Root still retains original HTTP lease and current-command gates.
func (b *Browser) Exchange(ctx context.Context, code, verifier string) (BrowserTokens, error) {
	if ctx == nil {
		return BrowserTokens{}, ErrInvalid
	}
	if !safe(code, browseroidc.MaxCodeBytes, false) {
		return BrowserTokens{}, ErrInvalid
	}
	if _, err := browseroidc.S256(verifier); err != nil {
		return BrowserTokens{}, ErrInvalid
	}
	if !b.Current() {
		return BrowserTokens{}, ErrStale
	}
	if !b.attempted.CompareAndSwap(false, true) {
		return BrowserTokens{}, ErrInvalid
	}
	select {
	case browserOperations <- struct{}{}:
		defer func() { <-browserOperations }()
	default:
		return BrowserTokens{}, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, b.lease.owner.opts.OperationTimeout)
	defer cancel()
	started := b.lease.owner.opts.Now()
	current := func() bool { return ctx.Err() == nil && b.Current() && !b.lease.owner.opts.Now().Before(started) }
	value, err := b.resolver.Resolve(ctx, *b.ref.Spec.ClientSecretRef)
	defer clear(value.Bytes)
	if err != nil || len(value.Bytes) > MaxCredentialBytes || len(bytes.TrimSpace(value.Bytes)) == 0 {
		return BrowserTokens{}, ErrUnavailable
	}
	if subtle.ConstantTimeCompare([]byte(value.Fingerprint), []byte(b.binding.CredentialFingerprint)) != 1 || !current() {
		return BrowserTokens{}, ErrStale
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier}, "redirect_uri": {b.info.RedirectURI}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return BrowserTokens{}, ErrInvalid
	}
	// RFC 6749 section 2.3.1 requires form encoding BEFORE HTTP Basic encoding.
	req.SetBasicAuth(url.QueryEscape(b.info.ClientID), url.QueryEscape(string(value.Bytes)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.GetBody = nil
	req.Close = true
	client, closeClient := b.network.client(b.lease.owner.opts.OperationTimeout)
	defer closeClient()
	// No reused connection, HTTP/2 stream retry, redirect, or auth-style probe
	// may automatically replay the one-shot code request.
	transport := client.Transport.(boundedTransport)
	transport.base.DisableKeepAlives = true
	transport.base.ForceAttemptHTTP2 = false
	transport.base.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	if !current() {
		return BrowserTokens{}, ErrStale
	}
	response, err := client.Do(req)
	if err != nil {
		return BrowserTokens{}, ErrUnavailable
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return BrowserTokens{}, ErrUnavailable
	}
	if response.ContentLength > MaxTokenResponseBytes {
		return BrowserTokens{}, ErrBounds
	}
	mediaType, params, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" || params["charset"] != "" && !strings.EqualFold(params["charset"], "utf-8") {
		return BrowserTokens{}, ErrInvalid
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, MaxTokenResponseBytes+1))
	defer clear(raw)
	if err != nil {
		return BrowserTokens{}, ErrUnavailable
	}
	if len(raw) > MaxTokenResponseBytes {
		return BrowserTokens{}, ErrBounds
	}
	tokens, err := parseBrowserTokens(raw)
	if err != nil {
		return BrowserTokens{}, err
	}
	if !current() {
		return BrowserTokens{}, ErrStale
	}
	tokens.info = b.Info()
	return tokens, nil
}

func parseBrowserTokens(raw []byte) (BrowserTokens, error) {
	fields, err := boundedObject(raw, MaxTokenResponseBytes)
	if err != nil {
		return BrowserTokens{}, err
	}
	defer func() {
		for _, value := range fields {
			clear(value)
		}
	}()
	if _, exists := fields["error"]; exists {
		return BrowserTokens{}, ErrUnavailable
	}
	values := make([]string, 3)
	for i, key := range []string{"id_token", "access_token", "token_type"} {
		if json.Unmarshal(fields[key], &values[i]) != nil || !safe(values[i], oidccore.MaxTokenBytes, false) {
			return BrowserTokens{}, ErrInvalid
		}
	}
	if !strings.EqualFold(values[2], "Bearer") {
		return BrowserTokens{}, ErrInvalid
	}
	if expires, exists := fields["expires_in"]; exists {
		n, err := strconv.ParseInt(string(expires), 10, 32)
		if err != nil || n < 0 {
			return BrowserTokens{}, ErrInvalid
		}
	}
	return BrowserTokens{idToken: values[0], accessToken: values[1]}, nil
}
