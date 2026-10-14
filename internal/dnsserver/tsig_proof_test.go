package dnsserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
)

type proofRequest struct {
	req   *pipeline.Request
	proof *VerifiedTSIG
}
type proofStage struct{ seen chan proofRequest }

func (s *proofStage) receive(t *testing.T) proofRequest {
	t.Helper()
	select {
	case request := <-s.seen:
		return request
	case <-time.After(time.Second):
		t.Fatal("UPDATE did not reach the pipeline")
		return proofRequest{}
	}
}

func (*proofStage) Name() string { return "authoritative" }
func (s *proofStage) Serve(req *pipeline.Request, _ pipeline.Next) (*pipeline.Response, error) {
	p, _ := VerifiedTSIGFor(req, time.Now())
	if req.Msg.Opcode == dns.OpcodeNotify {
		p, _ = VerifiedNotifyFor(req, time.Now())
	}
	copyReq := *req
	copyReq.Msg = req.Msg.Copy()
	s.seen <- proofRequest{req: &copyReq, proof: p}
	return pipeline.Reply(req, dns.RcodeRefused, pipeline.StatusRefusedACL), nil
}

func signedUpdate() *dns.Msg {
	m := new(dns.Msg)
	m.SetUpdate("example.org.")
	m.Compress = true
	rr, _ := dns.NewRR("host.example.org. 60 IN A 192.0.2.3")
	m.Insert([]dns.RR{rr})
	m.SetTsig(xfrKey, dns.HmacSHA256, 300, time.Now().Unix())
	return m
}

func TestVerifiedUpdateProofNativeListeners(t *testing.T) {
	keys := testKeyring(t)
	stage := &proofStage{seen: make(chan proofRequest, 16)}
	f := startFixture(t, fixtureOpts{specs: allSpecs(), tsig: keys, stages: []pipeline.Stage{&ingress.Stage{}, stage}})
	for _, proto := range []Protocol{ProtoUDP, ProtoTCP, ProtoDoT} {
		t.Run(string(proto), func(t *testing.T) {
			c := &dns.Client{Net: string(proto), Timeout: time.Second, TsigProvider: TSIGProvider(keys)}
			if proto == ProtoDoT {
				c.Net, c.TLSConfig = "tcp-tls", (&tlsPool{f.cert}).client("dot")
			}
			res, _, err := c.Exchange(signedUpdate(), f.addr(proto))
			if err != nil || res == nil || res.Rcode != dns.RcodeRefused || res.IsTsig() == nil {
				t.Fatalf("signed reply: %v", err)
			}
			got := stage.receive(t)
			want, _ := keys.Identity(xfrKey)
			if got.proof == nil || got.proof.Identity() != want {
				t.Fatal("missing exact immutable verification identity")
			}
			if got.proof.Message().Ns[0].String() != "host.example.org.\t60\tIN\tA\t192.0.2.3" {
				t.Fatal("signed content lost")
			}
			// Pure checks use the captured instant because the listener has now
			// completed and canceled the request's operation context.
			now := got.proof.verifiedAt
			if _, ok := VerifiedTSIGFor(got.req, now); !ok {
				t.Fatal("exact detached original lost proof")
			}
			mutated := *got.req
			mutated.Msg = got.req.Msg.Copy()
			mutated.Msg.Ns[0].Header().Ttl++
			if _, ok := VerifiedTSIGFor(&mutated, now); ok {
				t.Fatal("proof authorized changed UPDATE semantics")
			}
			mutated = *got.req
			mutated.Client.Proto = pipeline.ProtoDoH
			if _, ok := VerifiedTSIGFor(&mutated, now); ok {
				t.Fatal("proof transplanted to unverified transport")
			}
			mutated = *got.req
			mutated.Client.Synthetic = true
			if _, ok := VerifiedTSIGFor(&mutated, now); ok {
				t.Fatal("proof admitted synthetic request")
			}
			mutated = *got.req
			mutated.Ctx = context.Background()
			if _, ok := VerifiedTSIGFor(&mutated, now); ok {
				t.Fatal("key name alone authorized UPDATE")
			}
			if _, ok := VerifiedTSIGFor(got.req, got.proof.Deadline()); ok {
				t.Fatal("expired proof accepted")
			}
			copyMsg := got.proof.Message()
			copyMsg.Ns[0].Header().Ttl++
			if _, ok := VerifiedTSIGFor(got.req, now); !ok {
				t.Fatal("returned message aliased private proof")
			}
			for _, text := range []string{fmt.Sprintf("%+v", got.proof), fmt.Sprintf("%#v", got.proof)} {
				if strings.Contains(text, "192.0.2.3") || strings.Contains(text, want.CredentialVersion) {
					t.Fatal("private proof leaked in diagnostics")
				}
			}
			raw, _ := json.Marshal(got.proof)
			if string(raw) != `{"private":true}` {
				t.Fatal("private proof serialized")
			}
		})
	}
	for _, proto := range []Protocol{ProtoDoH, ProtoDoQ, ProtoDoH3} {
		t.Run(string(proto), func(t *testing.T) {
			// These transports parse TSIG as data, but do not verify it.
			_, err := f.dnsproxyClient(t, proto).Exchange(context.Background(), signedUpdate())
			if err != nil {
				t.Fatal(err)
			}
			if got := stage.receive(t); got.proof != nil || got.req.Client.TSIGKey != "" {
				t.Fatal("unverified transport minted proof")
			}
		})
	}
}

// Wrapping a static keyring in an arbitrary Lookup capability intentionally
// loses immutable-provider proof. A live provider may rotate at any lookup.
type lookupOnly struct{ TSIGKeyring }

func TestMutableTSIGProviderAndUnsignedRequestsHaveNoUpdateProof(t *testing.T) {
	stage := &proofStage{seen: make(chan proofRequest, 4)}
	keys := testKeyring(t)
	f := startFixture(t, fixtureOpts{specs: []Spec{{Protocol: ProtoTCP}}, tsig: lookupOnly{keys}, stages: []pipeline.Stage{&ingress.Stage{}, stage}})
	c := &dns.Client{Net: "tcp", Timeout: time.Second, TsigProvider: TSIGProvider(keys)}
	for _, signed := range []bool{true, false} {
		msg := signedUpdate()
		if !signed {
			msg.Extra = nil
		}
		if _, _, err := c.Exchange(msg, f.addr(ProtoTCP)); err != nil {
			t.Fatal(err)
		}
		if got := stage.receive(t); got.proof != nil {
			t.Fatal("mutable provider or unsigned request minted proof")
		}
	}
	if _, ok := VerifiedTSIGFor(&pipeline.Request{Ctx: context.WithValue(context.Background(), verifiedTSIGKey{}, &VerifiedTSIG{})}, time.Now()); ok {
		t.Fatal("zero proof accepted")
	}
}

func TestUpdateSignatureFailureNeverReachesPipeline(t *testing.T) {
	stage := &proofStage{seen: make(chan proofRequest, 4)}
	f := startFixture(t, fixtureOpts{specs: []Spec{{Protocol: ProtoUDP}, {Protocol: ProtoTCP}}, tsig: testKeyring(t), stages: []pipeline.Stage{&ingress.Stage{}, stage}})
	for _, proto := range []Protocol{ProtoUDP, ProtoTCP} {
		for _, failure := range []string{"unknown", "wrong-secret", "expired"} {
			msg := signedUpdate()
			secret := xfrSecret
			switch failure {
			case "unknown":
				msg.IsTsig().Hdr.Name = "unknown-key."
			case "wrong-secret":
				secret = otherSecret
			case "expired":
				msg.IsTsig().TimeSigned -= 3600
			}
			c := &dns.Client{Net: string(proto), Timeout: time.Second, TsigSecret: map[string]string{msg.IsTsig().Hdr.Name: b64(secret)}}
			res, _, _ := c.Exchange(msg, f.addr(proto))
			if res == nil || res.Rcode != dns.RcodeNotAuth {
				t.Fatalf("%s/%s did not reject TSIG", proto, failure)
			}
			select {
			case <-stage.seen:
				t.Fatal("failed UPDATE signature reached pipeline")
			default:
			}
		}
	}
}

func TestUpdateHeaderAdmissionBounds(t *testing.T) {
	for _, tc := range []struct {
		header dns.Header
		want   dns.MsgAcceptAction
	}{
		{dns.Header{Bits: dns.OpcodeUpdate << 11, Qdcount: 1, Ancount: 10, Nscount: 10, Arcount: 1}, dns.MsgAccept},
		{dns.Header{Bits: dns.OpcodeUpdate << 11, Qdcount: 0}, dns.MsgReject},
		{dns.Header{Bits: dns.OpcodeUpdate << 11, Qdcount: 1, Ancount: 65535, Nscount: 2}, dns.MsgReject},
		{dns.Header{Bits: dns.OpcodeUpdate << 11, Qdcount: 1, Arcount: 3}, dns.MsgReject},
		{dns.Header{Bits: dns.OpcodeUpdate<<11 | 1<<15, Qdcount: 1}, dns.MsgIgnore},
	} {
		if got := acceptDNSMessage(tc.header); got != tc.want {
			t.Fatalf("header %+v: %v, want %v", tc.header, got, tc.want)
		}
	}
	for _, opcode := range []uint16{dns.OpcodeQuery, dns.OpcodeNotify, dns.OpcodeStatus} {
		h := dns.Header{Bits: opcode << 11, Qdcount: 1}
		if acceptDNSMessage(h) != dns.DefaultMsgAcceptFunc(h) {
			t.Fatal("changed non-UPDATE admission")
		}
	}
}

func TestStaticTSIGVersionsAndSecretIsolation(t *testing.T) {
	keys := testKeyring(t)
	original, _ := keys.Identity(xfrKey)
	secret, _, _ := keys.Lookup(xfrKey)
	secret[0] ^= 1
	got, _, _ := keys.Lookup(xfrKey)
	if string(got) != string(xfrSecret) {
		t.Fatal("Lookup mutated immutable verification generation")
	}
	other, err := NewStaticKeyring(TSIGKey{Name: xfrKey512, Algorithm: TSIGHMACSHA512, Secret: xfrSecret512}, TSIGKey{Name: xfrKey, Algorithm: TSIGHMACSHA256, Secret: xfrSecret})
	if err != nil {
		t.Fatal(err)
	}
	reordered, _ := other.Identity("XFR-Key")
	if reordered != original {
		t.Fatal("order/name-case changed generation identity")
	}
	rotated, err := NewStaticKeyring(TSIGKey{Name: xfrKey, Algorithm: TSIGHMACSHA256, Secret: otherSecret})
	if err != nil {
		t.Fatal(err)
	}
	changed, _ := rotated.Identity(xfrKey)
	if changed.CredentialVersion == original.CredentialVersion || changed.ProviderVersion == original.ProviderVersion {
		t.Fatal("rotation retained credential identity")
	}
}

func TestVerifiedNotifyProofCannotBecomeUpdate(t *testing.T) {
	keys := testKeyring(t)
	stage := &proofStage{seen: make(chan proofRequest, 16)}
	f := startFixture(t, fixtureOpts{specs: allSpecs(), tsig: keys, stages: []pipeline.Stage{&ingress.Stage{}, stage}})
	message := func() *dns.Msg {
		m := new(dns.Msg).SetNotify("example.org.")
		m.SetTsig(xfrKey, dns.HmacSHA256, 300, time.Now().Unix())
		return m
	}
	for _, proto := range []Protocol{ProtoUDP, ProtoTCP, ProtoDoT} {
		t.Run(string(proto), func(t *testing.T) {
			c := &dns.Client{Net: string(proto), Timeout: time.Second, TsigProvider: TSIGProvider(keys)}
			if proto == ProtoDoT {
				c.Net, c.TLSConfig = "tcp-tls", (&tlsPool{f.cert}).client("dot")
			}
			response, _, err := c.Exchange(message(), f.addr(proto))
			if err != nil || response == nil || response.IsTsig() == nil {
				t.Fatal("native NOTIFY proof response", err)
			}
			got := stage.receive(t)
			want, _ := keys.Identity(xfrKey)
			if got.proof == nil || got.proof.Identity() != want {
				t.Fatal("NOTIFY lost original verifier identity")
			}
			now := got.proof.verifiedAt
			if _, ok := VerifiedNotifyFor(got.req, now); !ok {
				t.Fatal("NOTIFY lost original proof")
			}
			if _, ok := VerifiedTSIGFor(got.req, now); ok {
				t.Fatal("NOTIFY authorized UPDATE")
			}
			copyReq := *got.req
			copyReq.Msg = got.req.Msg.Copy()
			copyReq.Msg.Opcode = dns.OpcodeUpdate
			if _, ok := VerifiedTSIGFor(&copyReq, now); ok {
				t.Fatal("changed opcode converted proof authority")
			}
			copyReq.Msg = got.req.Msg.Copy()
			copyReq.Msg.Question[0].Name = "other.example.org."
			if _, ok := VerifiedNotifyFor(&copyReq, now); ok {
				t.Fatal("NOTIFY proof changed target")
			}
			if _, ok := VerifiedNotifyFor(got.req, got.proof.Deadline()); ok {
				t.Fatal("expired NOTIFY proof accepted")
			}
		})
	}
	for _, proto := range []Protocol{ProtoDoH, ProtoDoQ, ProtoDoH3} {
		if _, err := f.dnsproxyClient(t, proto).Exchange(t.Context(), message()); err != nil {
			t.Fatal(err)
		}
		if got := stage.receive(t); got.proof != nil {
			t.Fatal("unsupported transport minted NOTIFY proof", proto)
		}
	}
}
