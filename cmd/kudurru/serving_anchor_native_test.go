package main

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/recursor"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
)

type rootNativeExchange struct {
	hold func() error
	once sync.Once
	err  error
}

func (e *rootNativeExchange) Exchange(_ context.Context, q *dns.Msg, _ netip.AddrPort, _ bool) (*dns.Msg, error) {
	e.once.Do(func() { e.err = e.hold() })
	if e.err != nil {
		return nil, e.err
	}
	m := new(dns.Msg)
	m.SetReply(q)
	m.Authoritative = true
	for _, name := range []string{"a.root.", "b.root."} {
		m.Answer = append(m.Answer, &dns.NS{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeNS, Class: dns.ClassINET, Ttl: 60}, Ns: name})
		m.Extra = append(m.Extra, &dns.A{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.IPv4(10, 0, 0, 1)})
	}
	return m, nil
}

type rootNativeObserved struct {
	recursor.CacheMutationSource
	hold func() error
	once sync.Once
}

func (s *rootNativeObserved) Capture(ctx context.Context) (recursor.CacheMutation, error) {
	m, err := s.CacheMutationSource.Capture(ctx)
	if err != nil {
		return nil, err
	}
	return &rootNativeObservedMutation{m, s}, nil
}

type rootNativeObservedMutation struct {
	recursor.CacheMutation
	source *rootNativeObserved
}

func (m *rootNativeObservedMutation) Apply(fn func() error) error {
	return m.CacheMutation.Apply(func() error {
		var err error
		m.source.once.Do(func() { err = m.source.hold() })
		if err != nil {
			return err
		}
		return fn()
	})
}

func TestRootNativeActualCacheRetirement(t *testing.T) {
	for _, where := range []string{"acquisition", "mutation"} {
		t.Run(where, func(t *testing.T) {
			f := newServingCommandFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			lease, err := f.s.publisher.CaptureHTTP(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Release()
			source, err := captureRootNativeSource(&pipeline.Request{Ctx: ctx, Serving: servingdns.Wrap(lease)})
			if err != nil {
				t.Fatal(err)
			}
			bootstrap := captureOutputBootstrap(ctx, t, f)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			hold := func() error {
				close(entered)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			ex := &rootNativeExchange{hold: func() error { return nil }}
			if where == "acquisition" {
				ex.hold = hold
			} else {
				source = &rootNativeObserved{CacheMutationSource: source, hold: hold}
			}
			cfg := recursor.DefaultConfig()
			cfg.DNSSEC = false
			cfg.IPv6 = recursor.IPv6Off
			cfg.RootHints = []string{"10.0.0.1:53"}
			r, err := recursor.New(cfg, recursor.Options{Exchanger: ex, RequireCacheMutations: true})
			if err != nil {
				t.Fatal(err)
			}
			query := new(dns.Msg)
			query.SetQuestion(".", dns.TypeNS)
			done := make(chan error, 1)
			go func() {
				_, _, err := r.ExchangeWithValidation(recursor.WithCacheMutations(ctx, source), query)
				done <- err
			}()
			select {
			case <-entered:
			case err := <-done:
				t.Fatal("native target missed", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			committed := make(chan error, 1)
			go func() { committed <- bootstrap() }()
			if where == "mutation" {
				select {
				case err := <-committed:
					t.Fatal("managed commit crossed active native mutation", err)
				case <-time.After(40 * time.Millisecond):
				}
			} else {
				select {
				case err := <-committed:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			unblock()
			select {
			case err := <-done:
				if where == "acquisition" && err == nil {
					t.Fatal("retired acquisition succeeded")
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if where == "mutation" {
				select {
				case err := <-committed:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			if where == "acquisition" && r.CacheLen() != 0 {
				t.Fatal("retired priming inserted actual native entry")
			}
			if where == "mutation" && r.CacheLen() != 1 {
				t.Fatal("admitted original native effect lost", r.CacheLen())
			}
			if _, err := source.Capture(ctx); err == nil {
				t.Fatal("original source adopted managed successor")
			}
		})
	}
}

func TestRootNativeOriginalLifetimeAndForeignDomain(t *testing.T) {
	f := newServingCommandFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	lease, err := f.s.publisher.CaptureHTTP(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	source, err := captureRootNativeSource(&pipeline.Request{Ctx: ctx, Serving: servingdns.Wrap(lease)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Capture(context.Background()); err == nil {
		t.Fatal("unbounded child accepted")
	}
	longer, stop := context.WithTimeout(t.Context(), 6*time.Second)
	defer stop()
	if _, err := source.Capture(longer); err == nil {
		t.Fatal("child renewed original lifetime")
	}
	attempt, err := source.Capture(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := attempt.Apply(func() error { t.Fatal("canceled actual mutation entered"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := captureRootNativeSource(&pipeline.Request{Ctx: ctx}); err == nil {
		t.Fatal("missing original component accepted")
	}
	r, err := recursor.New(recursor.DefaultConfig(), recursor.Options{RequireCacheMutations: true, TrustDomain: [32]byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	query := new(dns.Msg)
	query.SetQuestion("example.", dns.TypeA)
	if _, _, err := r.ExchangeWithValidation(recursor.WithCacheMutations(t.Context(), source), query); !errors.Is(err, recursor.ErrCacheAdmission) {
		t.Fatal("foreign native domain accepted", err)
	}
}
