// Package cloudflare is the elected Cloudflare SDK publication adapter. It
// never runs on DNS paths and never reads credentials outside named capabilities.
package cloudflare

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	sdk "github.com/cloudflare/cloudflare-go/v7/dns"
	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/cloudflare/cloudflare-go/v7/user"
	"github.com/cloudflare/cloudflare-go/v7/zones"
	"github.com/miekg/dns"
	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/ee/integration/plan"
)

const baseURL = "https://api.cloudflare.com/client/v4/"
const maxRecords = 10000
const maxPages = 32
const pageSize = 500

// Configuration, complete-observation and remote-conflict errors.
var (
	ErrInvalid = errors.New("cloudflare: invalid configuration or observation")
	ErrDrift   = errors.New("cloudflare: destination changed or ownership conflicts")
)

type resolvedZone struct {
	config       public.CloudflareZone
	apex         string
	destination  kinds.Destination
	commentLimit int
	minTTL       int64
}

// Provider uses the official SDK services and the host's original request scope.
// New does no I/O; the registered constructor cannot change the API origin.
type Provider struct {
	mu           sync.RWMutex
	applyMu      sync.Mutex
	config       public.CloudflareIntegration
	owner        host.InstanceID
	host         host.OwnerHost
	records      *sdk.RecordService
	zones        *zones.ZoneService
	tokens       *user.TokenService
	destinations map[kinds.Destination]resolvedZone
	client       *http.Client
	stopped      atomic.Bool
	stop         chan struct{}
	now          func() time.Time
	lastBatch    time.Time
	// Internal test injection only, absent from public config/registry constructor.
	apiURL       string
	roundTripper http.RoundTripper
	cadence      time.Duration
}

// New constructs a provider without I/O or live configuration access.
func New() *Provider {
	return &Provider{apiURL: baseURL, now: time.Now, cadence: time.Second, stop: make(chan struct{})}
}

// Manifest reports the registered type, schema and named secret capability.
func (p *Provider) Manifest() integration.Manifest { return manifest() }

// Provision binds immutable configuration, owner and explicit named token.
func (p *Provider) Provision(ctx context.Context, h integration.Host, raw json.RawMessage, secrets integration.SecretResolver) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.host != nil || p.stopped.Load() || secrets == nil {
		return ErrInvalid
	}
	ownerHost, ok := h.(host.OwnerHost)
	if !ok {
		return host.ErrUnsupported
	}
	owner := ownerHost.IntegrationOwner()
	if owner.Validate() != nil {
		return ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	var config public.CloudflareIntegration
	if err := d.Decode(&config); err != nil {
		return ErrInvalid
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return ErrInvalid
	}
	token, err := secrets.Resolve(ctx, "apiToken")
	if err != nil || token == "" || strings.TrimSpace(token) != token || strings.ContainsAny(token, "\x00\r\n\t ") {
		return host.ErrCredentials
	}
	p.config = config
	p.owner = owner
	p.host = ownerHost
	transport := p.roundTripper
	if transport == nil {
		base := http.DefaultTransport.(*http.Transport).Clone()
		// A reused HTTP/1 connection or HTTP/2 stream can be retried internally by
		// net/http beneath RoundTrip. One fresh HTTP/1 connection per host charge
		// makes the request budget and mutation fence cover each actual attempt.
		base.DisableKeepAlives = true
		base.ForceAttemptHTTP2 = false
		base.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
		transport = base
	}
	client := &http.Client{Transport: &requestTransport{host: ownerHost, base: transport, origin: p.apiURL, token: token}, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	opts := []option.RequestOption{option.WithBaseURL(p.apiURL), option.WithHTTPClient(client), option.WithAPIToken(token), option.WithAPIKey(""), option.WithAPIEmail(""), option.WithUserServiceKey(""), option.WithMaxRetries(0), option.WithRequestTimeout(30 * time.Second)}
	p.client = client
	p.records = sdk.NewRecordService(opts...)
	p.zones = zones.NewZoneService(opts...)
	p.tokens = user.NewTokenService(opts...)
	p.destinations = map[kinds.Destination]resolvedZone{}
	return nil
}

// Validate checks configuration offline without provider or credential I/O.
func (p *Provider) Validate(context.Context) error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.host == nil || len(p.config.Zones) == 0 || len(p.config.Zones) > 256 {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, z := range p.config.Zones {
		name, err := canonical(z.Zone)
		if err != nil || seen[name] || z.ZoneID != "" && !validID(z.ZoneID) {
			return ErrInvalid
		}
		seen[name] = true
		if z.Policy != "" && z.Policy != "sync" && z.Policy != "upsert-only" && z.Policy != "create-only" || z.Defaults.TTL < 0 || z.Defaults.TTL > 86400 || z.Defaults.TTL > 1 && z.Defaults.TTL < 30 || len(z.Defaults.Comment) > 500 || strings.ContainsFunc(z.Defaults.Comment, unicode.IsControl) || strings.Contains(z.Defaults.Comment, markerPrefix) {
			return ErrInvalid
		}
		if z.AdoptFrom != nil && (z.AdoptFrom.ExternalDNSOwner == "" || len(z.AdoptFrom.ExternalDNSOwner) > 253 || strings.ContainsFunc(z.AdoptFrom.ExternalDNSOwner, unicode.IsControl)) {
			return ErrInvalid
		}
		if _, err := plan.Select(plan.SelectionInput{Owner: p.owner, Select: z.Select, Domains: []string{name}, Policy: plan.Policy(z.Policy), Complete: true, Now: p.now()}); err != nil {
			return ErrInvalid
		}
		if _, err := translations(z.Translate); err != nil {
			return ErrInvalid
		}
	}
	return nil
}
func (p *Provider) available(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.stopped.Load() {
		return host.ErrUnavailable
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.host == nil {
		return ErrInvalid
	}
	return nil
}

// Check verifies the named token and exact configured destination access.
func (p *Provider) Check(ctx context.Context) (integration.Status, error) {
	if err := p.available(ctx); err != nil {
		return integration.Status{}, err
	}
	var env envelope[user.TokenVerifyResponse]
	_, err := p.tokens.Verify(ctx, option.WithResponseBodyInto(&env))
	if err != nil {
		return integration.Status{}, safe(ctx, err)
	}
	if env.valid() != nil || env.Result.Status != user.TokenVerifyResponseStatusActive {
		return integration.Status{Reachable: true}, &host.Failure{Kind: host.FailurePermanent}
	}
	if _, err := p.Destinations(ctx); err != nil {
		return integration.Status{}, err
	}
	return integration.Status{Authenticated: true, Reachable: true, Message: "Provider is reachable and authenticated."}, nil
}

// Start waits for cancellation; polling and retries belong to the host.
func (p *Provider) Start(ctx context.Context) error {
	select {
	case <-ctx.Done():
	case <-p.stop:
	}
	return nil
}

// Stop prevents new callbacks and closes idle transport resources.
func (p *Provider) Stop(context.Context) error {
	if p.stopped.CompareAndSwap(false, true) {
		close(p.stop)
		p.mu.RLock()
		defer p.mu.RUnlock()
		if p.client != nil {
			p.client.CloseIdleConnections()
		}
	}
	return nil
}

// Destinations resolves and verifies immutable account/zone identities.
func (p *Provider) Destinations(ctx context.Context) ([]host.Publication, error) {
	if err := p.available(ctx); err != nil {
		return nil, err
	}
	p.mu.RLock()
	config := p.config
	p.mu.RUnlock()
	var out []host.Publication
	resolved := map[kinds.Destination]resolvedZone{}
	for _, z := range config.Zones {
		apex, err := canonical(z.Zone)
		if err != nil {
			return nil, ErrInvalid
		}
		var zone zones.Zone
		if z.ZoneID != "" {
			var env envelope[zones.Zone]
			params := zones.ZoneGetParams{}
			params.ZoneID.Value, params.ZoneID.Present = z.ZoneID, true
			_, err = p.zones.Get(ctx, params, option.WithResponseBodyInto(&env))
			if err == nil {
				err = env.valid()
			}
			zone = env.Result
		} else {
			var env envelope[[]zones.Zone]
			params := zones.ZoneListParams{}
			params.Name.Value, params.Name.Present = strings.TrimSuffix(apex, "."), true
			params.PerPage.Value, params.PerPage.Present = 50, true
			params.Page.Value, params.Page.Present = 1, true
			_, err = p.zones.List(ctx, params, option.WithResponseBodyInto(&env))
			if err == nil {
				err = env.valid()
			}
			if err == nil {
				err = env.ResultInfo.valid(1, 50, len(env.Result))
			}
			if err == nil && (len(env.Result) != 1 || *env.ResultInfo.TotalCount != 1) {
				err = ErrInvalid
			}
			if err == nil {
				zone = env.Result[0]
			}
		}
		if err != nil {
			return nil, safe(ctx, err)
		}
		if !validID(zone.ID) || !validID(zone.Account.ID) || dns.CanonicalName(zone.Name) != apex || z.ZoneID != "" && zone.ID != z.ZoneID {
			return nil, ErrInvalid
		}
		destination := kinds.Destination{Provider: "cloudflare", Account: zone.Account.ID, Zone: zone.ID}
		if _, exists := resolved[destination]; exists {
			return nil, ErrInvalid
		}
		limit := 100
		minTTL := int64(60)
		//nolint:staticcheck // SDK deprecates changing Plan; this reads returned capabilities without requesting billing permissions.
		switch strings.ToLower(zone.Plan.Name) {
		case "pro", "pro website", "business", "business website":
			limit = 500
		case "enterprise", "enterprise website":
			limit = 500
			minTTL = 30
		}
		if !z.Defaults.Proxied && z.Defaults.TTL > 1 && z.Defaults.TTL < minTTL {
			return nil, ErrInvalid
		}
		if len(stampComment(z.Defaults.Comment, p.owner)) > limit {
			return nil, ErrInvalid
		}
		resolved[destination] = resolvedZone{config: z, apex: apex, destination: destination, commentLimit: limit, minTTL: minTTL}
		pub := host.Publication{Destination: destination, Domains: []string{apex}, Selection: *z.Select.DeepCopy(), Policy: plan.Policy(z.Policy), AdoptUnowned: z.AdoptUnowned}
		if z.AdoptFrom != nil {
			pub.AdoptExternalDNSOwner = z.AdoptFrom.ExternalDNSOwner
		}
		out = append(out, pub)
	}
	p.mu.Lock()
	p.destinations = resolved
	p.mu.Unlock()
	return out, nil
}
func (p *Provider) destination(d kinds.Destination) (resolvedZone, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	z, ok := p.destinations[d]
	if !ok {
		return resolvedZone{}, ErrInvalid
	}
	return z, nil
}
func canonical(name string) (string, error) {
	if name == "" || strings.TrimSpace(name) != name || strings.ContainsAny(name, " *\\\x00\r\n\t") || strings.ContainsFunc(name, func(r rune) bool { return r > 127 || unicode.IsControl(r) }) {
		return "", ErrInvalid
	}
	n := dns.CanonicalName(name)
	if _, ok := dns.IsDomainName(n); !ok || n == "." {
		return "", ErrInvalid
	}
	return n, nil
}
func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if c < '0' || c > '9' {
			if c < 'a' || c > 'f' {
				return false
			}
		}
	}
	return true
}

var _ integration.Integration = (*Provider)(nil)
