package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/ee/integration/plan"
	"github.com/robertherbaugh/kudurru/internal/credentials"
)

// provider retains one negotiated incarnation. The host owns pacing,
// schema/RBAC admission and durable delivery retries; this lifecycle never
// starts a scheduler.
type provider struct {
	mu            sync.RWMutex
	checkSlot     chan struct{}
	life          context.Context
	cancel        context.CancelFunc
	provisioning  bool
	transport     *transport
	owner         host.InstanceID
	advertisement *advertisement
	changed       bool
	hmacKey       []byte
	secrets       []string
	now           func() time.Time
	// records is the captured publication configuration (D380); applyMu
	// serializes one instance's record changes.
	records records
	applyMu sync.Mutex
}

var (
	_ integration.Integration    = (*provider)(nil)
	_ integration.ActionProvider = (*provider)(nil)
	_ host.DeliveryHook          = (*provider)(nil)
	_ host.Observer              = (*provider)(nil)
)

func newProvider() *provider {
	life, cancel := context.WithCancel(context.Background())
	return &provider{life: life, cancel: cancel, now: time.Now, checkSlot: make(chan struct{}, 1)}
}

func (p *provider) Manifest() integration.Manifest { return manifest() }

// Provision resolves only delegated names. A failed named resolution cannot
// be interpreted as absence except for the resolver's explicit ErrDenied.
func (p *provider) Provision(ctx context.Context, h integration.Host, raw json.RawMessage, secrets integration.SecretResolver) error {
	if ctx == nil || secrets == nil || len(raw) > 64<<10 || !objectJSON(raw) {
		return errInput
	}
	p.mu.Lock()
	if p.provisioning || p.transport != nil || p.life.Err() != nil {
		p.mu.Unlock()
		return host.ErrUnavailable
	}
	p.provisioning = true
	p.mu.Unlock()
	// Stop cancels even a held named resolver without waiting on a state lock.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(p.life, cancel)
	defer stop()
	owner, ok := h.(host.OwnerHost)
	if !ok {
		return host.ErrUnsupported
	}
	originalOwner := owner.IntegrationOwner()
	if originalOwner.Validate() != nil {
		return host.ErrUnsupported
	}
	var config public.WebhookIntegration
	if decode(raw, &config) != nil {
		return errInput
	}
	publication, err := publicationConfig(config, originalOwner)
	if err != nil {
		return err
	}
	values := make(map[string][]byte)
	var captured []string
	names := []string{"bearerToken", "ca", "clientCertificate", "clientKey", "hmacKey"}
	for _, name := range names {
		value, err := secrets.Resolve(ctx, name)
		if err != nil {
			if errors.Is(err, credentials.ErrDenied) {
				continue
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return host.ErrCredentials
		}
		if value == "" || len(value) > 1<<20 {
			return host.ErrCredentials
		}
		values[name] = []byte(value)
		captured = append(captured, value)
	}
	key := values["hmacKey"]
	// The same bounds as the conformance command (D379).
	if len(key) != 0 && (len(key) < 32 || len(key) > maxHMACKey) {
		return host.ErrCredentials
	}
	tr, err := newTransport(owner, transportConfig{endpoint: config.Endpoint, account: originalOwner.UID, timeout: config.Timeout.Duration,
		bearer: values["bearerToken"], ca: values["ca"], certificate: values["clientCertificate"], key: values["clientKey"]})
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if ctx.Err() != nil || p.life.Err() != nil {
		tr.close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return host.ErrUnavailable
	}
	p.transport, p.hmacKey, p.secrets, p.owner, p.records = tr, key, captured, originalOwner, publication
	return nil
}

// publicationConfig captures the domains and policy of a publishing
// instance (D380). Domains are canonical names without wildcards; no
// domains means the instance publishes nothing.
func publicationConfig(config public.WebhookIntegration, owner host.InstanceID) (records, error) {
	out := records{policy: plan.PolicySync, account: webhookDestination(config.Endpoint), marker: registryMarker(owner)}
	switch config.Policy {
	case "", "sync":
	case "upsert-only", "create-only":
		out.policy = plan.Policy(config.Policy)
	default:
		return records{}, errInput
	}
	if len(config.Domains) > 256 {
		return records{}, errInput
	}
	for _, d := range config.Domains {
		if d == "" || len(d) > 253 || strings.TrimSpace(d) != d || strings.ContainsAny(d, " *\\\x00\r\n\t") {
			return records{}, errInput
		}
		n := dns.CanonicalName(d)
		if _, ok := dns.IsDomainName(n); !ok || n == "." || slices.Contains(out.domains, n) {
			return records{}, errInput
		}
		if label, _, _ := strings.Cut(n, "."); strings.HasPrefix(label, registryPrefix) {
			return records{}, errInput
		}
		out.domains = append(out.domains, n)
	}
	slices.Sort(out.domains)
	return out, nil
}

func (p *provider) Validate(ctx context.Context) error {
	if ctx == nil {
		return errInput
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.life.Err() != nil || p.transport == nil {
		return host.ErrUnavailable
	}
	if p.changed {
		return host.ErrFenced
	}
	return nil
}

// Check retains the first successfully negotiated identity. Once a different
// peer is observed, only a newly provisioned instance can establish authority.
func (p *provider) Check(ctx context.Context) (integration.Status, error) {
	if ctx == nil {
		return integration.Status{}, errInput
	}
	select {
	case p.checkSlot <- struct{}{}:
		defer func() { <-p.checkSlot }()
	case <-ctx.Done():
		return integration.Status{}, ctx.Err()
	case <-p.life.Done():
		return integration.Status{}, host.ErrUnavailable
	}
	if err := p.Validate(ctx); err != nil {
		return integration.Status{}, err
	}
	p.mu.RLock()
	tr := p.transport
	p.mu.RUnlock()
	a, err := tr.negotiate(ctx)
	if err != nil {
		return integration.Status{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return integration.Status{}, err
	}
	if p.life.Err() != nil {
		return integration.Status{}, host.ErrUnavailable
	}
	if p.advertisement != nil && p.advertisement.digest != a.digest {
		p.changed = true
		return integration.Status{}, host.ErrFenced
	}
	// Hooks need the separate signing key, never the bearer credential.
	if a.has("hooks") && len(p.hmacKey) == 0 {
		return integration.Status{}, host.ErrUnsupported
	}
	// Configured domains need a records peer whose filter covers each of
	// them. A records peer without domains has nothing to publish, so a peer
	// that offers nothing else is refused (D380).
	if len(p.records.domains) != 0 {
		if !a.has("records") {
			return integration.Status{}, host.ErrUnsupported
		}
		for _, d := range p.records.domains {
			if !a.admits(d) {
				return integration.Status{}, host.ErrUnsupported
			}
		}
	} else if slices.Equal(a.declaration.Capabilities, []string{"records"}) {
		return integration.Status{}, host.ErrUnsupported
	}
	raw, err := json.Marshal(a.declaration)
	if err != nil || p.echo(raw) {
		return integration.Status{}, errProtocol
	}
	for _, action := range a.actions {
		raw, err = json.Marshal(action)
		if err != nil || p.echo(raw) {
			return integration.Status{}, errProtocol
		}
	}
	if p.advertisement == nil {
		p.advertisement = a
	}
	return integration.Status{Authenticated: true, Reachable: true, Message: "Provider is reachable and authenticated."}, nil
}

func (p *provider) Start(ctx context.Context) error {
	if err := p.Validate(ctx); err != nil {
		return err
	}
	p.mu.RLock()
	ready := p.advertisement != nil
	p.mu.RUnlock()
	if !ready {
		return host.ErrUnavailable
	}
	select {
	case <-ctx.Done():
	case <-p.life.Done():
	}
	return nil
}

func (p *provider) Stop(context.Context) error {
	p.cancel()
	p.mu.RLock()
	tr := p.transport
	p.mu.RUnlock()
	tr.close()
	return nil
}

func (p *provider) captured(ctx context.Context, capability string) (*transport, *advertisement, error) {
	if err := p.Validate(ctx); err != nil {
		return nil, nil, err
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.changed || p.life.Err() != nil {
		return nil, nil, host.ErrFenced
	}
	if !p.advertisement.has(capability) {
		return nil, nil, host.ErrUnsupported
	}
	return p.transport, p.advertisement, nil
}

func (p *provider) Actions() []integration.ActionSpec {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := []integration.ActionSpec{}
	if p.changed || p.life.Err() != nil || !p.advertisement.has("actions") {
		return out
	}
	for _, a := range p.advertisement.actions {
		a.Params = slices.Clone(a.Params)
		out = append(out, a)
	}
	return out
}

// Invoke sends one already-declared action. The host validates the captured
// schema and original operator permission before calling this private adapter.
func (p *provider) Invoke(ctx context.Context, name string, params json.RawMessage) (json.RawMessage, error) {
	if len(params) > 64<<10 || validJSON(params) != nil {
		return nil, errInput
	}
	tr, a, err := p.captured(ctx, "actions")
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(a.actions, func(action integration.ActionSpec) bool { return action.Name == name }) {
		return nil, host.ErrUnsupported
	}
	r, err := tr.request(ctx, http.MethodPost, "/actions/"+name, a.media, params, nil)
	if err != nil {
		return nil, err
	}
	if _, _, err := p.captured(ctx, "actions"); err != nil {
		return nil, err
	}
	if r.status == http.StatusNoContent && len(r.body) == 0 {
		return json.RawMessage("null"), nil
	}
	if r.status != http.StatusOK || r.media != a.media || len(r.body) > 64<<10 || validJSON(r.body) != nil || p.echo(r.body) {
		return nil, errProtocol
	}
	return r.body, nil
}

// eventTopics are exactly the topics root produces in v0.2 (D379). The
// deferred client.seen, client.left, query.blocked and vip.master_changed
// are not advertised, so no peer subscribes to an event that never comes.
var eventTopics = []string{"zone.changed", "record.changed", "blocking.toggled", "integration.error", "cluster.leader_changed"}

func (p *provider) Topics() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.changed || p.life.Err() != nil || !p.advertisement.has("hooks") {
		return []string{}
	}
	return slices.Clone(eventTopics)
}

// HandleDelivery never invents an ID or retries. The existing durable host
// retains this ID and original body/time across its explicit at-least-once lane.
func (p *provider) HandleDelivery(ctx context.Context, id string, event integration.Event) error {
	if !kinds.ValidDigest(id) || !slices.Contains(eventTopics, event.Topic) || len(event.Payload) > kinds.MaxPayloadBytes || validJSON(event.Payload) != nil || event.Time.IsZero() {
		return errInput
	}
	now := p.now()
	if event.Time.After(now) || !now.Before(event.Time.Add(24*time.Hour)) {
		return errInput
	}
	tr, a, err := p.captured(ctx, "hooks")
	if err != nil {
		return err
	}
	body, err := json.Marshal(event)
	if err != nil || p.echo(body) {
		return errInput
	}
	mac := hmac.New(sha256.New, p.hmacKey)
	_, _ = mac.Write(body)
	_, err = tr.request(ctx, http.MethodPost, "/events", a.media, body, http.Header{
		"X-Kudurru-Delivery": {id}, "X-Kudurru-Signature": {"sha256=" + hex.EncodeToString(mac.Sum(nil))},
	})
	if err != nil {
		return err
	}
	_, _, err = p.captured(ctx, "hooks")
	return err
}

// Named values are immutable after provisioning; this helper does not acquire
// locks and is also safe while Check holds the state lock.
func (p *provider) echo(raw []byte) bool {
	for _, secret := range p.secrets {
		probe := transport{token: secret}
		if probe.containsCredential(raw) {
			return true
		}
	}
	return false
}
