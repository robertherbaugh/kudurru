package blocking

import (
	"context"
	"errors"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	engine "github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/listdata/catalogue"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
	"github.com/robertherbaugh/kudurru/internal/responsepolicy"
	"github.com/robertherbaugh/kudurru/internal/rpz"
	cachestage "github.com/robertherbaugh/kudurru/internal/stages/cache"
	rewritestage "github.com/robertherbaugh/kudurru/internal/stages/rewrite"
)

var rpzEpoch = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

func rpzFeed(t testing.TB, list string, expiry time.Time, rules ...string) *rpz.Zone {
	t.Helper()
	records := rpzRRs(t, append([]string{"policy.test. 600 IN SOA ns.policy.test. hostmaster.policy.test. 1 10 3 30 5", "policy.test. 600 IN NS ns.policy.test."}, rules...)...)
	z, err := rpz.Compile(rpz.Spec{List: list, Source: "feed", Origin: "policy.test.", Identity: list, ExpiresAt: expiry}, records)
	if err != nil {
		t.Fatal(err)
	}
	return z
}

func rpzRRs(t testing.TB, text ...string) []dns.RR {
	t.Helper()
	var out []dns.RR
	for _, line := range text {
		rr, err := dns.NewRR(line)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, rr)
	}
	return out
}

func rpzStage(t *testing.T, selected *rpz.Selection, opts Options) *Stage {
	t.Helper()
	if opts.Now == nil {
		opts.Now = func() time.Time { return rpzEpoch }
	}
	if opts.RPZ == nil {
		opts.RPZ = func(*pipeline.Request) (*rpz.Selection, error) { return selected, nil }
	}
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func rpzRequest(t *testing.T, rules ...string) *pipeline.Request {
	t.Helper()
	return modifierRequest(modifierIndex(t, rules...), "bad.test.", dns.TypeA, "192.0.2.1")
}

func rpzRaw(t testing.TB, req *pipeline.Request, records ...string) *pipeline.Response {
	t.Helper()
	m := new(dns.Msg)
	m.SetReply(req.Msg)
	m.Answer = rpzRRs(t, records...)
	m.AuthenticatedData = true
	return &pipeline.Response{Msg: m, Status: pipeline.StatusForwarded, Origin: pipeline.OriginForwarded, Cacheable: true, Security: pipeline.Security{State: pipeline.ValidationSecure}}
}

func TestRPZStageActionsAndPrivateWire(t *testing.T) {
	for _, tc := range []struct {
		name, rule      string
		proto           pipeline.Proto
		rcode, answers  int
		truncated, drop bool
	}{
		{name: "nxdomain", rule: "CNAME .", rcode: dns.RcodeNameError},
		{name: "nodata", rule: "CNAME *."},
		{name: "local-data", rule: "A 192.0.2.9", answers: 1},
		{name: "type-missing", rule: "AAAA 2001:db8::9"},
		{name: "drop", rule: "CNAME rpz-drop.", drop: true},
		{name: "tcp-only", rule: "CNAME rpz-tcp-only.", truncated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selected := rpz.NewSelection(rpzFeed(t, "default/feed", rpzEpoch.Add(20*time.Second), "bad.test.policy.test. 300 IN "+tc.rule))
			s := rpzStage(t, selected, Options{})
			req := rpzRequest(t)
			req.Client.Proto, req.Msg.CheckingDisabled = tc.proto, true
			original := rpzRaw(t, req, "bad.test. 300 IN A 192.0.2.77", "bad.test. 300 IN RRSIG A 15 2 300 20300101000000 20200101000000 1 test. AAAA")
			before := original.Msg.String()
			calls, closes := 0, 0
			original.AddCleanup(func() { closes++ })
			out, err := s.Serve(req, func(*pipeline.Request) (*pipeline.Response, error) { calls++; return original, nil })
			if calls != 1 || out == nil || out.Status != pipeline.StatusBlockedRPZ || out.List != "default/feed" || !out.Modified || out.Cacheable || out.Security.State != pipeline.ValidationUnknown {
				t.Fatalf("policy provenance: %+v %v calls=%d", out, err, calls)
			}
			if tc.drop {
				if !errors.Is(err, pipeline.ErrDrop) || out.Msg != nil {
					t.Fatal("drop lost response metadata", err)
				}
			} else if err != nil || out.Msg.Rcode != tc.rcode || len(out.Msg.Answer) != tc.answers || out.Msg.Truncated != tc.truncated || out.Msg.AuthenticatedData || !out.Msg.CheckingDisabled || out.Msg.IsEdns0() == nil {
				t.Fatalf("action wire: %+v %v", out, err)
			}
			if tc.answers > 0 && out.Msg.Answer[0].Header().Ttl != 20 {
				t.Fatal("policy TTL exceeds source lifetime")
			}
			out.Close()
			out.Close()
			if closes != 1 || original.Msg.String() != before {
				t.Fatal("raw response changed or cleanup repeated", closes)
			}
		})
	}
}

func TestRPZInterEnginePrecedenceAndSingleResolution(t *testing.T) {
	for _, tc := range []struct {
		name, action string
		rules        []string
		proto        pipeline.Proto
		status       pipeline.Status
	}{
		{"ordinary exception", ".", []string{"@@||bad.test^"}, pipeline.ProtoUDP, pipeline.StatusBlockedRPZ},
		{"ordinary deny", "*.", []string{"||bad.test^"}, pipeline.ProtoUDP, pipeline.StatusBlockedRPZ},
		{"ordinary rewrite", ".", []string{"||bad.test^$dnsrewrite=192.0.2.9"}, pipeline.ProtoUDP, pipeline.StatusBlockedRPZ},
		{"passthru deny", "rpz-passthru.", []string{"||bad.test^"}, pipeline.ProtoUDP, pipeline.StatusBlockedExact},
		{"passthru rewrite", "rpz-passthru.", []string{"||bad.test^$dnsrewrite=192.0.2.9"}, pipeline.ProtoUDP, pipeline.StatusLocalRecord},
		{"tcp stream deny", "rpz-tcp-only.", []string{"||bad.test^"}, pipeline.ProtoTCP, pipeline.StatusBlockedExact},
		{"no match deny", ".", []string{"||bad.test^"}, pipeline.ProtoUDP, pipeline.StatusBlockedExact},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := "bad.test"
			if tc.name == "no match deny" {
				name = "unrelated.test"
			}
			s := rpzStage(t, rpz.NewSelection(rpzFeed(t, "feed", rpzEpoch.Add(time.Minute), name+".policy.test. 60 IN CNAME "+tc.action)), Options{})
			req := rpzRequest(t, tc.rules...)
			req.Client.Proto = tc.proto
			calls, closes := 0, 0
			raw := rpzRaw(t, req, "bad.test. 60 IN A 192.0.2.77")
			raw.AddCleanup(func() { closes++ })
			out, err := s.Serve(req, func(*pipeline.Request) (*pipeline.Response, error) { calls++; return raw, nil })
			if err != nil || out.Status != tc.status || calls != 1 {
				t.Fatalf("precedence: %+v %v calls=%d", out, err, calls)
			}
			out.Close()
			if closes != 1 {
				t.Fatal("superseded raw lease leaked", closes)
			}
		})
	}
}

func TestRPZExplicitAuthorizationPrecedesUnavailableFeed(t *testing.T) {
	snap := compileLists(t, map[string][]string{"allow": {"||bad.test^$client=192.0.2.1,dnstype=A"}, "deny": {"@@||bad.test^"}}, map[string]engine.Mode{"allow": engine.ModeAllow})
	for _, allow := range []bool{true, false} {
		req := rpzRequest(t)
		refs := []string{"deny"}
		if allow {
			refs = append(refs, "allow")
		}
		sel, err := snap.Select(refs, engine.SelectOptions{AllowOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		d := *policy.FromRequest(req)
		d.Selection, req.Policy = sel, &d
		s := rpzStage(t, rpz.NewSelection(nil), Options{})
		calls := 0
		out, err := s.Serve(req, func(r *pipeline.Request) (*pipeline.Response, error) { calls++; return rpzRaw(t, r), nil })
		if err != nil {
			t.Fatal(err)
		}
		if allow && (calls != 1 || out.Status != pipeline.StatusForwarded) {
			t.Fatal("explicit allow did not bypass RPZ")
		}
		if !allow && (calls != 0 || out.Msg.Rcode != dns.RcodeSuccess || out.Status == pipeline.StatusBlockedRPZ || out.Status == pipeline.StatusServfailUpstream) {
			t.Fatal("allow-only not outer authorization", out)
		}
		out.Close()
	}
}

func TestRPZResolverFailuresRemainFailures(t *testing.T) {
	boom := errors.New("transport failed")
	for _, kind := range []string{"error", "nil", "no message", "servfail", "refused", "origin", "bogus"} {
		t.Run(kind, func(t *testing.T) {
			s := rpzStage(t, rpz.NewSelection(rpzFeed(t, "feed", rpzEpoch.Add(time.Minute), "bad.test.policy.test. IN CNAME .")), Options{})
			req := rpzRequest(t, "||bad.test^$dnsrewrite=192.0.2.9", "||bad.test^")
			raw := rpzRaw(t, req)
			var wantErr error
			switch kind {
			case "error":
				wantErr = boom
			case "nil":
				raw = nil
			case "no message":
				raw.Msg = nil
			case "servfail":
				raw.Msg.Rcode = dns.RcodeServerFailure
			case "refused":
				raw.Msg.Rcode = dns.RcodeRefused
			case "origin":
				raw.Origin = pipeline.OriginFailure
			case "bogus":
				raw.Security.State = pipeline.ValidationBogus
			}
			out, err := s.Serve(req, func(*pipeline.Request) (*pipeline.Response, error) { return raw, wantErr })
			if out != raw || err != wantErr {
				t.Fatalf("genuine failure replaced: %+v %v", out, err)
			}
			out.Close()
		})
	}
	for _, rcode := range []int{dns.RcodeSuccess, dns.RcodeNameError} {
		s := rpzStage(t, rpz.NewSelection(rpzFeed(t, "feed", rpzEpoch.Add(time.Minute), "bad.test.policy.test. IN A 192.0.2.9")), Options{})
		req := rpzRequest(t)
		raw := rpzRaw(t, req)
		raw.Msg.Rcode = rcode
		out, err := s.Serve(req, func(*pipeline.Request) (*pipeline.Response, error) { return raw, nil })
		if err != nil || out.Status != pipeline.StatusBlockedRPZ || len(out.Msg.Answer) != 1 {
			t.Fatal("normal negative not eligible for RPZ", err)
		}
		out.Close()
	}
}

func TestRPZEarlierResponseIPBeatsLaterQName(t *testing.T) {
	first := rpzFeed(t, "first", rpzEpoch.Add(time.Minute), "32.77.2.0.192.rpz-ip.policy.test. IN CNAME *.")
	last := rpzFeed(t, "last", rpzEpoch.Add(time.Minute), "bad.test.policy.test. IN CNAME .")
	s := rpzStage(t, rpz.NewSelection(first, last), Options{})
	req := rpzRequest(t)
	out, err := s.Serve(req, func(r *pipeline.Request) (*pipeline.Response, error) {
		return rpzRaw(t, r, "bad.test. IN A 192.0.2.77"), nil
	})
	if err != nil || out.List != "first" || out.Msg.Rcode != dns.RcodeSuccess {
		t.Fatal("preflight QNAME bypassed earlier response-IP feed", out, err)
	}
	out.Close()
}

func TestRPZCoverageBoundaryAndBypassGuards(t *testing.T) {
	expiry := rpzEpoch.Add(time.Minute)
	feed := rpzFeed(t, "feed", expiry, "bad.test.policy.test. IN CNAME .")
	for _, tc := range []struct {
		name     string
		selected *rpz.Selection
		now      time.Time
		fail     bool
	}{
		{"before", rpz.NewSelection(feed), expiry.Add(-time.Nanosecond), false},
		{"boundary", rpz.NewSelection(feed), expiry, true},
		{"missing", rpz.NewSelection(nil), rpzEpoch, true},
		{"unrelated selection", nil, expiry, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rpzStage(t, tc.selected, Options{Now: func() time.Time { return tc.now }})
			req := rpzRequest(t)
			calls := 0
			out, err := s.Serve(req, func(r *pipeline.Request) (*pipeline.Response, error) { calls++; return rpzRaw(t, r), nil })
			if err != nil || tc.fail != (out.Msg.Rcode == dns.RcodeServerFailure) || tc.fail && calls != 0 {
				t.Fatalf("coverage: %+v %v calls=%d", out, err, calls)
			}
			out.Close()
		})
	}
	for _, guard := range []string{"disabled", "toggle", "RD", "class", "opcode", "transfer", "no policy"} {
		t.Run(guard, func(t *testing.T) {
			s := rpzStage(t, nil, Options{RPZ: func(*pipeline.Request) (*rpz.Selection, error) { t.Fatal("guard captured RPZ"); return nil, nil }})
			req := rpzRequest(t)
			switch guard {
			case "disabled":
				s.Reconfigure(Options{Disabled: true})
			case "toggle":
				if _, err := s.Toggle().Disable(time.Minute); err != nil {
					t.Fatal(err)
				}
			case "RD":
				req.Msg.RecursionDesired = false
			case "class":
				req.Msg.Question[0].Qclass = dns.ClassCHAOS
			case "opcode":
				req.Msg.Opcode = dns.OpcodeUpdate
			case "transfer":
				req.Msg.Question[0].Qtype = dns.TypeAXFR
			case "no policy":
				req.Policy = nil
			}
			calls := 0
			out, err := s.Serve(req, func(r *pipeline.Request) (*pipeline.Response, error) { calls++; return rpzRaw(t, r), nil })
			if err != nil || calls != 1 || out.Status != pipeline.StatusForwarded {
				t.Fatal("guard intercepted request", err)
			}
			out.Close()
		})
	}
}

func TestRPZCNAMEPinsCaptureAndCleansDependencies(t *testing.T) {
	selected := rpz.NewSelection(rpzFeed(t, "feed", rpzEpoch.Add(30*time.Second), "bad.test.policy.test. 300 IN CNAME target.test."))
	var current atomic.Pointer[rpz.Selection]
	current.Store(selected)
	captures, views, originalCalls, targetCalls, rawCloses, targetCloses := 0, 0, 0, 0, 0, 0
	var s *Stage
	next := func(req *pipeline.Request) (*pipeline.Response, error) {
		if req.Question().Name == "bad.test." {
			originalCalls++
			current.Store(rpz.NewSelection(nil))
			r := rpzRaw(t, req, "bad.test. 300 IN A 192.0.2.77")
			r.AddCleanup(func() { rawCloses++ })
			return r, nil
		}
		targetCalls++
		r := rpzRaw(t, req, "target.test. 300 IN A 192.0.2.9", "target.test. 300 IN RRSIG A 15 2 300 20300101000000 20200101000000 1 test. AAAA")
		r.AddCleanup(func() { targetCloses++ })
		return r, nil
	}
	v, err := NewView(Options{}, time.Time{}, catalogue.Embedded())
	if err != nil {
		t.Fatal(err)
	}
	lease := blockingLease{v}
	s = rpzStage(t, nil, Options{RPZ: func(req *pipeline.Request) (*rpz.Selection, error) {
		captures++
		if req.Serving != lease {
			t.Fatal("lost serving cut")
		}
		return current.Load(), nil
	}, ResolveTarget: func(sub *pipeline.Request) (*pipeline.Response, error) {
		if sub.Zone != nil || sub.DNSSECValidation != nil || sub.UpstreamGroup != "" || sub.Serving != lease || sub.Client.ClientID != "pinned" {
			t.Fatal("target crossed authority/configuration boundary incorrectly")
		}
		return s.Serve(sub, next)
	}})
	s.Capture = func(*pipeline.Request) (*View, error) { views++; return v, nil }
	req := rpzRequest(t)
	req.Serving, req.Client.ClientID, req.Zone, req.UpstreamGroup = lease, "pinned", "old zone", "old group"
	enabled := false
	req.DNSSECValidation = &enabled
	out, err := s.Serve(req, next)
	if err != nil || out.Status != pipeline.StatusBlockedRPZ || out.Origin != pipeline.OriginMixed || len(out.Msg.Answer) != 2 || out.Msg.AuthenticatedData || out.Security.State != pipeline.ValidationUnknown || captures != 1 || views != 1 || originalCalls != 1 || targetCalls != 1 {
		t.Fatalf("capture/resolve composition: %+v %v captures=%d/%d calls=%d/%d", out, err, captures, views, originalCalls, targetCalls)
	}
	for _, rr := range out.Msg.Answer {
		if rr.Header().Ttl != 30 {
			t.Fatal("dependency exceeds policy expiry")
		}
	}
	out.Close()
	out.Close()
	if rawCloses != 1 || targetCloses != 1 {
		t.Fatal("dependency cleanup repeated/leaked", rawCloses, targetCloses)
	}
	// A fresh request must capture the newly unavailable generation.
	fresh := rpzRequest(t)
	fresh.Serving = lease
	out, err = s.Serve(fresh, neverNext(t))
	if err != nil || out.Msg.Rcode != dns.RcodeServerFailure || captures != 2 {
		t.Fatal("old feed leaked to fresh capture", err)
	}
	out.Close()
}

func TestRPZCNAMETargetExpiryFailureAndLoop(t *testing.T) {
	for _, kind := range []string{"expiry", "servfail", "bogus", "error", "drop", "loop"} {
		t.Run(kind, func(t *testing.T) {
			now := rpzEpoch
			selected := rpz.NewSelection(rpzFeed(t, "feed", now.Add(time.Second), "bad.test.policy.test. 300 IN CNAME target.test."))
			targetCloses, rawCloses := 0, 0
			boom := errors.New("target failed")
			s := rpzStage(t, selected, Options{Now: func() time.Time { return now }, ResolveTarget: func(req *pipeline.Request) (*pipeline.Response, error) {
				dep := rpzRaw(t, req, "target.test. 300 IN A 192.0.2.9")
				dep.AddCleanup(func() { targetCloses++ })
				switch kind {
				case "expiry":
					now = now.Add(time.Second)
				case "servfail":
					dep.Msg.Rcode, dep.Status = dns.RcodeServerFailure, pipeline.StatusServfailUnsynced
				case "bogus":
					dep.Security.State = pipeline.ValidationBogus
				case "error":
					return dep, boom
				case "drop":
					dep.Msg, dep.Status = nil, pipeline.StatusBlockedRPZ
					return dep, pipeline.ErrDrop
				}
				return dep, nil
			}})
			req := rpzRequest(t, "||bad.test^")
			if kind == "loop" {
				selected = rpz.NewSelection(rpzFeed(t, "feed", now.Add(time.Second), "bad.test.policy.test. IN CNAME bad.test."))
				s.opts.RPZ = func(*pipeline.Request) (*rpz.Selection, error) { return selected, nil }
			}
			out, err := s.Serve(req, func(r *pipeline.Request) (*pipeline.Response, error) {
				raw := rpzRaw(t, r)
				raw.AddCleanup(func() { rawCloses++ })
				return raw, nil
			})
			if kind == "drop" {
				if !errors.Is(err, pipeline.ErrDrop) || out == nil || out.Status != pipeline.StatusBlockedRPZ {
					t.Fatal("target drop metadata lost", out, err)
				}
			} else if kind == "error" {
				if err != boom || out == nil || len(out.Msg.Answer) != 0 || out.Msg.Question[0].Name != "bad.test." {
					t.Fatal("target error lost", out, err)
				}
			} else if err != nil || out.Msg.Rcode != dns.RcodeServerFailure || len(out.Msg.Answer) != 0 {
				t.Fatalf("target failure became positive: %+v %v", out, err)
			}
			out.Close()
			out.Close()
			wantTarget := 1
			if kind == "loop" {
				wantTarget = 0
			}
			if rawCloses != 1 || targetCloses != wantTarget {
				t.Fatal("failure cleanup ownership", rawCloses, targetCloses)
			}
		})
	}
}

func TestRPZCNAMETailLocalDataLifetimeForMissingType(t *testing.T) {
	selected := rpz.NewSelection(rpzFeed(t, "feed", rpzEpoch.Add(2*time.Second), "target.test.policy.test. 300 IN AAAA 2001:db8::9"))
	s := rpzStage(t, selected, Options{})
	req := rpzRequest(t)
	out, err := s.Serve(req, func(r *pipeline.Request) (*pipeline.Response, error) {
		return rpzRaw(t, r, "bad.test. 300 IN CNAME target.test.", "target.test. 300 IN A 192.0.2.77"), nil
	})
	if err != nil || len(out.Msg.Answer) != 1 || out.Msg.Answer[0].Header().Rrtype != dns.TypeCNAME || out.Msg.Answer[0].Header().Ttl != 2 {
		t.Fatal("type-missing local data retained stale alias", out, err)
	}
	out.Close()
}

func TestRPZInspectionIgnoresUnprovenHintsAndBoundsInput(t *testing.T) {
	selected := rpz.NewSelection(rpzFeed(t, "feed", rpzEpoch.Add(time.Minute), "32.77.2.0.192.rpz-ip.policy.test. IN CNAME .", "target.test.policy.test. IN CNAME ."))
	s := rpzStage(t, selected, Options{})
	req := rpzRequest(t)
	raw := rpzRaw(t, req, "bad.test. IN HTTPS 1 target.test. ipv4hint=192.0.2.77")
	raw.Msg.Ns = rpzRRs(t, "bad.test. IN NS target.test.")
	raw.Msg.Extra = rpzRRs(t, "target.test. IN A 192.0.2.77")
	out, err := s.Serve(req, func(*pipeline.Request) (*pipeline.Response, error) { return raw, nil })
	if err != nil || out != raw {
		t.Fatal("RPZ inferred NS or HTTPS provenance", out, err)
	}
	out.Close()
	req = rpzRequest(t)
	raw = rpzRaw(t, req)
	for range maxInspect + 1 {
		raw.Msg.Answer = append(raw.Msg.Answer, &dns.A{Hdr: dns.RR_Header{Name: "bad.test.", Rrtype: dns.TypeA, Class: 1}, A: netip.MustParseAddr("192.0.2.77").AsSlice()})
	}
	out, err = s.Serve(req, func(*pipeline.Request) (*pipeline.Response, error) { return raw, nil })
	if err != nil || out.Msg.Rcode != dns.RcodeServerFailure {
		t.Fatal("inspection overflow fell through", err)
	}
	out.Close()
}

func TestRPZRawCachePolicyIsolationAndConfiguredRewrite(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(map[bool]string{false: "raw hit", true: "configured rewrite"}[configured], func(t *testing.T) {
			selected := rpz.NewSelection(rpzFeed(t, "feed", rpzEpoch.Add(time.Minute), "32.77.2.0.192.rpz-ip.policy.test. IN CNAME ."))
			var live atomic.Pointer[rpz.Selection]
			live.Store(selected)
			s := rpzStage(t, nil, Options{RPZ: func(r *pipeline.Request) (*rpz.Selection, error) {
				if r.Client.Addr.Addr() == netip.MustParseAddr("192.0.2.1") {
					return live.Load(), nil
				}
				return nil, nil
			}})
			ix := modifierIndex(t)
			terminal := &modifierTerminal{}
			chain := pipeline.MustNew(s, &rewritestage.Stage{}, cachestage.New(cache.New(cache.Options{MaxEntries: 100}), cachestage.Settings{}, nil), terminal)
			rules, err := responsepolicy.CompileRewrites([]responsepolicy.RewriteSpec{{Name: "raw.test", Targets: []string{"192.0.2.77"}}})
			if err != nil {
				t.Fatal(err)
			}
			request := func(client string, synthesize bool) *pipeline.Request {
				r := modifierRequest(ix, "raw.test.", dns.TypeA, client)
				if synthesize {
					d := *policy.FromRequest(r)
					d.Rewrites, r.Policy = rules, &d
				}
				return r
			}
			// Warm only the raw cache; every later policy response wraps that data.
			out, err := chain.Serve(request("192.0.2.2", false))
			if err != nil {
				t.Fatal(err)
			}
			out.Close()
			for _, client := range []string{"192.0.2.1", "192.0.2.2", "192.0.2.1"} {
				out, err = chain.Serve(request(client, configured))
				if err != nil {
					t.Fatal(err)
				}
				if client == "192.0.2.1" && out.Status != pipeline.StatusBlockedRPZ {
					t.Fatal("raw hit/configured rewrite escaped RPZ", out)
				}
				if client == "192.0.2.2" && (len(out.Msg.Answer) != 1 || out.Msg.Answer[0].(*dns.A).A.String() != "192.0.2.77") {
					t.Fatal("RPZ contaminated another client", out)
				}
				out.Close()
			}
			live.Store(nil)
			out, err = chain.Serve(request("192.0.2.1", false))
			if err != nil || out.Status != pipeline.StatusCached || out.Modified || terminal.calls.Load() != 1 {
				t.Fatal("retired RPZ persisted in raw cache", out, err)
			}
			out.Close()
			live.Store(rpz.NewSelection(nil))
			out, err = chain.Serve(request("192.0.2.1", false))
			if err != nil || out.Msg.Rcode != dns.RcodeServerFailure || terminal.calls.Load() != 1 {
				t.Fatal("raw cache bypassed required missing feed", out, err)
			}
			out.Close()
		})
	}
}

func TestRPZOnlyListsSurviveExactPolicySelectionOrder(t *testing.T) {
	var compiled []*engine.List
	var objects []*v1alpha1.BlockList
	for _, name := range []string{"first", "last", "unrelated"} {
		object := bl(name, "block", v1alpha1.ListSource{Format: "rpz", RPZ: &v1alpha1.RPZSource{ZoneName: "policy.test", Primaries: []string{"127.0.0.1"}}})
		spec, err := engine.SpecFromAPI("default/"+name, &object.Spec, nil)
		if err != nil {
			t.Fatal(err)
		}
		l, err := engine.Compile(spec, nil, engine.CompileOptions{})
		if err != nil {
			t.Fatal(err)
		}
		compiled, objects = append(compiled, l), append(objects, object)
	}
	snap, err := engine.NewSnapshot(1, compiled...)
	if err != nil {
		t.Fatal(err)
	}
	ix := policy.Build(policy.Input{ClientGroups: []*v1alpha1.ClientGroup{cg("selected", v1alpha1.ClientMatcher{CIDR: "192.0.2.1/32"}), cg("other", v1alpha1.ClientMatcher{CIDR: "192.0.2.2/32"})}, Policies: []*v1alpha1.Policy{
		{ObjectMeta: meta("selected"), Spec: v1alpha1.PolicySpec{ClientGroupRefs: []string{"selected"}, BlockListRefs: []string{"last", "first"}}},
		{ObjectMeta: meta("other"), Spec: v1alpha1.PolicySpec{ClientGroupRefs: []string{"other"}, BlockListRefs: []string{"first"}}},
	}, BlockLists: objects, Lists: snap}, policy.Options{})
	feeds := map[string]*rpz.Zone{"default/first": rpzFeed(t, "default/first", rpzEpoch.Add(time.Minute), "bad.test.policy.test. IN CNAME ."), "default/last": rpzFeed(t, "default/last", rpzEpoch.Add(time.Minute), "bad.test.policy.test. IN CNAME *.")}
	s := rpzStage(t, nil, Options{RPZ: func(req *pipeline.Request) (*rpz.Selection, error) {
		d := policy.FromRequest(req)
		var ordered []*rpz.Zone
		for _, name := range d.Selection.BlockLists() {
			if d.List(name) == nil || d.List(name).Object == "" {
				t.Fatal("selected external object lost")
			}
			ordered = append(ordered, feeds[name])
		}
		return rpz.NewSelection(ordered...), nil
	}})
	for _, client := range []string{"192.0.2.1", "192.0.2.2"} {
		req := modifierRequest(ix, "bad.test.", dns.TypeA, client)
		if !policy.FromRequest(req).Filters() {
			t.Fatal("external list dropped policy filtering")
		}
		out, err := s.Serve(req, func(r *pipeline.Request) (*pipeline.Response, error) { return rpzRaw(t, r), nil })
		want := "default/last"
		if client == "192.0.2.2" {
			want = "default/first"
		}
		if err != nil || out.Status != pipeline.StatusBlockedRPZ || out.List != want {
			t.Fatal("RPZ selection escaped group scope/declaration order", out, err)
		}
		out.Close()
	}
}

func TestRPZCaptureErrorNeverUsesLiveFallback(t *testing.T) {
	s := rpzStage(t, nil, Options{RPZ: func(*pipeline.Request) (*rpz.Selection, error) { return nil, pipeline.ErrServingCapture }})
	out, err := s.Serve(rpzRequest(t), neverNext(t))
	if out != nil || !errors.Is(err, pipeline.ErrServingCapture) {
		t.Fatal("capture error fell through", out, err)
	}
}

func TestRPZRequiredInvalidConfigurationCoverage(t *testing.T) {
	for _, src := range []v1alpha1.ListSource{
		{Format: "rpz"},
		{Format: "rpz", RPZ: &v1alpha1.RPZSource{ZoneName: ".", Primaries: []string{"127.0.0.1"}}},
		{Format: "rpz", RPZ: &v1alpha1.RPZSource{ZoneName: "policy.test", Primaries: []string{"127.0.0.1"}}, URL: "https://invalid.test"},
	} {
		object := bl("feed", "block", src)
		spec, err := engine.SpecFromAPI("default/feed", &object.Spec, nil)
		if err != nil {
			t.Fatal(err)
		}
		list, err := engine.Compile(spec, nil, engine.CompileOptions{})
		if err != nil {
			t.Fatal(err)
		}
		snap, err := engine.NewSnapshot(1, list)
		if err != nil {
			t.Fatal(err)
		}
		ix := policy.Build(policy.Input{ClientGroups: []*v1alpha1.ClientGroup{cg("default", v1alpha1.ClientMatcher{CIDR: "192.0.2.0/24"})}, Policies: []*v1alpha1.Policy{{ObjectMeta: meta("default"), Spec: v1alpha1.PolicySpec{ClientGroupRefs: []string{"default"}, BlockListRefs: []string{"feed"}}}}, BlockLists: []*v1alpha1.BlockList{object}, Lists: snap}, policy.Options{})
		for _, authoritative := range []bool{false, true} {
			selected := new(rpz.Snapshot).Select([]*v1alpha1.BlockList{object}, authoritative)
			s := rpzStage(t, selected, Options{})
			out, err := s.Serve(modifierRequest(ix, "bad.test.", dns.TypeA, "192.0.2.1"), neverNext(t))
			if err != nil || out.Msg.Rcode != dns.RcodeServerFailure || out.Origin != pipeline.OriginFailure || out.List != "default/feed" {
				t.Fatal("invalid/stale RPZ fell through", out, err)
			}
			out.Close()
		}
	}
}

func TestRPZOrdinaryCNAMERewriteUsesOneOriginalQuestion(t *testing.T) {
	s := rpzStage(t, rpz.NewSelection(rpzFeed(t, "feed", rpzEpoch.Add(time.Minute), "unrelated.test.policy.test. IN CNAME .")), Options{})
	req := rpzRequest(t, "||bad.test^$dnsrewrite=target.test")
	calls := map[string]int{}
	out, err := s.Serve(req, func(r *pipeline.Request) (*pipeline.Response, error) {
		calls[r.Question().Name]++
		return rpzRaw(t, r, r.Question().Name+" 60 IN A 192.0.2.77"), nil
	})
	if err != nil || len(out.Msg.Answer) != 2 || calls["bad.test."] != 1 || calls["target.test."] != 1 {
		t.Fatal("ordinary target repeated original resolution", out, err, calls)
	}
	out.Close()
}

func TestRPZCNAMETargetTCPOnlyPreservesTransportAction(t *testing.T) {
	selected := rpz.NewSelection(rpzFeed(t, "feed", rpzEpoch.Add(time.Minute), "bad.test.policy.test. IN CNAME target.test.", "target.test.policy.test. IN CNAME rpz-tcp-only."))
	s := rpzStage(t, selected, Options{})
	calls, closes := 0, 0
	out, err := s.Serve(rpzRequest(t), func(req *pipeline.Request) (*pipeline.Response, error) {
		calls++
		raw := rpzRaw(t, req)
		raw.AddCleanup(func() { closes++ })
		return raw, nil
	})
	if err != nil || out.Status != pipeline.StatusBlockedRPZ || !out.Msg.Truncated || len(out.Msg.Answer) != 0 || out.Rule != "target.test.policy.test. tcp-only" || calls != 2 {
		t.Fatal("target TCP-only action became a positive alias", out, err, calls)
	}
	out.Close()
	out.Close()
	if closes != 2 {
		t.Fatal("truncated target cleanup", closes)
	}
}

func TestRPZTargetUsesOriginalDeadlineAndKnownEmptyCapture(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		selected := rpz.NewSelection(rpzFeed(t, "feed", rpzEpoch.Add(time.Minute), "bad.test.policy.test. IN CNAME target.test."))
		if !enabled {
			selected = nil
		}
		captureCalls := 0
		deadline := time.Now().Add(time.Minute)
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		defer cancel()
		var s *Stage
		s = rpzStage(t, nil, Options{RPZ: func(*pipeline.Request) (*rpz.Selection, error) {
			captureCalls++
			if captureCalls > 1 {
				t.Fatal("target recaptured a known empty/RPZ selection")
			}
			return selected, nil
		}, ResolveTarget: func(req *pipeline.Request) (*pipeline.Response, error) {
			if got, ok := req.Ctx.Deadline(); !ok || !got.Equal(deadline) {
				t.Fatal("target lost original deadline")
			}
			cancel()
			return s.Serve(req, func(*pipeline.Request) (*pipeline.Response, error) { return nil, req.Ctx.Err() })
		}})
		rules := []string(nil)
		if !enabled {
			rules = []string{"||bad.test^$dnsrewrite=target.test"}
		}
		req := rpzRequest(t, rules...)
		req.Ctx = ctx
		out, err := s.Serve(req, func(r *pipeline.Request) (*pipeline.Response, error) { return rpzRaw(t, r), nil })
		if out != nil || !errors.Is(err, context.Canceled) || captureCalls != 1 {
			t.Fatal("target ignored lifetime/capture contract", out, err, captureCalls)
		}
	}
}

func BenchmarkRPZStageRawResponse(b *testing.B) {
	for _, matched := range []bool{false, true} {
		b.Run(map[bool]string{false: "pass", true: "local-data"}[matched], func(b *testing.B) {
			name := "unrelated.test"
			if matched {
				name = "bad.test"
			}
			selection := rpz.NewSelection(rpzFeed(b, "feed", rpzEpoch.Add(time.Hour), name+".policy.test. 600 IN A 192.0.2.9"))
			s, err := New(Options{Now: func() time.Time { return rpzEpoch }, RPZ: func(*pipeline.Request) (*rpz.Selection, error) { return selection, nil }})
			if err != nil {
				b.Fatal(err)
			}
			ix := modifierIndex(b)
			msg := rpzRaw(b, modifierRequest(ix, "bad.test.", dns.TypeA, "192.0.2.1"), "bad.test. 60 IN A 192.0.2.77").Msg
			next := func(*pipeline.Request) (*pipeline.Response, error) {
				return &pipeline.Response{Msg: msg, Origin: pipeline.OriginForwarded, Status: pipeline.StatusCached}, nil
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				out, err := s.Serve(modifierRequest(ix, "bad.test.", dns.TypeA, "192.0.2.1"), next)
				if err != nil {
					b.Fatal(err)
				}
				out.Close()
			}
		})
	}
}
