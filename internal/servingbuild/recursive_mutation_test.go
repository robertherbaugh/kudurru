package servingbuild

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/recursor"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
	upstreamstage "github.com/robertherbaugh/kudurru/internal/stages/upstream"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"github.com/robertherbaugh/kudurru/internal/upstream"
	extensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

type managedNativeSource struct {
	input   *sqlite.AnchorResolverInput
	applies atomic.Int32
}

func (s *managedNativeSource) Domain() [32]byte { return s.input.CacheDomain() }
func (s *managedNativeSource) Capture(ctx context.Context) (recursor.CacheMutation, error) {
	read, err := s.input.CaptureRead(ctx)
	if err != nil {
		return nil, err
	}
	return &managedNativeMutation{s, read}, nil
}

type managedNativeMutation struct {
	source *managedNativeSource
	read   *sqlite.AnchorRead
}

func (m *managedNativeMutation) Apply(fn func() error) error {
	return m.source.input.WithRead(m.read, func(scope sqlite.AnchorReadScope) error {
		if err := scope.Check(); err != nil {
			return err
		}
		m.source.applies.Add(1)
		if err := fn(); err != nil {
			return err
		}
		return scope.Check()
	})
}

func managedNativeInterface(t *testing.T) netip.Addr {
	t.Helper()
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range addresses {
		prefix, err := netip.ParsePrefix(address.String())
		if err == nil {
			ip := prefix.Addr().Unmap()
			if ip.Is4() && ip.IsGlobalUnicast() && !ip.IsLoopback() {
				return ip
			}
		}
	}
	t.Fatal("private authority fixture requires a local non-loopback IPv4 interface")
	return netip.Addr{}
}

func TestManagedBuilderNativeCacheActualSignedDenial(t *testing.T) {
	for _, deny := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "held-key-denied"}[deny], func(t *testing.T) {
			f := newManagedBuilderFixture(t, true)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			wire := managedBuilderMessage(t, f.key, f.signer, false)
			ip := managedNativeInterface(t)
			packet, err := net.ListenPacket("udp", net.JoinHostPort(ip.String(), "0"))
			if err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			var keys atomic.Int32
			listening := make(chan struct{})
			peer := &dns.Server{PacketConn: packet, NotifyStartedFunc: func() { close(listening) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
				m := new(dns.Msg)
				m.SetReply(q)
				m.Authoritative = true
				if q.Question[0].Qtype == dns.TypeDNSKEY {
					if keys.Add(1) == 2 && deny {
						close(entered)
						select {
						case <-release:
						case <-ctx.Done():
							return
						}
					}
					m.Answer = wire.Copy().Answer
				} else {
					for _, name := range []string{"a.root.", "b.root."} {
						m.Answer = append(m.Answer, &dns.NS{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeNS, Class: dns.ClassINET, Ttl: 60}, Ns: name})
						m.Extra = append(m.Extra, &dns.A{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.IP(ip.AsSlice())})
					}
				}
				_ = w.WriteMsg(m)
			})}
			peerDone := make(chan struct{})
			var peerErr error
			go func() { peerErr = peer.ActivateAndServe(); close(peerDone) }()
			defer func() {
				unblock()
				_ = peer.Shutdown()
				<-peerDone
				if peerErr != nil {
					t.Error("authority completion", peerErr)
				}
			}()
			select {
			case <-listening:
			case <-peerDone:
				t.Fatal("authority startup", peerErr)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
				var cfg public.Kudurru
				if err := tx.Get(ctx, store.Key{Kind: "Kudurru", Name: "default"}, &cfg); err != nil {
					return err
				}
				var upstreams map[string]any
				if err := json.Unmarshal(cfg.Spec.Settings["upstreams"].Raw, &upstreams); err != nil {
					return err
				}
				recursion := upstreams["recursion"].(map[string]any)
				recursion["rootHints"] = []string{packet.LocalAddr().String()}
				raw, err := json.Marshal(upstreams)
				if err != nil {
					return err
				}
				cfg.Spec.Settings["upstreams"] = extensions.JSON{Raw: raw}
				_, err = tx.Apply(ctx, &cfg, store.ApplyOptions{Manager: store.ManagerSystem})
				return err
			})
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
			runCtx, stop := context.WithCancel(ctx)
			pubDone := make(chan struct{})
			go func() { defer close(pubDone); _ = pub.Run(runCtx) }()
			defer func() { stop(); <-pubDone; pub.Close() }()
			select {
			case <-pub.Initialized():
			case <-ctx.Done():
				t.Fatal("actual managed publisher unavailable", ctx.Err(), pub.Status())
			}
			p := <-prepared
			if p.Anchor == nil || !p.Current() {
				t.Fatal("actual managed candidate missing")
			}
			lease, err := pub.CaptureHTTP(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Release()
			stage := upstreamstage.NewStage(nil)
			stage.Capture = func(req *pipeline.Request) (*upstreamstage.View, error) {
				return servingdns.Component[*upstreamstage.View](req, "resolution")
			}
			// Use the actual published plan and its owned managed resolver, retaining
			// the same complete lease and original input through the real socket work.
			query := new(dns.Msg)
			query.SetQuestion(".", dns.TypeDNSKEY)
			query.SetEdns0(1232, true)
			plan, err := stage.Bind(&pipeline.Request{Ctx: ctx, Msg: query, Serving: servingdns.Wrap(lease)})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := plan.Exchange(ctx, query); err == nil || keys.Load() != 0 {
				t.Fatal("managed owned resolver accepted missing mutation source", err)
			}
			source := &managedNativeSource{input: p.Anchor.Input()}
			before, err := f.store.ReadGeneration(ctx)
			if err != nil {
				t.Fatal(err)
			}
			owner, err := accepted.DecodeRows(before.Payload.Runtime, before.Term.ClusterID)
			if err != nil {
				t.Fatal(err)
			}
			originalDenial, err := f.store.CaptureAnchorDenial(ctx, owner.Version(), f.clock)
			if err != nil {
				t.Fatal(err)
			}
			type outcome struct {
				result *upstream.Result
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				r, err := plan.Exchange(recursor.WithCacheMutations(ctx, source), query)
				done <- outcome{r, err}
			}()
			if deny {
				select {
				case <-entered:
				case out := <-done:
					t.Fatal("second signed DNSKEY target not reached", out.err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				retained := source.applies.Load()
				verified, err := originalDenial.Verify(managedBuilderMessage(t, f.key, f.signer, true))
				if err != nil {
					t.Fatal(err)
				}
				receipt, _, err := f.store.CommitAnchorDenial(verified)
				if err != nil || receipt.Outcome() != sqlite.AnchorCommitted {
					t.Fatal("actual local deny failed", err)
				}
				after, err := f.store.ReadGeneration(ctx)
				if err != nil || !before.Generation().Same(after.Generation()) || source.input.Current() {
					t.Fatal("local denial changed canonical progress or retained grant", err)
				}
				unblock()
				select {
				case out := <-done:
					if out.err == nil || out.result != nil {
						t.Fatal("held key adopted revoked owner", out)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if source.applies.Load() != retained {
					t.Fatal("denied DNSKEY entered managed native mutation callback")
				}
			} else {
				select {
				case out := <-done:
					if out.err != nil || out.result == nil || out.result.DNSSEC != recursor.SecSecure || keys.Load() < 2 || source.applies.Load() < 2 {
						t.Fatal("actual signed managed native fill missing", out.err, keys.Load(), source.applies.Load())
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				beforeKeys := keys.Load()
				warm, err := plan.Exchange(recursor.WithCacheMutations(ctx, source), query)
				if err != nil || warm.DNSSEC != recursor.SecSecure || keys.Load() != beforeKeys+1 {
					t.Fatal("actual managed DNSKEY cache was not reused", err, keys.Load(), beforeKeys)
				}
			}
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				t.Fatal("test exhausted original lifetime")
			}
		})
	}
}
