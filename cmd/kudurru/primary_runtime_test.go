package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	cachedata "github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/listdata/catalogue"
	"github.com/robertherbaugh/kudurru/internal/listfetcher"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
	"github.com/robertherbaugh/kudurru/internal/stages/authoritative"
	"github.com/robertherbaugh/kudurru/internal/stages/blocking"
	"github.com/robertherbaugh/kudurru/internal/stages/cache"
	"github.com/robertherbaugh/kudurru/internal/stages/identify"
	"github.com/robertherbaugh/kudurru/internal/stages/postprocess"
	"github.com/robertherbaugh/kudurru/internal/stages/upstream"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type primaryNetworkFixture struct {
	store           *sqlite.Store
	publisher       *serving.Publisher
	keys            *dnsserver.StaticKeyring
	keyrings        *servingbuild.KeyringOwner
	runtime         *primaryRuntime
	chain           *pipeline.Chain
	listeners       *dnsserver.Registry
	pause           atomic.Bool
	entered, resume chan struct{}
}

func newPrimaryNetworkFixture(t *testing.T) *primaryNetworkFixture {
	t.Helper()
	f := &primaryNetworkFixture{entered: make(chan struct{}, 1), resume: make(chan struct{}, 1)}
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	f.store, err = sqlite.Open(filepath.Join(t.TempDir(), sqlite.FileName), sqlite.Options{Scheme: scheme, Validator: v, InternalKinds: privateStoreKinds(), ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.store.Close() })
	if err := f.store.Command(t.Context(), f.store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := runtimeowners.Initialize(ctx, tx, time.Now().UTC()); err != nil {
			return err
		}
		for _, obj := range []store.Object{
			&public.Kudurru{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
			&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "home"}, Spec: public.DNSZoneSpec{ZoneName: "example.", Type: public.ZoneTypePrimary, Nameservers: []string{"ns.example."}, Policy: public.ZonePolicy{Notify: public.NotifyNone, Update: public.AccessAllow, UpdatePolicies: []public.UpdatePolicy{{TSIGKeyRef: "updates", Name: "*.example.", Types: []string{"A"}}}}}},
		} {
			if _, err := tx.Apply(ctx, obj, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.keys, err = dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: "updates.", Algorithm: dnsserver.TSIGHMACSHA256, Secret: bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	f.keyrings, err = servingbuild.NewKeyringOwner(f.keys)
	if err != nil {
		t.Fatal(err)
	}
	host, err := config.FromFlat(config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	permissions, err := rbac.LoadPermissions()
	if err != nil {
		t.Fatal(err)
	}
	schema, err := servingbuild.NewSchema(scheme, v, runtimeowners.ValidateEntry, runtimeowners.ValidateImage)
	if err != nil {
		t.Fatal(err)
	}
	lists, err := listfetcher.OpenDiskCache(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	builder, err := servingbuild.New(servingbuild.Options{Schema: schema, Lists: lists, Authority: f.store.ServingAuthority, Capture: func(context.Context, *replication.Image) (servingbuild.Dependencies, error) {
		cut := f.keyrings.Capture()
		return servingbuild.Dependencies{Identity: "test-host/" + cut.Identity(), Host: host, Catalog: servingbuild.CatalogResource{Catalogue: catalogue.Embedded(), Source: host.Blocking.Catalogue}, Keys: cut.Keys(), KeyringIdentity: cut.Identity(), Current: cut.Current, Auth: servingauth.Dependencies{Identity: "test-auth", ExpectedIdentity: "test-auth", CurrentKey: bytes.Repeat([]byte{2}, 32), ConfigProviders: []servingauth.ProviderSource{}, ProviderResources: []servingauth.ProviderResource{}, Permissions: permissions}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	source, err := f.keyrings.GuardSource(f.store)
	if err != nil {
		t.Fatal(err)
	}
	f.publisher, err = serving.New(serving.Options{Source: source, Prepare: builder.Prepare, Required: serving.CompleteManifest(), DependencyChanges: f.keyrings.Changes, RetryInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	pin := servingbuild.NewTransferredRuntime(nil, nil, nil).PrimaryPin(f.keyrings.PrimaryPin(f.publisher))
	f.runtime, err = newPrimaryRuntime(t.Context(), primaryRuntimeOptions{Store: f.store, Publisher: f.publisher, Schema: schema, Pin: pin, Observe: func(out pr.UpdateOutcome) { t.Logf("UPDATE outcome: %+v", out) }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 2)
	go func() { done <- f.publisher.Run(ctx) }()
	go func() {
		done <- f.store.Elector("primary").Run(ctx, func(term context.Context) {
			if err := f.runtime.Run(term); err != nil && term.Err() == nil {
				t.Error("primary driver ended early", err)
			}
		}, nil)
	}()
	t.Cleanup(func() {
		cancel()
		for range 2 {
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Error(err)
				}
			case <-time.After(5 * time.Second):
				t.Error("primary assembly failed to stop")
			}
		}
		f.publisher.Close()
	})
	i, a, u := identify.New(nil, nil), authoritative.New(nil), upstream.NewStage(nil)
	a.Update = f.runtime.Update
	b, err := blocking.New(blocking.Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := cache.New(cachedata.New(cachedata.Options{}), cache.Settings{}, u)
	r := &postprocess.Stage{}
	f.chain, err = pipeline.New(i, r, a, b, c, u)
	if err != nil {
		t.Fatal(err)
	}
	if err := servingbuild.BindDNS(f.publisher, servingbuild.DNSStages{Chain: f.chain, Identify: i, Authority: a, Blocking: b, Cache: c, Response: r, Resolution: u}, nil); err != nil {
		t.Fatal(err)
	}
	capture := servingdns.Capture(f.publisher)
	f.chain.SetCapture(func(req *pipeline.Request) (pipeline.ServingLease, error) {
		lease, err := capture(req)
		if err == nil && req.Msg.Opcode == dns.OpcodeUpdate && f.pause.CompareAndSwap(true, false) {
			f.entered <- struct{}{}
			select {
			case <-f.resume:
			case <-req.Ctx.Done():
			}
		}
		return lease, err
	})
	logger := slog.New(slog.DiscardHandler)
	f.listeners, err = dnsserver.NewRegistry(dnsserver.Options{Handler: &dnsserver.Handler{Chain: f.chain, Logger: logger}, Logger: logger, TSIG: f.keys, Sockets: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.listeners.Add(dnsserver.Spec{Protocol: dnsserver.ProtoUDP, Address: "127.0.0.1:0"}, dnsserver.Spec{Protocol: dnsserver.ProtoTCP, Address: "127.0.0.1:0"}); err != nil {
		t.Fatal(err)
	}
	if err := f.listeners.Bind(); err != nil {
		t.Fatal(err)
	}
	listenerCtx, stop := context.WithCancel(t.Context())
	listenerDone := make(chan error, 1)
	go func() { listenerDone <- f.listeners.Run(listenerCtx) }()
	t.Cleanup(func() {
		stop()
		if err := <-listenerDone; err != nil {
			t.Error(err)
		}
	})
	f.wait(t, func() bool {
		ready, _ := f.listeners.Ready()
		lease, err := f.publisher.CaptureHTTP(t.Context())
		if err != nil {
			return false
		}
		defer lease.Release()
		value, _ := lease.Bundle().Component("runtime")
		owner := value.(*servingbuild.RuntimeOwner)
		return ready && f.runtime.driver.Status().Ready && owner.Primary().Admitted("example.", time.Now())
	})
	return f
}

func (f *primaryNetworkFixture) wait(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for !check() {
		select {
		case <-deadline.C:
			t.Fatalf("assembly failed to converge: driver=%+v publisher=%+v", f.runtime.driver.Status(), f.publisher.Status())
		case <-tick.C:
		}
	}
}

func (f *primaryNetworkFixture) exchange(msg *dns.Msg, tcp bool) (*dns.Msg, error) {
	proto, network := dnsserver.ProtoUDP, "udp"
	if tcp {
		proto, network = dnsserver.ProtoTCP, "tcp"
	}
	response, _, err := (&dns.Client{Net: network, Timeout: 5 * time.Second, TsigProvider: dnsserver.TSIGProvider(f.keys)}).Exchange(msg, f.listeners.Addr(proto).String())
	return response, err
}

func primaryUpdateMessage(apex, name string, signed bool) *dns.Msg {
	m := new(dns.Msg)
	m.SetUpdate(apex)
	rr, _ := dns.NewRR(name + " 60 IN A 192.0.2.33")
	m.Insert([]dns.RR{rr})
	if signed {
		m.SetTsig("updates.", dns.HmacSHA256, 300, time.Now().Unix())
	}
	return m
}

func TestPrimaryRuntimeNativeUpdateUsesCompletePublishedAuthority(t *testing.T) {
	f := newPrimaryNetworkFixture(t)
	for _, tcp := range []bool{false, true} {
		name := "udp.example."
		if tcp {
			name = "tcp.example."
		}
		answer, err := f.exchange(primaryUpdateMessage("example.", name, true), tcp)
		if err != nil || answer.Rcode != dns.RcodeSuccess || answer.IsTsig() == nil {
			t.Fatal("signed committed UPDATE failed", answer, err)
		}
		query := new(dns.Msg)
		query.SetQuestion(name, dns.TypeA)
		answer, err = f.exchange(query, tcp)
		if err != nil || !answer.Authoritative || len(answer.Answer) != 1 || answer.Answer[0].(*dns.A).A.String() != "192.0.2.33" {
			t.Fatal("success preceded complete query publication", answer, err)
		}
	}
	for _, tc := range []struct {
		apex, name string
		signed     bool
		code       int
	}{
		{"example.", "unsigned.example.", false, dns.RcodeRefused},
		{"sub.example.", "x.sub.example.", true, dns.RcodeNotAuth},
		{"elsewhere.", "x.elsewhere.", true, dns.RcodeNotAuth},
	} {
		answer, err := f.exchange(primaryUpdateMessage(tc.apex, tc.name, tc.signed), true)
		if answer == nil || answer.Rcode != tc.code || (err != nil && (tc.code != dns.RcodeNotAuth || !errors.Is(err, dns.ErrAuth))) {
			t.Fatal("incorrect exact-apex/native rejection", answer, err, tc.code)
		}
	}
}

func TestPrimaryRuntimeHeldNativeRequestCannotAdoptNewKeyring(t *testing.T) {
	f := newPrimaryNetworkFixture(t)
	rcode := func(msg *dns.Msg) int {
		if msg == nil {
			return -1
		}
		return msg.Rcode
	}
	f.pause.Store(true)
	type result struct {
		msg *dns.Msg
		err error
	}
	done := make(chan result, 1)
	go func() {
		msg, err := f.exchange(primaryUpdateMessage("example.", "held.example.", true), true)
		done <- result{msg, err}
	}()
	select {
	case <-f.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("native request did not capture lease")
	}
	old := f.publisher.Status().Served
	newKeys, err := dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: "updates.", Algorithm: dnsserver.TSIGHMACSHA256, Secret: bytes.Repeat([]byte{8}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.keyrings.Replace(t.Context(), newKeys); err != nil {
		t.Fatal(err)
	}
	f.wait(t, func() bool { s := f.publisher.Status(); return s.ProtectedReady && s.Served != old })
	f.resume <- struct{}{}
	got := <-done
	if got.err != nil || rcode(got.msg) != dns.RcodeServerFailure {
		t.Fatalf("old request adopted new publication: rcode=%d transport_error=%t", rcode(got.msg), got.err != nil)
	}
	answer, err := f.exchange(primaryUpdateMessage("example.", "stale-listener.example.", true), true)
	if err != nil || rcode(answer) != dns.RcodeRefused {
		t.Fatalf("old listener proof bypassed current keyring: rcode=%d transport_error=%t", rcode(answer), err != nil)
	}
	if err := f.keyrings.Replace(t.Context(), f.keys); err != nil {
		t.Fatal(err)
	}
	f.wait(t, func() bool {
		lease, err := f.publisher.CaptureHTTP(t.Context())
		if err != nil {
			return false
		}
		defer lease.Release()
		value, _ := lease.Bundle().Component("runtime")
		owner := value.(*servingbuild.RuntimeOwner)
		id, keys, current := owner.NotifyKeys()
		if id != f.keyrings.Capture().Identity() || keys != f.keys || !current() || !owner.Primary().Admitted("example.", time.Now()) {
			return false
		}
		// Restoring transport keys can publish before the primary driver has
		// reconciled the intervening keyring's durable UPDATE policy. Inspect
		// the head and its chunks from this exact complete published image;
		// never repair a captured read with a later live head or retry a write.
		image, _, err := f.store.ReadServingCandidate(t.Context())
		if err != nil {
			t.Fatal("could not read restored primary candidate")
		}
		if !image.Generation().Same(lease.Bundle().Identity().Base) {
			return false
		}
		member, ok := owner.Primary().Cut(time.Now()).Member("example.")
		if !ok {
			return false
		}
		var head *zk.ZoneState
		chunks := map[string]*zk.ZoneChunk{}
		for _, raw := range image.Payload.Objects {
			var header metav1.TypeMeta
			if json.Unmarshal(raw, &header) != nil {
				t.Fatal("could not decode restored primary object kind")
			}
			switch header.Kind {
			case zk.KindZoneState:
				var candidate zk.ZoneState
				if json.Unmarshal(raw, &candidate) != nil {
					t.Fatal("could not decode restored primary head")
				}
				if candidate.Spec.Zone == member.Version.Zone {
					head = &candidate
				}
			case zk.KindZoneChunk:
				var chunk zk.ZoneChunk
				if json.Unmarshal(raw, &chunk) != nil {
					t.Fatal("could not decode restored primary chunk")
				}
				chunks[chunk.Spec.Digest] = &chunk
			}
		}
		if head == nil || head.Spec.CommitSequence != member.Version.CommitSequence || head.Spec.RootDigest != member.Version.RootDigest {
			return false
		}
		aggregate, err := zonestate.Decode(head, func(digest string) (*zk.ZoneChunk, error) { return chunks[digest], nil }, zonestate.Limits{})
		if err != nil {
			t.Fatal("could not verify restored primary policy")
		}
		expected, ok := keys.Identity("updates.")
		policy := aggregate.Policy
		if !ok || !policy.Enabled || len(policy.Credentials) != 1 || policy.Credentials[0].Key != expected.Key || policy.Credentials[0].Version != expected.CredentialVersion {
			return false
		}
		driver := f.runtime.driver.Status()
		return driver.Running && driver.Ready && !f.publisher.Status().Preparing &&
			f.publisher.CheckWriterLease(t.Context(), lease, serving.TermActivation{Term: driver.Writer.Term, Incarnation: driver.Writer.Incarnation}) == nil
	})
	// Fresh native verification may use a newly installed ring with old bytes.
	// The held request above remains rejected and must never have added its RR.
	answer, err = f.exchange(primaryUpdateMessage("example.", "fresh.example.", true), true)
	if err != nil || rcode(answer) != dns.RcodeSuccess {
		t.Fatalf("fresh restored installation failed: rcode=%d transport_error=%t", rcode(answer), err != nil)
	}
	query := new(dns.Msg)
	query.SetQuestion("held.example.", dns.TypeA)
	answer, err = f.exchange(query, true)
	if err != nil || rcode(answer) != dns.RcodeNameError {
		t.Fatalf("rejected held request mutated durable data: rcode=%d transport_error=%t", rcode(answer), err != nil)
	}
}
