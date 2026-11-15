package dnsserver

import (
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

type startupKeysStage struct {
	entered, release chan struct{}
	once             sync.Once
}

func (*startupKeysStage) Name() string { return "authoritative" }
func (s *startupKeysStage) Serve(req *pipeline.Request, _ pipeline.Next) (*pipeline.Response, error) {
	if req.Serving != nil {
		return nil, errors.New("startup credentials claimed complete serving ownership")
	}
	if req.Question().Name == "held.example.org." {
		s.once.Do(func() { close(s.entered) })
		select {
		case <-s.release:
		case <-req.Ctx.Done():
			return nil, req.Ctx.Err()
		}
	}
	return pipeline.Reply(req, dns.RcodeSuccess, pipeline.StatusLocalRecord), nil
}

func TestStartupKeysCaptureRefusalAndOriginalTransportProvider(t *testing.T) {
	for _, proto := range []Protocol{ProtoUDP, ProtoTCP, ProtoDoT, ProtoDoH, ProtoDoQ, ProtoDoH3} {
		t.Run(string(proto), func(t *testing.T) {
			original := testKeyring(t)
			fresh, err := NewStaticKeyring(TSIGKey{Name: xfrKey, Algorithm: dns.HmacSHA256, Secret: otherSecret})
			if err != nil {
				t.Fatal(err)
			}
			empty, err := NewStaticKeyring()
			if err != nil {
				t.Fatal(err)
			}
			var current atomic.Pointer[StaticKeyring]
			var captures atomic.Int32
			stage := &startupKeysStage{entered: make(chan struct{}), release: make(chan struct{})}
			release := sync.OnceFunc(func() { close(stage.release) })
			t.Cleanup(release)
			f := startFixture(t, fixtureOpts{specs: []Spec{{Protocol: proto}}, tsig: original, stages: []pipeline.Stage{stage}, handler: func(h *Handler) {
				h.Keys = func(req *pipeline.Request) (*StaticKeyring, error) {
					captures.Add(1)
					if keys := current.Load(); keys != nil {
						return keys, nil
					}
					if req.Msg.IsTsig() != nil {
						return nil, errors.New("private source unknown")
					}
					return empty, nil
				}
			}})
			var conn net.Conn
			if proto == ProtoUDP || proto == ProtoTCP || proto == ProtoDoT {
				conn = captureNativeDial(t, f, proto)
			}
			var exchange func([]byte) ([]byte, error)
			if conn != nil {
				exchange = func(wire []byte) ([]byte, error) { return captureNativeExchange(conn, proto, wire) }
			} else {
				exchange = captureEncryptedConnection(t, f, proto)
			}
			initialExchange := exchange
			if proto == ProtoDoH || proto == ProtoDoH3 {
				// The held-connection helper requires signed-response privacy.
				// Unsigned success keeps its ordinary HTTP freshness contract.
				initialExchange = func(wire []byte) ([]byte, error) { return encryptedExchange(t, f, proto, http.MethodPost, wire), nil }
			}
			q := query("unsigned.example.org.")
			q.Id = 0
			wire, err := q.Pack()
			if err != nil {
				t.Fatal(err)
			}
			out, err := initialExchange(wire)
			if err != nil || encryptedResponse(t, out).Rcode != dns.RcodeSuccess {
				t.Fatal("unsigned request blocked by unknown source")
			}
			q.SetTsig(xfrKey, dns.HmacSHA256, 30, time.Now().Unix())
			wire, _ = encryptedRequest(t, q, original)
			out, err = initialExchange(wire)
			if err != nil {
				t.Fatal("missing refusal")
			}
			refused := encryptedResponse(t, out)
			if refused.Rcode != dns.RcodeServerFailure || refused.IsTsig() != nil || len(refused.Answer) != 0 {
				t.Fatal("unknown source fell back to listener signer")
			}
			current.Store(original)
			q = query("held.example.org.")
			q.Id = 0
			q.SetTsig(xfrKey, dns.HmacSHA256, 30, time.Now().Unix())
			wire, mac := encryptedRequest(t, q, original)
			type result struct {
				wire []byte
				err  error
			}
			done := make(chan result, 1)
			go func() { b, e := exchange(wire); done <- result{b, e} }()
			select {
			case <-stage.entered:
			case <-time.After(2 * time.Second):
				t.Fatal("original request never entered")
			}
			current.Store(fresh)
			release()
			select {
			case got := <-done:
				if got.err != nil || !encryptedMACValid(t, got.wire, mac, xfrSecret) || encryptedMACValid(t, got.wire, mac, otherSecret) {
					t.Fatal("held response adopted a replacement key")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("held response did not join")
			}
			if captures.Load() != 3 {
				t.Fatal("request recaptured provider")
			}
		})
	}
}
