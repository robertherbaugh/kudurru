package main

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/recursor"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
	upstreamstage "github.com/robertherbaugh/kudurru/internal/stages/upstream"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	upstreamset "github.com/robertherbaugh/kudurru/internal/upstream"
)

func TestRootAnchorRetainedDNSDoesNotGrantDataOrCacheMutation(t *testing.T) {
	f := newServingCommandFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	lease, err := f.s.publisher.CaptureHTTP(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	q := new(dns.Msg)
	q.SetQuestion("retained.example.", dns.TypeA)
	req := &pipeline.Request{Ctx: ctx, Msg: q, Serving: servingdns.Wrap(lease)}
	insertion, err := captureRootAnchorInsertion(req)
	if err != nil {
		t.Fatal(err)
	}
	native, err := captureRootNativeSource(req)
	if err != nil {
		t.Fatal(err)
	}
	mutation, err := native.Capture(ctx)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := f.s.opts.Store.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	err = f.s.opts.Store.Command(sqlite.WithWriterToken(ctx, writer), writer.Term(), func(ctx context.Context, tx *sqlite.CommandTx) error {
		return stageCommandLabel(ctx, tx, auth.Principal{})
	})
	if err != nil {
		t.Fatal(err)
	}
	// A native resolver attempt keeps full data currency for its writes, but
	// an ordinary commit consumes it without the writes instead of failing the
	// resolution (D377), like a superseded raw-cache fill.
	if err := mutation.Apply(func() error { t.Fatal("native mutation lost its strict data guard"); return nil }); err != nil {
		t.Fatal("ordinary commit failed held native recursion", err)
	}
	if err := mutation.Apply(func() error { t.Fatal("consumed native attempt reused"); return nil }); err == nil {
		t.Fatal("skipped native mutation did not consume its attempt")
	}
	// Recursion that starts on the retained lease after the commit, before a
	// successor is served, is admitted under the retained policy; its own
	// cache attempts are skipped the same way.
	lateNative, err := captureRootNativeSource(req)
	if err != nil {
		t.Fatal("ordinary commit refused native recursion on the retained lease", err)
	}
	lateMutation, err := lateNative.Capture(ctx)
	if err != nil {
		t.Fatal("ordinary commit refused a native cache attempt", err)
	}
	if err := lateMutation.Apply(func() error { t.Fatal("late native mutation used superseded data"); return nil }); err != nil {
		t.Fatal(err)
	}
	// No new publication is captured. Both an original insertion and one
	// captured from the retained lease after commit must leave the cache alone.
	late, err := captureRootAnchorInsertion(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range []interface{ Apply(func() error) error }{insertion, late} {
		if err := attempt.Apply(func() error { t.Fatal("old data mutated cache"); return nil }); err != nil {
			t.Fatal("ordinary commit invalidated held DNS work", err)
		}
		if err := attempt.Apply(func() error { t.Fatal("consumed insertion reused"); return nil }); err == nil {
			t.Fatal("skipped mutation did not consume its attempt")
		}
	}
	// Exercise actual ingress replies from the original complete lease. These
	// carry node identity, never canonical authoritative or local zone data.
	for _, name := range []string{ingress.HealthName, "id.server."} {
		t.Run(name, func(t *testing.T) {
			query := new(dns.Msg)
			query.SetQuestion(name, dns.TypeTXT)
			if name == "id.server." {
				query.Question[0].Qclass = dns.ClassCHAOS
			}
			request := &pipeline.Request{Ctx: ctx, Msg: query, Serving: req.Serving}
			output, err := captureRootAnchorOutput(request)
			if err != nil {
				t.Fatal(err)
			}
			response, err := (&ingress.Stage{NodeID: "original-node"}).Serve(request, func(*pipeline.Request) (*pipeline.Response, error) {
				t.Fatal("node identity reached a data-answering stage")
				return nil, nil
			})
			if err != nil || len(response.Msg.Answer) != 1 || response.Msg.Answer[0].Header().Rrtype != dns.TypeTXT {
				t.Fatal("ingress identity reply failed", err)
			}
			o := output.(*rootAnchorOutput)
			if err := o.BindResponse(response); err != nil {
				t.Fatal(err)
			}
			called := false
			if err := o.Emit(func() error { called = true; return nil }); err != nil || !called {
				t.Fatal("ordinary commit suppressed ingress identity", err)
			}
			if err := o.Next(); err == nil {
				t.Fatal("node identity granted transfer continuation")
			}
		})
	}
	// Local, authoritative and mixed answers of the original lease stay
	// admissible while its publication is served (D378,
	// TestRootAnchorServedDNSFollowsThePublication). The cases below that
	// refuse them are about an original lease whose successor the node has
	// already published.
	seal, err := servingdns.Component[*recursiveRootSeal](req, recursiveRootComponent)
	if err != nil {
		t.Fatal(err)
	}
	awaitServingRuntime(t, f.s, func() bool {
		return f.s.publisher.Status().ProtectedReady && !f.s.publisher.Serves(recursiveRootComponent, seal)
	})
	for _, tc := range []struct {
		name   string
		origin pipeline.ResponseOrigin
		status pipeline.Status
		rcode  int
		allow  bool
		// contacted is the upstream stage's evidence that a server was
		// actually asked (D377); security is the validation outcome.
		contacted bool
		security  pipeline.ValidationState
	}{
		{"forwarded", pipeline.OriginForwarded, pipeline.StatusForwarded, dns.RcodeSuccess, true, false, 0},
		{"cached", pipeline.OriginForwarded, pipeline.StatusCached, dns.RcodeSuccess, true, false, 0},
		{"blocked alias", pipeline.OriginSynthetic, pipeline.StatusBlockedCNAME, dns.RcodeNameError, true, false, 0},
		{"authoritative", pipeline.OriginAuthoritative, pipeline.StatusAuthoritative, dns.RcodeSuccess, false, false, 0},
		{"mixed", pipeline.OriginMixed, pipeline.StatusForwarded, dns.RcodeSuccess, false, false, 0},
		{"local", pipeline.OriginLocal, pipeline.StatusLocalRecord, dns.RcodeSuccess, false, false, 0},
		{"unattributed", pipeline.OriginUnknown, pipeline.StatusForwarded, dns.RcodeSuccess, false, false, 0},
		{"unavailable", pipeline.OriginUnknown, pipeline.StatusServfailUnsynced, dns.RcodeServerFailure, true, false, 0},
		{"unavailable success", pipeline.OriginUnknown, pipeline.StatusServfailUnsynced, dns.RcodeSuccess, false, false, 0},
		{"authoritative special status", pipeline.OriginAuthoritative, pipeline.StatusSpecialDomain, dns.RcodeSuccess, false, false, 0},
		{"upstream failure", pipeline.OriginUnknown, pipeline.StatusServfailUpstream, dns.RcodeServerFailure, true, true, 0},
		{"upstream timeout", pipeline.OriginUnknown, pipeline.StatusTimeoutUpstream, dns.RcodeServerFailure, true, true, 0},
		// Node-produced refusals carry the same status and shape but no
		// evidence that any upstream was asked: they keep the disposition
		// they had before upstream failures were released (D377).
		{"capture refusal", pipeline.OriginUnknown, pipeline.StatusServfailUpstream, dns.RcodeServerFailure, false, false, 0},
		{"capture refusal timeout", pipeline.OriginUnknown, pipeline.StatusTimeoutUpstream, dns.RcodeServerFailure, false, false, 0},
		{"indeterminate validation", pipeline.OriginFailure, pipeline.StatusServfailUpstream, dns.RcodeServerFailure, false, false, pipeline.ValidationIndeterminate},
		{"indeterminate validation of a contacted answer", pipeline.OriginFailure, pipeline.StatusServfailUpstream, dns.RcodeServerFailure, false, true, pipeline.ValidationIndeterminate},
		{"failed target", pipeline.OriginFailure, pipeline.StatusServfailUpstream, dns.RcodeServerFailure, false, false, 0},
		{"failed contacted target", pipeline.OriginFailure, pipeline.StatusServfailUpstream, dns.RcodeServerFailure, false, true, 0},
		{"upstream failure success", pipeline.OriginUnknown, pipeline.StatusServfailUpstream, dns.RcodeSuccess, false, true, 0},
		{"authoritative upstream failure", pipeline.OriginAuthoritative, pipeline.StatusServfailUpstream, dns.RcodeServerFailure, false, true, 0},
		{"local upstream failure", pipeline.OriginLocal, pipeline.StatusServfailUpstream, dns.RcodeServerFailure, false, true, 0},
		{"mixed upstream timeout", pipeline.OriginMixed, pipeline.StatusTimeoutUpstream, dns.RcodeServerFailure, false, true, 0},
		{"synthesized upstream failure", pipeline.OriginSynthetic, pipeline.StatusServfailUpstream, dns.RcodeServerFailure, false, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output, err := captureRootAnchorOutput(req)
			if err != nil {
				t.Fatal(err)
			}
			o := output.(*rootAnchorOutput)
			if o.seal.current() || !o.seal.dnsCurrent() {
				t.Fatal("fixture did not retain an old data generation")
			}
			m := new(dns.Msg)
			m.SetReply(q)
			m.Rcode = tc.rcode
			if err := o.BindResponse(&pipeline.Response{Msg: m, Origin: tc.origin, Status: tc.status, UpstreamContacted: tc.contacted, Security: pipeline.Security{State: tc.security}}); err != nil {
				t.Fatal(err)
			}
			called := false
			err = o.Emit(func() error { called = true; return nil })
			if called != tc.allow || (err == nil) != tc.allow {
				t.Fatal("wrong original output disposition", called, err)
			}
			if err := o.Next(); err == nil {
				t.Fatal("retained DNS output authorized a transfer continuation")
			}
		})
	}
	// The actual upstream stage against a port nothing listens on, with and
	// without EDNS: its empty SERVFAIL is the answer the client would have had
	// before the commit, and the only form the retained check lets through.
	closed, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := closed.LocalAddr().String()
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	forward, err := upstreamstage.New("default", []string{dead}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = forward.Set().Close() })
	address := &dns.A{Hdr: dns.RR_Header{Name: "upstream-failure.example.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.IPv4(192, 0, 2, 1)}
	for _, edns := range []bool{false, true} {
		query := new(dns.Msg)
		query.SetQuestion("upstream-failure.example.", dns.TypeA)
		if edns {
			query.SetEdns0(1232, false)
		}
		failed, err := forward.Serve(&pipeline.Request{Ctx: ctx, Msg: query}, nil)
		if err != nil || failed == nil || failed.Msg == nil || failed.Msg.Rcode != dns.RcodeServerFailure ||
			failed.Status != pipeline.StatusServfailUpstream && failed.Status != pipeline.StatusTimeoutUpstream {
			t.Fatal("upstream stage did not report its failure", err, failed)
		}
		extended := false
		if opt := failed.Msg.IsEdns0(); opt != nil {
			for _, option := range opt.Option {
				_, ok := option.(*dns.EDNS0_EDE)
				extended = extended || ok
			}
		}
		if extended != edns {
			t.Fatal("upstream failure lost or invented its extended error", failed.Msg)
		}
		for _, tc := range []struct {
			name  string
			alter func(*dns.Msg)
			allow bool
		}{
			{"empty", func(*dns.Msg) {}, true},
			{"answer", func(m *dns.Msg) { m.Answer = []dns.RR{address} }, false},
			{"authority", func(m *dns.Msg) { m.Ns = []dns.RR{address} }, false},
			{"additional", func(m *dns.Msg) { m.Extra = append(m.Extra, address) }, false},
			{"authoritative", func(m *dns.Msg) { m.Authoritative = true }, false},
		} {
			name := "actual upstream failure " + tc.name
			if edns {
				name += " edns"
			}
			t.Run(name, func(t *testing.T) {
				response := *failed
				response.Msg = failed.Msg.Copy()
				tc.alter(response.Msg)
				output, err := captureRootAnchorOutput(&pipeline.Request{Ctx: ctx, Msg: query, Serving: req.Serving})
				if err != nil {
					t.Fatal(err)
				}
				o := output.(*rootAnchorOutput)
				if o.seal.current() || !o.seal.dnsCurrent() {
					t.Fatal("fixture did not retain an old data generation")
				}
				if err := o.BindResponse(&response); err != nil {
					t.Fatal(err)
				}
				called := false
				err = o.Emit(func() error { called = true; return nil })
				if called != tc.allow || (err == nil) != tc.allow {
					t.Fatal("wrong upstream failure disposition", called, err)
				}
				if err := o.Next(); err == nil {
					t.Fatal("upstream failure authorized a transfer continuation")
				}
			})
		}
	}
	// Refusals that the actual upstream stage produces without asking any
	// server have the same status and wire shape as a failed upstream, but no
	// contact evidence: they are not released across the commit (D377). A
	// recursive group whose authorities fail is a contacted failure and is.
	query := new(dns.Msg)
	query.SetQuestion("node-refusal.example.", dns.TypeA)
	query.SetEdns0(1232, false)
	for _, tc := range []struct {
		name  string
		serve func(*testing.T) (*pipeline.Response, int)
		allow bool
	}{
		{"bind capture failure", func(t *testing.T) (*pipeline.Response, int) {
			stage := upstreamstage.NewStage(forward.Set())
			stage.Capture = func(*pipeline.Request) (*upstreamstage.View, error) { return nil, serving.ErrDependency }
			response, err := stage.Serve(&pipeline.Request{Ctx: ctx, Msg: query.Copy()}, nil)
			if err != nil {
				t.Fatal(err)
			}
			return response, 0
		}, false},
		{"native recursion refused", func(t *testing.T) (*pipeline.Response, int) {
			return retainedRecursiveServe(ctx, t, query, false, func(*pipeline.Request) (recursor.CacheMutationSource, error) {
				return nil, serving.ErrDependency
			})
		}, false},
		{"native cache admission refused", func(t *testing.T) (*pipeline.Response, int) {
			return retainedRecursiveServe(ctx, t, query, false, func(*pipeline.Request) (recursor.CacheMutationSource, error) {
				return refusingNativeSource{}, nil
			})
		}, false},
		{"recursive authorities failed", func(t *testing.T) (*pipeline.Response, int) {
			return retainedRecursiveServe(ctx, t, query, true, nil)
		}, true},
	} {
		t.Run("actual "+tc.name, func(t *testing.T) {
			response, wire := tc.serve(t)
			if response == nil || response.Msg == nil || response.Msg.Rcode != dns.RcodeServerFailure ||
				response.Status != pipeline.StatusServfailUpstream && response.Status != pipeline.StatusTimeoutUpstream {
				t.Fatal("upstream stage did not refuse", response)
			}
			if response.UpstreamContacted != tc.allow || (wire != 0) != tc.allow {
				t.Fatal("wrong upstream contact evidence", response.UpstreamContacted, wire)
			}
			output, err := captureRootAnchorOutput(&pipeline.Request{Ctx: ctx, Msg: query, Serving: req.Serving})
			if err != nil {
				t.Fatal(err)
			}
			o := output.(*rootAnchorOutput)
			if o.seal.current() || !o.seal.dnsCurrent() {
				t.Fatal("fixture did not retain an old data generation")
			}
			if err := o.BindResponse(response); err != nil {
				t.Fatal(err)
			}
			called := false
			err = o.Emit(func() error { called = true; return nil })
			if called != tc.allow || (err == nil) != tc.allow {
				t.Fatal("wrong node refusal disposition", called, err)
			}
		})
	}
	// Actual first Managed acceptance still retires all old static output and
	// cache attempts; permitting a skipped cache insertion grants no fallback.
	retired, err := captureRootAnchorInsertion(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := captureOutputBootstrap(ctx, t, f)(); err != nil {
		t.Fatal(err)
	}
	if err := retired.Apply(func() error { t.Fatal("retired anchor mutated cache"); return nil }); err == nil {
		t.Fatal("managed acceptance treated as an ordinary data commit")
	}
	if _, err := captureRootAnchorOutput(req); err == nil {
		t.Fatal("old lease adopted managed output authority")
	}
}

// refusingNativeSource refuses every cache mutation attempt, as a source whose
// original admission is gone does.
type refusingNativeSource struct{}

func (refusingNativeSource) Domain() [32]byte { return [32]byte{} }
func (refusingNativeSource) Capture(context.Context) (recursor.CacheMutation, error) {
	return nil, serving.ErrDependency
}

type retainedNativeSource struct{}

func (retainedNativeSource) Domain() [32]byte { return [32]byte{} }
func (retainedNativeSource) Capture(context.Context) (recursor.CacheMutation, error) {
	return retainedNativeMutation{}, nil
}

type retainedNativeMutation struct{}

func (retainedNativeMutation) Apply(fn func() error) error { return fn() }

// retainedAuthorities counts the questions that reach the authorities of a
// recursive group; with fail set, every one of them fails on the network.
type retainedAuthorities struct {
	fail  bool
	asked atomic.Int32
}

func (a *retainedAuthorities) Exchange(_ context.Context, q *dns.Msg, _ netip.AddrPort, _ bool) (*dns.Msg, error) {
	a.asked.Add(1)
	if a.fail {
		return nil, errors.New("authority unreachable")
	}
	m := new(dns.Msg)
	m.SetReply(q)
	m.Authoritative = true
	return m, nil
}

// retainedRecursiveServe runs the actual upstream stage over a recursive
// group whose native resolver requires cache mutation admission, and returns
// its response with the number of questions the authorities received.
func retainedRecursiveServe(ctx context.Context, t *testing.T, query *dns.Msg, fail bool, capture func(*pipeline.Request) (recursor.CacheMutationSource, error)) (*pipeline.Response, int) {
	t.Helper()
	cfg := recursor.DefaultConfig()
	cfg.DNSSEC = false
	cfg.IPv6 = recursor.IPv6Off
	cfg.RootHints = []string{"10.0.0.1:53"}
	authorities := &retainedAuthorities{fail: fail}
	r, err := recursor.New(cfg, recursor.Options{RequireCacheMutations: true, Exchanger: authorities})
	if err != nil {
		t.Fatal(err)
	}
	set, err := upstreamset.New(upstreamset.Config{Groups: map[string]upstreamset.GroupConfig{"default": {Recursive: true}}, Recursion: cfg}, upstreamset.Options{Recursor: r})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = set.Close() })
	stage := upstreamstage.NewStage(set)
	if capture == nil {
		capture = func(*pipeline.Request) (recursor.CacheMutationSource, error) { return retainedNativeSource{}, nil }
	}
	stage.CaptureNative = capture
	response, err := stage.Serve(&pipeline.Request{Ctx: ctx, Msg: query.Copy(), Deadline: time.Now().Add(3 * time.Second)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return response, int(authorities.asked.Load())
}
