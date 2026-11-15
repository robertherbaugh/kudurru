package blocking

import (
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/miekg/dns"
	engine "github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

func TestRewriteDependencyDropAndTruncation(t *testing.T) {
	for _, safe := range []bool{false, true} {
		for _, outcome := range []string{"drop", "truncated", "error"} {
			t.Run(fmt.Sprintf("safesearch=%t/%s", safe, outcome), func(t *testing.T) {
				ix := modifierIndex(t, "||alias.example^$dnsrewrite=target.example")
				name, client := "alias.example.", "192.0.2.1"
				if safe {
					ix, name, client = familyIndex(t), "www.google.com.", "10.0.90.5"
				}
				closed := 0
				failure := errors.New("target failed")
				var dependency *pipeline.Response
				st, err := New(Options{ResolveTarget: func(sub *pipeline.Request) (*pipeline.Response, error) {
					dependency = pipeline.Reply(sub, dns.RcodeSuccess, pipeline.StatusBlockedRPZ)
					dependency.List, dependency.Rule = "default/feed", "target.policy.test. tcp-only"
					dependency.AddCleanup(func() { closed++ })
					switch outcome {
					case "drop":
						dependency.Msg, dependency.Rule = nil, "target.policy.test. drop"
						return dependency, pipeline.ErrDrop
					case "error":
						dependency.Msg.Rcode, dependency.Status = dns.RcodeServerFailure, pipeline.StatusDNSSECBogus
						dependency.Security.State = pipeline.ValidationBogus
						return dependency, failure
					default:
						dependency.Msg.Truncated, dependency.Msg.AuthenticatedData = true, true
						dependency.Security.State = pipeline.ValidationSecure
						rr, _ := dns.NewRR(sub.Question().Name + " 60 IN A 192.0.2.1")
						dependency.Msg.Answer = []dns.RR{rr}
						return dependency, nil
					}
				}})
				if err != nil {
					t.Fatal(err)
				}
				req := modifierRequest(ix, name, dns.TypeA, client)
				out, err := st.Serve(req, neverNext(t))
				if out == nil || closed != 0 {
					t.Fatalf("lost dependency ownership: response=%v closed=%d", out, closed)
				}
				switch outcome {
				case "drop":
					if !errors.Is(err, pipeline.ErrDrop) || out.Msg != nil || out.Status != pipeline.StatusBlockedRPZ || out.List != dependency.List || out.Rule != dependency.Rule {
						t.Fatalf("lost drop diagnostic: %+v %v", out, err)
					}
				case "error":
					if !errors.Is(err, failure) || out.Msg.Question[0].Name != name || out.Status != pipeline.StatusDNSSECBogus || out.Security.State != pipeline.ValidationBogus || len(out.Msg.Answer) != 0 {
						t.Fatalf("lost target failure: %+v %v", out, err)
					}
				default:
					if err != nil || !out.Msg.Truncated || out.Msg.Question[0].Name != name || len(out.Msg.Answer) != 0 || out.Msg.AuthenticatedData || out.Security.State != pipeline.ValidationUnknown || out.Cacheable || !out.Modified || out.List != dependency.List || out.Rule != dependency.Rule || out.Status != pipeline.StatusBlockedRPZ {
						t.Fatalf("truncation became positive alias: %+v %v", out, err)
					}
					if len(dependency.Msg.Answer) != 1 || !dependency.Msg.AuthenticatedData {
						t.Fatal("mutated target message")
					}
				}
				out.Close()
				out.Close()
				if closed != 1 {
					t.Fatalf("dependency cleanup count=%d", closed)
				}
			})
		}
	}
}

func TestRewriteDependencyFailureRetainsEDEAndAuthorityBoundary(t *testing.T) {
	for _, safe := range []bool{false, true} {
		t.Run(fmt.Sprintf("safesearch=%t", safe), func(t *testing.T) {
			ix := modifierIndex(t, "||alias.example^$dnsrewrite=expired.example")
			name, client := "alias.example.", "192.0.2.1"
			if safe {
				ix, name, client = familyIndex(t), "www.google.com.", "10.0.90.5"
			}
			var target *dns.Msg
			st, err := New(Options{ResolveTarget: func(sub *pipeline.Request) (*pipeline.Response, error) {
				r := pipeline.Reply(sub, dns.RcodeServerFailure, pipeline.StatusDNSSECBogus)
				r.Origin, r.Security.State = pipeline.OriginFailure, pipeline.ValidationBogus
				r.Msg.SetEdns0(1232, true)
				r.Msg.IsEdns0().Option = []dns.EDNS0{&dns.EDNS0_EDE{InfoCode: dns.ExtendedErrorCodeDNSSECIndeterminate, ExtraText: "dependency proof failed"}}
				r.Msg.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: sub.Question().Name, Rrtype: dns.TypeA, Class: 1}, A: net.ParseIP("192.0.2.77")}}
				target = r.Msg
				return r, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			q := modifierRequest(ix, name, dns.TypeA, client)
			r, err := st.Serve(q, neverNext(t))
			if err != nil || r.Msg.Rcode != dns.RcodeServerFailure || len(r.Msg.Answer) != 0 || r.Status != pipeline.StatusDNSSECBogus || r.Security.State != pipeline.ValidationBogus || r.Origin != pipeline.OriginFailure || r.Cacheable || r.Msg.AuthenticatedData || r.Msg.Question[0].Name != name {
				t.Fatalf("failure became usable alias: %+v %v", r, err)
			}
			code, text, ok := ede(r.Msg)
			if !ok || code != dns.ExtendedErrorCodeDNSSECIndeterminate || text != "dependency proof failed" {
				t.Fatalf("dependency EDE hidden by forged-answer diagnostic: %d %q", code, text)
			}
			if len(target.Answer) != 1 || target.Question[0].Name == name {
				t.Fatal("failure handling mutated dependency")
			}
		})
	}
}

func TestModifierResponseContextPinnedAndGroupsNotTags(t *testing.T) {
	ix := modifierIndex(t, "||target.example^$ctag=device_pc")
	tags := []string{"device_pc"}
	calls := 0
	st, _ := New(Options{Context: func(*pipeline.Request) engine.Context { calls++; return engine.Context{Tags: tags} }})
	q := modifierRequest(ix, "raw.example.", dns.TypeA, "192.0.2.1")
	r, err := st.Serve(q, func(sub *pipeline.Request) (*pipeline.Response, error) {
		tags = nil // the next inventory publication does not recapture this request
		return (&modifierTerminal{alias: true}).Serve(sub, nil)
	})
	if err != nil || r.Status != pipeline.StatusBlockedCNAME || calls != 1 {
		t.Fatal("answer did not retain captured inventory context")
	}
	st, _ = New(Options{})
	q = modifierRequest(ix, "raw.example.", dns.TypeA, "192.0.2.1")
	q.Client.GroupIDs = []string{"device_pc"}
	r, err = st.Serve(q, func(sub *pipeline.Request) (*pipeline.Response, error) {
		return (&modifierTerminal{alias: true}).Serve(sub, nil)
	})
	if err != nil || r.Status != pipeline.StatusForwarded {
		t.Fatal("policy group IDs became inventory tags")
	}
}

func TestHTTPSHintBudgetAndLastHintAreInspected(t *testing.T) {
	ix := modifierIndex(t, "||192.0.2.66^$dnstype=HTTPS,client=192.0.2.1")
	st, _ := New(Options{})
	for _, size := range []int{maxInspect, maxInspect + 1, 1024} {
		q := modifierRequest(ix, "raw.example.", dns.TypeHTTPS, "192.0.2.1")
		r, err := st.Serve(q, func(sub *pipeline.Request) (*pipeline.Response, error) {
			hint := &dns.SVCBIPv4Hint{}
			for i := range size {
				ip := "192.0.2.77"
				if i == size-1 {
					ip = "192.0.2.66"
				}
				hint.Hint = append(hint.Hint, net.ParseIP(ip))
			}
			m := new(dns.Msg)
			m.SetReply(sub.Msg)
			m.Answer = []dns.RR{&dns.HTTPS{SVCB: dns.SVCB{Hdr: dns.RR_Header{Name: sub.Question().Name, Rrtype: dns.TypeHTTPS, Class: 1}, Target: ".", Priority: 1, Value: []dns.SVCBKeyValue{hint}}}}
			return &pipeline.Response{Msg: m, Status: pipeline.StatusForwarded, Origin: pipeline.OriginForwarded}, nil
		})
		if err != nil || r.Status != pipeline.StatusBlockedCNAME {
			t.Fatalf("hint %d escaped inspection: %+v %v", size, r, err)
		}
		if size == maxInspect && r.Rule != "||192.0.2.66^$dnstype=HTTPS,client=192.0.2.1" {
			t.Fatal("last in-budget hint not matched")
		}
		if size > maxInspect && r.Rule != "response inspection limit" {
			t.Fatal("hint overflow did not fail closed")
		}
	}
}

func TestRewriteHopBudgetAndOpcodeBypass(t *testing.T) {
	var rules []string
	for i := range maxRewriteHops + 2 {
		rules = append(rules, fmt.Sprintf("||hop%d.example^$dnsrewrite=hop%d.example", i, i+1))
	}
	ix := modifierIndex(t, rules...)
	st, _ := New(Options{})
	r, err := st.Serve(modifierRequest(ix, "hop0.example.", dns.TypeA, "192.0.2.1"), neverNext(t))
	if err != nil || r.Msg.Rcode != dns.RcodeServerFailure || len(r.Msg.Answer) != 0 {
		t.Fatal("unbounded rewrite chase")
	}
	for _, opcode := range []int{dns.OpcodeNotify, dns.OpcodeUpdate} {
		q := modifierRequest(ix, "hop0.example.", dns.TypeA, "192.0.2.1")
		q.Msg.Opcode = opcode
		called := false
		_, err = st.Serve(q, func(sub *pipeline.Request) (*pipeline.Response, error) {
			called = true
			return pipeline.Reply(sub, dns.RcodeSuccess, pipeline.StatusForwarded), nil
		})
		if err != nil || !called {
			t.Fatal("modifier consumed an authoritative opcode")
		}
	}
}
