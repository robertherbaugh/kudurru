package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

type updateOutputObservation struct {
	request  *pipeline.Request
	response *pipeline.Response
	output   *rootAnchorOutput
	err      error
}

type updateOutputObserver func(*pipeline.Request, *pipeline.Response, error)

func (f updateOutputObserver) Observe(req *pipeline.Request, resp *pipeline.Response, err error, _ time.Duration) {
	f(req, resp, err)
}

func TestRootUpdateOutputActualCompletionAndFinalRefusals(t *testing.T) {
	for _, mode := range []string{"acknowledge", "acknowledge UDP", "data", "rcode", "substituted message", "transfer", "keys", "writer", "released", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			entered := make(chan updateOutputObservation, 1)
			release := make(chan struct{}, 1)
			f := newPrimaryNetworkFixtureWithOutput(t, updateOutputObserver(func(req *pipeline.Request, resp *pipeline.Response, err error) {
				if req.Msg.Opcode != dns.OpcodeUpdate {
					return
				}
				o, _ := dnsserver.CapturedOutput(req).(*rootAnchorOutput)
				entered <- updateOutputObservation{req, resp, o, err}
				select {
				case <-release:
				case <-req.Ctx.Done():
				}
			}))
			defer close(release)
			type result struct {
				message *dns.Msg
				err     error
			}
			done := make(chan result, 1)
			go func() {
				m, err := f.exchange(primaryUpdateMessage("example.", "committed.example.", true), mode != "acknowledge UDP")
				done <- result{m, err}
			}()
			var held updateOutputObservation
			select {
			case held = <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("actual UPDATE did not complete before original bound")
			}
			if held.err != nil || held.response == nil || held.response.Msg.Rcode != dns.RcodeSuccess || held.output == nil || held.output.ack == nil || !held.output.ack.current() || held.output.seal.current() {
				t.Fatal("did not retain actual completion independently of retired data generation")
			}
			switch mode {
			case "data":
				rr, err := dns.NewRR("committed.example. 60 IN A 192.0.2.33")
				if err != nil {
					t.Fatal(err)
				}
				held.response.Msg.Answer = []dns.RR{rr}
			case "rcode":
				held.response.Msg.Rcode = dns.RcodeRefused
			case "substituted message":
				held.response.Msg = held.response.Msg.Copy()
			case "transfer":
				held.response.Transfer = func(yield func(*dns.Msg) bool) { yield(held.response.Msg) }
			case "keys":
				keys, err := dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: "updates.", Algorithm: dnsserver.TSIGHMACSHA256, Secret: bytes.Repeat([]byte{8}, 32)})
				if err != nil {
					t.Fatal(err)
				}
				if err := f.keyrings.Replace(t.Context(), keys); err != nil {
					t.Fatal(err)
				}
			case "writer":
				f.expectWriterLoss.Store(true)
				term := f.store.Authority().Term
				term.Epoch++
				term.PrimaryID, term.PromotionID = "other", "later"
				if _, err := f.store.ObserveAuthority(t.Context(), term); err != nil {
					t.Fatal(err)
				}
			case "released":
				held.response.Close()
			case "deadline":
				<-held.request.Ctx.Done()
			}
			release <- struct{}{}
			got := <-done
			if mode == "acknowledge" || mode == "acknowledge UDP" {
				if got.err != nil || got.message == nil || got.message.Rcode != dns.RcodeSuccess || got.message.IsTsig() == nil || len(got.message.Answer) != 0 || len(got.message.Ns) != 0 {
					t.Fatal("original committed UPDATE acknowledgment failed", got.err)
				}
				if err := held.output.Next(); err == nil {
					t.Fatal("acknowledgment allowed a transfer continuation")
				}
			} else if got.message != nil || got.err == nil {
				t.Fatal("invalidated acknowledgment emitted bytes")
			}
		})
	}
}

func TestRootUpdateOutputDoesNotRelaxOrdinaryDataGeneration(t *testing.T) {
	entered := make(chan updateOutputObservation, 1)
	release := make(chan struct{}, 1)
	f := newPrimaryNetworkFixtureWithOutput(t, updateOutputObserver(func(req *pipeline.Request, resp *pipeline.Response, err error) {
		if req.Msg.Opcode != dns.OpcodeQuery {
			return
		}
		o, _ := dnsserver.CapturedOutput(req).(*rootAnchorOutput)
		entered <- updateOutputObservation{req, resp, o, err}
		select {
		case <-release:
		case <-req.Ctx.Done():
		}
	}))
	defer close(release)
	done := make(chan bool, 1)
	go func() {
		q := new(dns.Msg)
		q.SetQuestion("missing.example.", dns.TypeA)
		m, err := f.exchange(q, true)
		done <- m == nil && err != nil
	}()
	var held updateOutputObservation
	select {
	case held = <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("query did not reach original output boundary")
	}
	if held.output == nil || held.output.ack != nil || !held.output.seal.current() {
		t.Fatal("query missing original data guard")
	}
	m, err := f.exchange(primaryUpdateMessage("example.", "different.example.", true), true)
	if err != nil || m == nil || m.Rcode != dns.RcodeSuccess {
		t.Fatal("separate actual UPDATE did not complete", err)
	}
	if held.output.seal.current() {
		t.Fatal("old query generation did not retire")
	}
	release <- struct{}{}
	if !<-done {
		t.Fatal("ordinary query used command acknowledgment bypass")
	}
}
