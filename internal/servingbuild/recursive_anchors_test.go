package servingbuild

import (
	"context"
	"crypto"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
	upstreamstage "github.com/robertherbaugh/kudurru/internal/stages/upstream"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/persistence"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/retirement"
	"github.com/robertherbaugh/kudurru/internal/upstream"
	extensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

type managedBuilderFixture struct {
	*fixture
	key    *dns.DNSKEY
	signer crypto.Signer
	clock  *trustanchors.SessionClock
	policy config.RootAnchorPolicy
}

func managedBuilderContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func managedBuilderMessage(t *testing.T, key *dns.DNSKEY, signer crypto.Signer, revoke bool) *dns.Msg {
	t.Helper()
	k := *key
	if revoke {
		k.Flags |= dns.REVOKE
	}
	now := time.Now()
	sig := &dns.RRSIG{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeRRSIG, Class: dns.ClassINET, Ttl: 60}, Algorithm: k.Algorithm, KeyTag: k.KeyTag(), SignerName: ".", Inception: uint32(now.Add(-time.Minute).Unix() & 0xffffffff), Expiration: uint32(now.Add(time.Hour).Unix() & 0xffffffff)}
	if err := sig.Sign(signer, []dns.RR{&k}); err != nil {
		t.Fatal(err)
	}
	q := new(dns.Msg)
	q.SetQuestion(".", dns.TypeDNSKEY)
	m := new(dns.Msg)
	m.SetReply(q)
	m.Authoritative = true
	m.Answer = []dns.RR{&k, sig}
	return m
}

func (f *managedBuilderFixture) settings(t *testing.T, mode config.RootAnchorMode, seeds []string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"groups":    map[string]any{"default": map[string]any{"mode": "recursive", "servers": []string{}}, "forward": map[string]any{"servers": []string{"192.0.2.53"}}},
		"recursion": map[string]any{"anchorMode": mode, "dnssec": true, "ipv6": "off", "trustAnchors": seeds},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var settings public.Kudurru
		if err := tx.Get(ctx, store.Key{Kind: "Kudurru", Name: "default"}, &settings); err != nil {
			return err
		}
		settings.Spec.Settings["upstreams"] = extensions.JSON{Raw: raw}
		settings.Spec.Settings["dnssec"] = extensions.JSON{Raw: []byte(`{"enabled":true,"useBuiltinAnchors":true}`)}
		_, err := tx.Apply(ctx, &settings, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	var persisted public.Kudurru
	if err := f.store.Get(t.Context(), store.Key{Kind: "Kudurru", Name: "default"}, &persisted); err != nil {
		t.Fatal(err)
	}
	var actual struct {
		Recursion struct {
			AnchorMode   config.RootAnchorMode `json:"anchorMode"`
			TrustAnchors []string              `json:"trustAnchors"`
		} `json:"recursion"`
	}
	if err := json.Unmarshal(persisted.Spec.Settings["upstreams"].Raw, &actual); err != nil || actual.Recursion.AnchorMode != mode || !reflect.DeepEqual(actual.Recursion.TrustAnchors, seeds) {
		t.Fatal("actual settings schema pruned or changed anchor policy", err)
	}
}

func newManagedBuilderFixture(t *testing.T, bootstrap bool) *managedBuilderFixture {
	t.Helper()
	f := &managedBuilderFixture{fixture: newFixture(t)}
	f.stop() // retain the real initialized Store; later publishers are explicit.
	f.key = &dns.DNSKEY{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: 60}, Flags: dns.ZONE | dns.SEP, Protocol: 3, Algorithm: dns.ED25519}
	private, err := f.key.Generate(256)
	if err != nil {
		t.Fatal(err)
	}
	f.signer = private.(crypto.Signer)
	other := *f.key
	if _, err := other.Generate(256); err != nil {
		t.Fatal(err)
	}
	// The second explicit seed is absent from the received root set. It must
	// remain configuration provenance without becoming an effective live key.
	policy, err := (config.Recursion{AnchorMode: config.RootAnchorsManaged, DNSSEC: true, TrustAnchors: []string{f.key.ToDS(dns.SHA256).String(), other.ToDS(dns.SHA256).String()}}).RootAnchorPolicy()
	if err != nil {
		t.Fatal(err)
	}
	f.policy = policy
	f.settings(t, config.RootAnchorsManaged, policy.Seeds)
	ctx := managedBuilderContext(t)
	writer, err := f.store.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	ctx = sqlite.WithWriterToken(ctx, writer)
	if _, err := f.store.BindAnchorLifecycle(ctx); err != nil {
		t.Fatal(err)
	}
	f.clock, err = trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	f.builder.opts.AnchorStore = f.store
	if !bootstrap {
		return f
	}
	image, err := f.store.ReadGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := accepted.DecodeRows(image.Payload.Runtime, image.Term.ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	original, err := persistence.CaptureTransition(ctx, owner.Version(), f.clock)
	if err != nil {
		t.Fatal(err)
	}
	update, err := f.store.CaptureAnchorUpdate(ctx, owner.Version())
	if err != nil {
		t.Fatal(err)
	}
	state, observation, err := trustanchors.Bootstrap(trustanchors.Seed{ClusterID: image.Term.ClusterID, PolicyID: policy.PolicyID, DS: policy.Seeds}, managedBuilderMessage(t, f.key, f.signer, false), f.clock.Sample())
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.store.AnchorCommand(update, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := persistence.Bootstrap(ctx, tx, original, state, observation)
		return err
	})
	if err != nil || result.Outcome() != sqlite.AnchorCommitted {
		t.Fatal("actual managed bootstrap failed", err)
	}
	return f
}

func (f *managedBuilderFixture) semantic(t *testing.T) *semanticOwners {
	t.Helper()
	ctx := managedBuilderContext(t)
	image, token, err := f.store.ReadServingCandidate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	deps, err := f.builder.opts.Capture(ctx, image)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(deps.Release)
	owners, err := prepareSemantic(ctx, serving.Candidate{Image: image, Token: token}, f.builder.opts, deps, f.now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owners.release)
	return owners
}

func managedBuilderPlan(t *testing.T, owners *semanticOwners, group string) *upstream.QueryPlan {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion("www.example.", dns.TypeA)
	validator, err := owners.config.DNSSEC.Validator(nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := owners.upstreams.Plan(&pipeline.Request{Msg: m, UpstreamGroup: group}, validator, owners.config.DNSSEC.Enabled, 1, owners.current)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestManagedBuilderBindsEffectiveKeysAndFreshRecursiveDomains(t *testing.T) {
	f := newManagedBuilderFixture(t, true)
	a, b := f.semantic(t), f.semantic(t)
	if len(a.config.Upstreams.Recursion.TrustAnchors) != 2 || len(a.anchor.Projection().DS()) != 1 {
		t.Fatal("fixture did not distinguish seeds from accepted keys")
	}
	if !reflect.DeepEqual(a.upstreams.Recursor().Config().TrustAnchors, a.anchor.Projection().DS()) || a.upstreams.Recursor() == b.upstreams.Recursor() {
		t.Fatal("native resolver reused seeds or mutable instance")
	}
	if a.upstreams.Identity() != b.upstreams.Identity() {
		t.Fatal("equal semantic settings fixture differs")
	}
	x, y := managedBuilderPlan(t, a, "default"), managedBuilderPlan(t, b, "default")
	if !x.Recursive() || !y.Recursive() || x.Scope().ID == y.Scope().ID {
		t.Fatal("equal-owner new Input revived managed cache identity")
	}
	alias := new(dns.Msg)
	alias.SetQuestion("alias.example.", dns.TypeA)
	derived, err := x.ForCache().Derive(&pipeline.Request{Msg: alias})
	if err != nil {
		t.Fatal(err)
	}
	if derived.Scope() != x.Scope() || !derived.Current() {
		t.Fatal("derivation lost original recursive identity")
	}
	fx, fy := managedBuilderPlan(t, a, "forward"), managedBuilderPlan(t, b, "forward")
	if fx.Recursive() || fy.Recursive() || !fx.Validates() || !fy.Validates() || fx.Scope().ID != fy.Scope().ID || fx.Validator().ActiveContextID() != fy.Validator().ActiveContextID() {
		t.Fatal("managed input changed independent forwarded scope")
	}
	if _, err := a.anchor.Input().CaptureRead(managedBuilderContext(t)); !errors.Is(err, sqlite.ErrAnchorUnavailable) {
		t.Fatal("builder preparation granted publication", err)
	}
}

func TestManagedBuilderActualPublicationAndLocalDenyCurrency(t *testing.T) {
	f := newManagedBuilderFixture(t, true)
	prepared := make(chan serving.Prepared, 1)
	pub, err := serving.New(serving.Options{Source: f.store, Required: serving.CompleteManifest(), PreparationTimeout: 20 * time.Second, Prepare: func(ctx context.Context, c serving.Candidate) (serving.Prepared, error) {
		p, err := f.builder.Prepare(ctx, c)
		if err == nil {
			select {
			case prepared <- p:
			default:
			}
		}
		return p, err
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = pub.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done; pub.Close() })
	select {
	case <-pub.Initialized():
	case <-time.After(10 * time.Second):
		t.Fatal("managed complete publisher did not initialize", pub.Status())
	}
	p := <-prepared
	if p.Anchor == nil || !p.Current() {
		t.Fatal("complete prepared source not bound")
	}
	lease, err := pub.CaptureHTTP(managedBuilderContext(t))
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	input := p.Anchor.Input()
	read, err := input.CaptureRead(managedBuilderContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := input.WithRead(read, func(scope sqlite.AnchorReadScope) error { return scope.Check() }); err != nil {
		t.Fatal(err)
	}
	stage := upstreamstage.NewStage(nil)
	stage.Capture = func(req *pipeline.Request) (*upstreamstage.View, error) {
		return servingdns.Component[*upstreamstage.View](req, "resolution")
	}
	m := new(dns.Msg)
	m.SetQuestion("www.example.", dns.TypeA)
	plan, err := stage.Bind(&pipeline.Request{Ctx: managedBuilderContext(t), Msg: m, Serving: servingdns.Wrap(lease)})
	if err != nil || !plan.Current() {
		t.Fatal("actual resolution owner not current", err)
	}
	original := managedBuilderContext(t)
	before, err := f.store.ReadGeneration(original)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := accepted.DecodeRows(before.Payload.Runtime, before.Term.ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	denial, err := f.store.CaptureAnchorDenial(original, owner.Version(), f.clock)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := denial.Verify(managedBuilderMessage(t, f.key, f.signer, true))
	if err != nil {
		t.Fatal(err)
	}
	result, _, err := f.store.CommitAnchorDenial(verified)
	if err != nil || result.Outcome() != sqlite.AnchorCommitted {
		t.Fatal("actual local denial failed", err)
	}
	after, err := f.store.ReadGeneration(original)
	if err != nil {
		t.Fatal(err)
	}
	if !before.Generation().Same(after.Generation()) {
		t.Fatal("local deny invented canonical progress")
	}
	if p.Current() || plan.Current() || input.Current() {
		t.Fatal("local-only denial missed complete or resolution currency")
	}
	refused, err := pub.CaptureHTTP(original)
	refused.Release()
	if err == nil {
		t.Fatal("protected capture ignored denied managed input")
	}
}

func TestManagedBuilderRefusesUnusableOrMismatchedHistory(t *testing.T) {
	for _, kind := range []string{"awaiting", "missing-store", "foreign-store", "static-downgrade", "changed-seeds", "unbounded-original"} {
		t.Run(kind, func(t *testing.T) {
			f := newManagedBuilderFixture(t, kind != "awaiting")
			if kind == "missing-store" {
				f.builder.opts.AnchorStore = nil
			}
			if kind == "foreign-store" {
				other := newFixture(t)
				f.builder.opts.AnchorStore = other.store
			}
			if kind == "static-downgrade" {
				f.settings(t, config.RootAnchorsStatic, nil)
			}
			if kind == "changed-seeds" {
				f.settings(t, config.RootAnchorsManaged, f.policy.Seeds[:1])
			}
			ctx := managedBuilderContext(t)
			if kind == "unbounded-original" {
				ctx = t.Context()
			}
			image, token, err := f.store.ReadServingCandidate(ctx)
			if err != nil {
				t.Fatal(err)
			}
			p, err := f.builder.Prepare(ctx, serving.Candidate{Image: image, Token: token})
			for _, component := range p.Components {
				if component.Release != nil {
					component.Release()
				}
			}
			want := sqlite.ErrAnchorUnavailable
			switch kind {
			case "foreign-store":
				want = replication.ErrInvalid
			case "unbounded-original":
				want = retirement.ErrInvalid
			}
			if !errors.Is(err, want) || p.Anchor != nil {
				t.Fatal("unusable source revealed static/default resolver", kind, err)
			}
		})
	}
}
