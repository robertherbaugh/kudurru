package unifi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/ee/integration/host"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Provider is host-paced; it owns no scheduler or query-path callback.
type Provider struct {
	mu                          sync.RWMutex
	observeMu                   sync.Mutex
	config                      public.UniFiIntegration
	owner                       host.InstanceID
	host                        host.OwnerHost
	options                     Options
	client                      *http.Client
	origin, prefix, bucket, key string
	stop                        chan struct{}
	stopped                     bool
	previous                    map[string]Device
	diagnostics                 Diagnostics
	now                         func() time.Time
	// Test-only trusted roots/transport, never part of public configuration.
	transport *http.Transport
}

// New constructs an unprovisioned provider without network I/O.
func New() *Provider { return NewWithOptions(Options{}) }

// NewWithOptions supplies only root's allocation callback, not transport bypasses.
func NewWithOptions(options Options) *Provider {
	return &Provider{options: options, stop: make(chan struct{}), now: time.Now, previous: map[string]Device{}}
}

// Manifest describes the read-only provider capability and offline schema.
func (p *Provider) Manifest() integration.Manifest { return manifest() }

// Provision captures named credentials and ownership without network I/O.
func (p *Provider) Provision(ctx context.Context, h integration.Host, raw json.RawMessage, secrets integration.SecretResolver) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.host != nil || p.stopped || secrets == nil || len(raw) > 64<<10 {
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
	var config public.UniFiIntegration
	if err := decode(raw, &config, true); err != nil {
		return err
	}
	if err := validateConfig(config); err != nil {
		return err
	}
	key, err := secrets.Resolve(ctx, "apiKey")
	if err != nil || key == "" || len(key) > 4096 || strings.ContainsAny(key, " \t\r\n\x00") {
		return host.ErrCredentials
	}
	origin, prefix, err := endpoint(config.Endpoint)
	if err != nil {
		return err
	}
	client, err := p.httpClient(config, origin)
	if err != nil {
		return err
	}
	if config.Inventory != nil {
		if _, ok := h.(host.SourceHost); !ok {
			return host.ErrUnsupported
		}
	}
	p.config, p.owner, p.host, p.client, p.key = config, owner, ownerHost, client, key
	p.origin, p.prefix = origin, prefix
	p.bucket = kinds.Digest([]byte(origin + prefix))
	return nil
}

// Validate checks the captured configuration without network I/O.
func (p *Provider) Validate(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.host == nil || p.stopped {
		return host.ErrUnavailable
	}
	return validateConfig(p.config)
}

// Check verifies version and exact site identity through guarded requests.
func (p *Provider) Check(ctx context.Context) (integration.Status, error) {
	p.observeMu.Lock()
	defer p.observeMu.Unlock()
	m := &pollBudget{}
	_, _, err := p.identity(ctx, m)
	if err != nil {
		return integration.Status{}, err
	}
	return integration.Status{Reachable: true, Authenticated: true, Message: "Provider is reachable and authenticated."}, nil
}

// Start waits for shutdown; the host schedules all polling.
func (p *Provider) Start(ctx context.Context) error {
	select {
	case <-ctx.Done():
	case <-p.stop:
	}
	return nil
}

// Stop cancels in-flight HTTP requests and closes idle transports.
func (p *Provider) Stop(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.stopped {
		p.stopped = true
		close(p.stop)
	}
	if p.client != nil {
		p.client.CloseIdleConnections()
	}
	return nil
}

// Diagnostics returns a copy of the last complete observation's diagnostics.
func (p *Provider) Diagnostics() Diagnostics {
	p.mu.RLock()
	defer p.mu.RUnlock()
	d := p.diagnostics
	d.LegacyUnavailable = slices.Clone(d.LegacyUnavailable)
	return d
}

// Observe acquires once; failure never commits a newer local observation.
func (p *Provider) Observe(ctx context.Context) (host.SourceSnapshot, error) {
	p.observeMu.Lock()
	defer p.observeMu.Unlock()
	p.mu.RLock()
	ready := p.host != nil && !p.stopped
	config, owner := p.config, p.owner
	p.mu.RUnlock()
	if !ready {
		return host.SourceSnapshot{}, host.ErrUnavailable
	}
	observation, diag, err := p.acquire(ctx)
	if err != nil {
		return host.SourceSnapshot{}, err
	}
	var zone *public.DNSZone
	if config.Inventory != nil {
		zone, err = p.host.(host.SourceHost).SourceZone(ctx, config.Inventory.ZoneRef)
		if err != nil {
			return host.SourceSnapshot{}, err
		}
	}
	in := TranslationInput{Owner: owner, Config: config, Zone: zone, Observation: observation}
	if zone != nil && p.options.Allocations != nil {
		requests, err := NameRequests(in)
		if err != nil {
			return host.SourceSnapshot{}, err
		}
		allocations, err := p.options.Allocations(ctx, owner, zone.DeepCopy(), slices.Clone(requests))
		if err != nil {
			return host.SourceSnapshot{}, err
		}
		in.Allocations = maps.Clone(allocations)
	}
	snapshot, compiled, err := Translate(in)
	if err != nil {
		return host.SourceSnapshot{}, err
	}
	compiled.LegacyUnavailable = slices.Clone(diag.LegacyUnavailable)
	compiled.HistoricalWithoutTime += diag.HistoricalWithoutTime
	if err := ctx.Err(); err != nil {
		return host.SourceSnapshot{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return host.SourceSnapshot{}, host.ErrUnavailable
	}
	p.previous = map[string]Device{}
	for _, d := range observation.Clients {
		d.IPs = slices.Clone(d.IPs)
		p.previous[d.ID] = d
	}
	p.diagnostics = compiled
	return snapshot, nil
}

func validateConfig(c public.UniFiIntegration) error {
	_, prefix, err := endpoint(c.Endpoint)
	if err != nil {
		return err
	}
	if c.Legacy && prefix != "/proxy/network/integration" {
		return host.ErrUnsupported
	}
	if c.Push.DHCPDNSServers || c.Push.MirrorRecords.Enabled {
		return host.ErrUnsupported
	}
	if len(c.Push.MirrorRecords.Types) > 0 {
		return host.ErrUnsupported
	}
	if c.Site != "" && (len(c.Site) > 253 || strings.TrimSpace(c.Site) != c.Site || strings.ContainsAny(c.Site, "/\\\x00\r\n\t")) {
		return ErrInvalid
	}
	if c.Poll.Duration != 0 && (c.Poll.Duration < time.Second || c.Poll.Duration > 8*time.Hour) {
		return ErrInvalid
	}
	if c.TLSFingerprint != "" {
		if _, err := fingerprint(c.TLSFingerprint); err != nil {
			return err
		}
	}
	if len(c.Groups.ByNetwork) > 256 || len(c.Groups.ByNote) > 128 || strings.ContainsAny(c.Groups.ByNote, "\x00\r\n\t ") {
		return ErrInvalid
	}
	for name, group := range c.Groups.ByNetwork {
		if name == "" || len(name) > 253 || !objectName(group) {
			return ErrInvalid
		}
	}
	if c.Inventory != nil {
		v := c.Inventory
		if !objectName(v.ZoneRef.Name) || v.ZoneRef.Namespace != "" || v.TTL < 0 || v.TTL > 86400 || v.ReverseZones != "" && v.ReverseZones != "auto" && v.ReverseZones != "disabled" || len(v.IncludeNetworks) > 256 {
			return ErrInvalid
		}
		if _, err := parseFormatter(v.NameTemplate); err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, n := range v.IncludeNetworks {
			if n == "" || len(n) > 253 || seen[n] {
				return ErrInvalid
			}
			seen[n] = true
		}
	}
	return nil
}
func pollInterval(c public.UniFiIntegration) time.Duration {
	if c.Poll.Duration == 0 {
		return time.Minute
	}
	return c.Poll.Duration
}

func endpoint(raw string) (string, string, error) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 2048 || strings.TrimSpace(raw) != raw || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.RawPath != "" || strings.ContainsAny(u.Host, "\x00\r\n\t ") {
		return "", "", ErrInvalid
	}
	path := strings.TrimSuffix(u.Path, "/")
	if path == "" {
		path = "/proxy/network/integration"
	} else if strings.HasSuffix(path, "/integration/v1") {
		path = strings.TrimSuffix(path, "/v1")
	}
	if path != "/proxy/network/integration" {
		parts := strings.Split(path, "/")
		if len(parts) != 8 || parts[1] != "v1" || parts[2] != "connector" || parts[3] != "consoles" || parts[4] == "" || parts[5] != "proxy" || parts[6] != "network" || parts[7] != "integration" {
			return "", "", ErrInvalid
		}
		for _, r := range parts[4] {
			if !asciiAlphaNumeric(r) && r != ':' && r != '-' && r != '_' {
				return "", "", ErrInvalid
			}
		}
	}
	hostname := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	ip, ipErr := netip.ParseAddr(hostname)
	if ipErr == nil {
		if ip.Zone() != "" {
			return "", "", ErrInvalid
		}
		hostname = ip.String()
	} else if len(validation.IsDNS1123Subdomain(hostname)) != 0 {
		return "", "", ErrInvalid
	}
	port := u.Port()
	if port == "" && strings.HasSuffix(u.Host, ":") {
		return "", "", ErrInvalid
	}
	if port != "" {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || n == 0 {
			return "", "", ErrInvalid
		}
		port = strconv.FormatUint(n, 10)
		if port == "443" {
			port = ""
		}
	}
	u.Host = hostname
	if port != "" {
		u.Host = net.JoinHostPort(hostname, port)
	} else if ipErr == nil && ip.Is6() {
		u.Host = "[" + hostname + "]"
	}
	u.Path, u.RawPath = "", ""
	return u.String(), path, nil
}

func asciiAlphaNumeric(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

func validUUID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id != uuid.Nil && id.String() == s
}

// decode refuses duplicate JSON keys before decoding either spec or wire data.
func decode(raw []byte, out any, strict bool) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	if err := jsonValue(d, 0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return ErrInvalid
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	if strict {
		d.DisallowUnknownFields()
	}
	if err := d.Decode(out); err != nil {
		return ErrInvalid
	}
	return nil
}
func jsonValue(d *json.Decoder, depth int) error {
	if depth > 32 {
		return ErrInvalid
	}
	t, err := d.Token()
	if err != nil {
		return ErrInvalid
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	if delim != '{' && delim != '[' {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for d.More() {
		if delim == '{' {
			key, err := d.Token()
			if err != nil {
				return ErrInvalid
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return ErrInvalid
			}
			seen[name] = true
		}
		if err := jsonValue(d, depth+1); err != nil {
			return err
		}
	}
	end, err := d.Token()
	if err != nil || delim == '{' && end != json.Delim('}') || delim == '[' && end != json.Delim(']') {
		return ErrInvalid
	}
	return nil
}
