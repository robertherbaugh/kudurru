package network_test

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"path/filepath"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
	"github.com/robertherbaugh/kudurru/internal/listdata/catalogue"
	"github.com/robertherbaugh/kudurru/internal/listfetcher"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/currentguard"
	network "github.com/robertherbaugh/kudurru/internal/primaryruntime/network"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/sqliteadapter"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func ring(t *testing.T, value string) *dnsserver.StaticKeyring {
	t.Helper()
	k, err := dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: "key.", Algorithm: dns.HmacSHA256, Secret: []byte(value)})
	check(t, err)
	return k
}

type fixture struct {
	t                  *testing.T
	s                  *sqlite.Store
	path               string
	keys               *servingbuild.KeyringOwner
	pub                *serving.Publisher
	publication        *servingbuild.PrimaryPublication
	sender             *network.Sender
	a                  *sqliteadapter.Adapter
	r                  *pr.Runtime
	options            pr.Options
	repo               *zonestate.Repository
	zone               *public.DNSZone
	l                  *zk.ZoneLifecycle
	fence              pr.PublicationFence
	issuer             *nativeIssuer
	blocked            atomic.Bool
	invalidateFinal    atomic.Bool
	publicationTimeout atomic.Int64
}

func newFixture(t *testing.T, policy public.ZonePolicy) *fixture {
	t.Helper()
	return newNotifyFixture(t, policy, ring(t, "test-original-secret"), netip.Addr{})
}

func newNotifyFixture(t *testing.T, policy public.ZonePolicy, keys *dnsserver.StaticKeyring, notifySource netip.Addr) *fixture {
	t.Helper()
	f := &fixture{t: t, path: filepath.Join(t.TempDir(), "state.db")}
	scheme, err := runtimekinds.NewScheme()
	check(t, err)
	v, err := validator.New()
	check(t, err)
	var private []store.Kind
	for _, k := range runtimekinds.Private() {
		private = append(private, store.Kind(k))
	}
	f.s, err = sqlite.Open(f.path, sqlite.Options{Scheme: scheme, Validator: v, InternalKinds: private, ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
	check(t, err)
	t.Cleanup(func() { check(t, f.s.Close()) })
	f.fence = pr.PublicationFence{Term: f.s.Authority().Term, Incarnation: f.s.Authority().Incarnation}
	check(t, f.s.Command(f.ctx(), f.fence.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return runtimeowners.Initialize(ctx, tx, time.Now().UTC())
	}))
	_, err = f.s.Apply(f.ctx(), &public.Kudurru{ObjectMeta: metav1.ObjectMeta{Name: "default"}}, store.ApplyOptions{Manager: store.ManagerSystem})
	check(t, err)
	schema, err := servingbuild.NewSchema(scheme, v, runtimeowners.ValidateEntry, runtimeowners.ValidateImage)
	check(t, err)
	host, err := config.FromFlat(config.Defaults())
	check(t, err)
	permissions, err := rbac.LoadPermissions()
	check(t, err)
	cache, err := listfetcher.OpenDiskCache(t.TempDir(), nil)
	check(t, err)
	f.keys, err = servingbuild.NewKeyringOwner(keys)
	check(t, err)
	builder, err := servingbuild.New(servingbuild.Options{Schema: schema, Lists: cache, Authority: f.s.ServingAuthority, Capture: func(context.Context, *replication.Image) (servingbuild.Dependencies, error) {
		cut := f.keys.Capture()
		return servingbuild.Dependencies{Identity: "test-host/" + cut.Identity(), Host: host, Catalog: servingbuild.CatalogResource{Catalogue: catalogue.Embedded(), Source: host.Blocking.Catalogue}, Keys: cut.Keys(), KeyringIdentity: cut.Identity(), Current: func() bool { return cut.Current() && !f.blocked.Load() }, Auth: servingauth.Dependencies{ConfigProviders: []servingauth.ProviderSource{}, ProviderResources: []servingauth.ProviderResource{}, Identity: "test-auth", ExpectedIdentity: "test-auth", Permissions: permissions, License: servingauth.LicensePolicy{RBAC: true}, CurrentKey: bytes.Repeat([]byte{3}, 32)}}, nil
	}})
	check(t, err)
	source, err := f.keys.GuardSource(f.s)
	check(t, err)
	f.pub, err = serving.New(serving.Options{Source: source, Required: serving.CompleteManifest(), Prepare: builder.Prepare, DependencyChanges: f.keys.Changes, RetryInterval: time.Millisecond})
	check(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.pub.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
		f.pub.Close()
	})
	select {
	case <-f.pub.Initialized():
	case <-time.After(10 * time.Second):
		t.Fatal("complete publisher failed", f.pub.Status())
	}
	f.publication, err = servingbuild.NewPrimaryPublication(f.pub, nil)
	check(t, err)
	f.sender, err = network.NewNotify(network.NotifyOptions{Capture: f.captureNotify, SourceAddr: notifySource})
	check(t, err)
	basePin := f.keys.PrimaryPin(f.pub)
	guard, err := currentguard.New(currentguard.Options{Pin: func(ctx context.Context, w pr.PublicationFence, fn func(currentguard.Pinned) error) error {
		return basePin(ctx, w, func(p currentguard.Pinned) error {
			validate := f.sender.Validator(p.Identity, p.Keys)
			p.Notify = func(ctx context.Context, tx *sqlite.CommandTx, c sqliteadapter.BackgroundCheck) error {
				if err := validate(ctx, tx, c); err != nil {
					return err
				}
				if c.Reason == "notify-ack" && f.invalidateFinal.Load() {
					f.blocked.Store(true)
				}
				return nil
			}
			return fn(p)
		})
	}})
	check(t, err)
	f.a, err = sqliteadapter.New(sqliteadapter.Options{Store: f.s, Background: guard.Background(), Credentials: guard.Credentials()})
	check(t, err)
	backend, err := f.s.ZoneStateBackend(zonestate.Limits{})
	check(t, err)
	f.repo = zonestate.NewRepository(backend, zonestate.Limits{})
	f.issuer = &nativeIssuer{f: f}
	f.options = pr.Options{Store: f.s, Repository: f.repo, Reader: primaryzone.NewReader(f.s, primaryzone.ReaderOptions{Policy: guard.CapturePolicy}), LifecycleCAS: f.a.LifecycleCAS, Admit: f.a.Admission, Proofs: f.issuer, Publish: func(ctx context.Context, c pr.Cut, m pr.Member) error {
		if budget := time.Duration(f.publicationTimeout.Load()); budget > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, budget)
			defer cancel()
		}
		return f.publication.Publish(ctx, c, m)
	}, Notify: f.sender, Timeout: 5 * time.Second}
	f.r, err = pr.New(f.options)
	check(t, err)
	policy.QueryAccess, policy.Update = public.AccessAllow, public.AccessAllow
	policy.UpdatePolicies = []public.UpdatePolicy{{TSIGKeyRef: "key.", Name: "*.example."}}
	saved, err := f.s.Apply(f.ctx(), &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "primary"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: "example.", Nameservers: []string{"ns.example."}, Policy: policy}}, store.ApplyOptions{Manager: store.ManagerSystem})
	check(t, err)
	f.zone = saved.(*public.DNSZone)
	f.l, err = pr.NewLifecycle(f.fence.Term.ClusterID, f.zone, nil)
	check(t, err)
	f.l, err = f.r.Transition(f.ctx(), nil, f.l)
	check(t, err)
	_, err = f.r.Reconcile(f.ctx(), f.l.Spec.Zone)
	check(t, err)
	f.l, err = f.r.ReadLifecycle(f.ctx(), f.l.Spec.Zone)
	check(t, err)
	return f
}

func (f *fixture) ctx() context.Context { return pr.WithPublicationFence(f.t.Context(), f.fence) }
func (f *fixture) view() *zonestate.View {
	f.t.Helper()
	v, err := f.repo.Read(f.ctx(), f.l.Spec.HeadZone())
	check(f.t, err)
	return v
}

func (f *fixture) captureNotify(ctx context.Context, w pr.PublicationFence, m pr.Member) (network.NotifyCut, error) {
	lease, err := f.pub.CaptureHTTP(ctx)
	if err != nil {
		lease.Release()
		return network.NotifyCut{}, err
	}
	cut := network.NotifyCut{Release: lease.Release}
	target := serving.TermActivation{Term: w.Term, Incarnation: w.Incarnation}
	if err := f.pub.CheckWriterLease(ctx, lease, target); err != nil {
		return cut, err
	}
	value, ok := lease.Bundle().Component("runtime")
	if !ok {
		return cut, pr.ErrUnavailable
	}
	owner := value.(*servingbuild.RuntimeOwner)
	actual, ok := owner.Primary().DurableMember(m.Zone)
	want := m
	want.Published = false
	if !ok || !reflect.DeepEqual(actual, want) || !owner.Primary().Admitted(m.Zone.Apex, time.Now()) {
		return cut, pr.ErrUnavailable
	}
	route := owner.Zones().Zones(time.Now()).Match(m.Zone.Apex)
	if route == nil || route.Name() != m.Zone.Apex || !route.AvailableAt(time.Now()) {
		return cut, pr.ErrUnavailable
	}
	cut.Identity, cut.Keys, cut.Current = owner.NotifyKeys()
	dependencyCurrent := cut.Current
	cut.Current = func() bool {
		return dependencyCurrent != nil && dependencyCurrent() && owner.Primary().Admitted(m.Zone.Apex, time.Now()) && f.pub.CheckWriterLease(ctx, lease, target) == nil
	}
	return cut, f.pub.CheckWriterLease(ctx, lease, target)
}

func (f *fixture) change() {
	f.t.Helper()
	_, err := f.s.Apply(f.ctx(), &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "change", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: f.zone.Name}, Endpoints: []public.Endpoint{{DNSName: "host.example.", RecordType: "A", RecordTTL: 60, Targets: []string{"192.0.2.1"}}}}}, store.ApplyOptions{Manager: store.ManagerSystem})
	check(f.t, err)
	_, err = f.r.Reconcile(f.ctx(), f.l.Spec.Zone)
	check(f.t, err)
	f.l, err = f.r.ReadLifecycle(f.ctx(), f.l.Spec.Zone)
	check(f.t, err)
	if f.view().Head.Spec.NotifySequence == 0 {
		f.t.Fatal("source change did not persist intent")
	}
}

func (f *fixture) bound() (context.Context, func(), pr.Member, *zone.Zone) {
	f.t.Helper()
	cut := f.r.Capture(time.Now())
	m, ok := cut.Member(f.zone.Spec.ZoneName)
	if !ok {
		f.t.Fatal("missing member")
	}
	z := cut.Match(m.Zone.Apex)
	ctx, release, err := f.sender.Bind(f.ctx(), m, z)
	check(f.t, err)
	return ctx, release, m, z
}

// Explicit TEST-ONLY native issuer: actual VerifiedTSIGFor is required. Root's
// private cmd issuer remains the production constructor/transport authority.
type nativeIssuer struct {
	f     *fixture
	last  *nativeIssued
	retry atomic.Uint64
}
type nativeIssued struct {
	owner    *nativeIssuer
	req      pipeline.Request
	wire     []byte
	identity pr.ProofIdentity
}

func (p *nativeIssuer) capture(req *pipeline.Request, apex string) (pr.VerifiedUpdate, error) {
	if apex != p.f.zone.Spec.ZoneName {
		return pr.VerifiedUpdate{}, &network.UpdateReject{Rcode: dns.RcodeNotAuth}
	}
	lease, ok := req.Ctx.Value(requestLeaseKey{}).(*serving.Lease)
	if !ok {
		return pr.VerifiedUpdate{}, pr.ErrUnavailable
	}
	target := serving.TermActivation{Term: p.f.fence.Term, Incarnation: p.f.fence.Incarnation}
	if err := p.f.pub.CheckWriterLease(req.Ctx, lease, target); err != nil {
		return pr.VerifiedUpdate{}, err
	}
	value, found := lease.Bundle().Component("runtime")
	if !found {
		return pr.VerifiedUpdate{}, pr.ErrUnavailable
	}
	owner := value.(*servingbuild.RuntimeOwner)
	m, admitted := owner.Primary().Cut(time.Now()).Member(apex)
	if !admitted || m.Zone != p.f.l.Spec.Zone || m.Type != public.ZoneTypePrimary || !owner.Primary().Admitted(apex, time.Now()) {
		return pr.VerifiedUpdate{}, pr.ErrUnavailable
	}
	proof, ok := dnsserver.VerifiedTSIGFor(req, time.Now())
	if !ok {
		return pr.VerifiedUpdate{}, pr.ErrUnavailable
	}
	msg := proof.Message()
	msg.Compress = false
	wire, err := msg.Pack()
	if err != nil {
		return pr.VerifiedUpdate{}, err
	}
	digest, err := pr.SemanticDigest(wire)
	if err != nil {
		return pr.VerifiedUpdate{}, err
	}
	id := proof.Identity()
	copied := *req
	copied.Msg = proof.Message()
	issued := &nativeIssued{owner: p, req: copied, wire: bytes.Clone(wire), identity: pr.ProofIdentity{Principal: dynamicupdate.Principal{Key: id.Key, Version: id.CredentialVersion}, ProviderVersion: id.ProviderVersion, Algorithm: id.Algorithm, Transport: req.Client.Proto.String(), Domain: "dns-update:" + zk.HeadName(p.f.l.Spec.Zone), Subject: id.Key, RetryID: strconv.FormatUint(p.retry.Add(1), 10), SemanticDigest: digest, Term: p.f.fence.Term, AuthorityIncarnation: p.f.fence.Incarnation, NotAfter: proof.Deadline()}}
	p.last = issued
	return pr.VerifiedUpdate{Zone: p.f.l.Spec.Zone, Wire: wire, Proof: issued}, nil
}
func (p *nativeIssuer) Verify(ctx context.Context, opaque any, wire []byte) (pr.ProofIdentity, error) {
	issued, ok := opaque.(*nativeIssued)
	if !ok || issued == nil || issued.owner != p || ctx.Err() != nil || issued.req.Ctx.Err() != nil || !bytes.Equal(wire, issued.wire) {
		return pr.ProofIdentity{}, pr.ErrUnavailable
	}
	if _, ok := dnsserver.VerifiedTSIGFor(&issued.req, time.Now()); !ok {
		return pr.ProofIdentity{}, pr.ErrUnavailable
	}
	return issued.identity, nil
}
