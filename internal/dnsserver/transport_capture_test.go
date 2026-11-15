package dnsserver

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

type transportTestOwner struct {
	keys  *StaticKeyring
	label string
	refs  atomic.Int64
}
type transportTestLease struct {
	owner    *transportTestOwner
	released atomic.Bool
}

func (l *transportTestLease) Value() any { return l.owner }
func (l *transportTestLease) Clone() (pipeline.ServingLease, error) {
	l.owner.refs.Add(1)
	return &transportTestLease{owner: l.owner}, nil
}
func (l *transportTestLease) Release() {
	if l.released.CompareAndSwap(false, true) {
		l.owner.refs.Add(-1)
	}
}
func testTransportCapture(current *atomic.Pointer[transportTestOwner]) func(*Handler) {
	return func(h *Handler) {
		h.Capture = func(*pipeline.Request) (pipeline.ServingLease, error) {
			owner := current.Load()
			owner.refs.Add(1)
			return &transportTestLease{owner: owner}, nil
		}
		h.Keys = func(req *pipeline.Request) (*StaticKeyring, error) {
			return req.Serving.Value().(*transportTestOwner).keys, nil
		}
	}
}
func fixedTransportCapture(keys *StaticKeyring) func(*Handler) {
	current := new(atomic.Pointer[transportTestOwner])
	current.Store(&transportTestOwner{keys: keys, label: "fixed"})
	return testTransportCapture(current)
}

// One actual UDP/TCP/TLS socket is reused, so connection-scoped providers cannot
// pass the fresh-message assertion after publication replacement.
func captureNativeDial(t *testing.T, f *fixture, proto Protocol) net.Conn {
	t.Helper()
	var conn net.Conn
	var err error
	if proto == ProtoDoT {
		conn, err = tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", f.addr(proto), (&tlsPool{f.cert}).client("dot"))
	} else {
		conn, err = net.DialTimeout(string(proto), f.addr(proto), time.Second)
	}
	if err != nil {
		t.Fatal("dial capture fixture")
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}
func captureNativeExchange(conn net.Conn, proto Protocol, wire []byte) ([]byte, error) {
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return nil, err
	}
	if proto == ProtoUDP {
		if _, err := conn.Write(wire); err != nil {
			return nil, err
		}
		result := make([]byte, maxDNSMessage)
		n, err := conn.Read(result)
		return result[:n], err
	}
	if _, err := conn.Write(append(lenPrefix(wire), wire...)); err != nil {
		return nil, err
	}
	var header [2]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return nil, err
	}
	result := make([]byte, int(binary.BigEndian.Uint16(header[:])))
	_, err := io.ReadFull(conn, result)
	return result, err
}

type capturePolicyStage struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (*capturePolicyStage) Name() string { return "authoritative" }
func (s *capturePolicyStage) Serve(req *pipeline.Request, _ pipeline.Next) (*pipeline.Response, error) {
	owner := req.Serving.Value().(*transportTestOwner)
	if req.Question().Name == "held.example.org." {
		s.once.Do(func() { close(s.entered) })
		select {
		case <-s.release:
		case <-req.Ctx.Done():
		}
	}
	response := pipeline.Reply(req, dns.RcodeSuccess, pipeline.StatusLocalRecord)
	rr, _ := dns.NewRR(req.Question().Name + " 60 IN TXT " + owner.label)
	response.Msg.Answer = []dns.RR{rr}
	return response, nil
}

func TestCapturedTSIGHeldPublicationAndFreshSameConnection(t *testing.T) {
	for _, proto := range []Protocol{ProtoUDP, ProtoTCP, ProtoDoT, ProtoDoH, ProtoDoQ, ProtoDoH3} {
		t.Run(string(proto), func(t *testing.T) {
			oldRing := testKeyring(t)
			newRing, err := NewStaticKeyring(TSIGKey{Name: xfrKey, Algorithm: dns.HmacSHA256, Secret: otherSecret, CredentialEpoch: "new-incarnation"})
			if err != nil {
				t.Fatal(err)
			}
			old, fresh := &transportTestOwner{keys: oldRing, label: "old"}, &transportTestOwner{keys: newRing, label: "fresh"}
			current := new(atomic.Pointer[transportTestOwner])
			current.Store(old)
			stage := &capturePolicyStage{entered: make(chan struct{}), release: make(chan struct{})}
			release := sync.OnceFunc(func() { close(stage.release) })
			t.Cleanup(release)
			f := startFixture(t, fixtureOpts{specs: []Spec{{Protocol: proto}}, tsig: oldRing, stages: []pipeline.Stage{stage}, handler: testTransportCapture(current)})
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
			request := query("held.example.org.")
			request.Id = 0
			request.SetTsig(xfrKey, dns.HmacSHA256, 30, time.Now().Unix())
			wire, mac := encryptedRequest(t, request, oldRing)
			type result struct {
				wire []byte
				err  error
			}
			done := make(chan result, 1)
			go func() { out, err := exchange(wire); done <- result{out, err} }()
			select {
			case <-stage.entered:
			case <-time.After(2 * time.Second):
				t.Fatal("held original did not enter")
			}
			current.Store(fresh)
			if old.refs.Load() != 1 {
				t.Fatal("original publication not retained while held")
			}
			release()
			var held result
			select {
			case held = <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("held query failed to join")
			}
			if held.err != nil || len(held.wire) == 0 {
				t.Fatal("held query failed emission")
			}
			response := encryptedResponse(t, held.wire)
			if response.Rcode != dns.RcodeSuccess || len(response.Answer) != 1 || response.Answer[0].(*dns.TXT).Txt[0] != "old" || !encryptedMACValid(t, held.wire, mac, xfrSecret) {
				t.Fatal("held query adopted successor policy/provider")
			}
			request = query("fresh.example.org.")
			request.Id = 0
			request.SetTsig(xfrKey, dns.HmacSHA256, 30, time.Now().Unix())
			newWire, newMAC := encryptedRequest(t, request, newRing)
			out, err := exchange(newWire)
			if err != nil {
				t.Fatal("fresh same-connection exchange failed")
			}
			response = encryptedResponse(t, out)
			if response.Rcode != dns.RcodeSuccess || len(response.Answer) != 1 || response.Answer[0].(*dns.TXT).Txt[0] != "fresh" || !encryptedMACValid(t, out, newMAC, otherSecret) || encryptedMACValid(t, out, newMAC, xfrSecret) {
				t.Fatal("fresh message retained connection's stale policy/provider")
			}
			oldWire, _ := encryptedRequest(t, request, oldRing)
			out, err = exchange(oldWire)
			if err != nil {
				t.Fatal("original credential refusal response missing")
			}
			response = encryptedResponse(t, out)
			if response.Rcode != dns.RcodeNotAuth || response.IsTsig() == nil || response.IsTsig().Error != dns.RcodeBadSig || response.IsTsig().MAC != "" {
				t.Fatal("successor accepted old credential")
			}
			// The network client may finish reading before the handler unwinds.
			deadline := time.NewTimer(time.Second)
			defer deadline.Stop()
			for old.refs.Load() != 0 || fresh.refs.Load() != 0 {
				select {
				case <-deadline.C:
					t.Fatal("request publication lease leaked")
				default:
					time.Sleep(time.Millisecond)
				}
			}
		})
	}
}

type capturedStreamStage struct{ entered, release chan struct{} }

func (*capturedStreamStage) Name() string { return "authoritative" }
func (s *capturedStreamStage) Serve(req *pipeline.Request, _ pipeline.Next) (*pipeline.Response, error) {
	soa, _ := dns.NewRR("example.org. 60 IN SOA ns.example.org. hostmaster.example.org. 1 60 60 300 60")
	first := new(dns.Msg).SetReply(req.Msg)
	first.Answer = []dns.RR{soa}
	response := &pipeline.Response{Msg: first, Status: pipeline.StatusAuthoritative}
	response.Transfer = func(yield func(*dns.Msg) bool) {
		if !yield(first) {
			return
		}
		close(s.entered)
		select {
		case <-s.release:
		case <-req.Ctx.Done():
			return
		}
		middle := new(dns.Msg).SetReply(req.Msg)
		txt, _ := dns.NewRR("original.example.org. 60 IN TXT " + req.Serving.Value().(*transportTestOwner).label)
		middle.Answer = []dns.RR{txt}
		if !yield(middle) {
			return
		}
		_ = yield(first.Copy())
	}
	return response, nil
}

func TestCapturedTSIGTransferChainsOriginalProvider(t *testing.T) {
	for _, proto := range []Protocol{ProtoTCP, ProtoDoT} {
		t.Run(string(proto), func(t *testing.T) {
			keys := testKeyring(t)
			old := &transportTestOwner{keys: keys, label: "original"}
			current := new(atomic.Pointer[transportTestOwner])
			current.Store(old)
			stage := &capturedStreamStage{entered: make(chan struct{}), release: make(chan struct{})}
			release := sync.OnceFunc(func() { close(stage.release) })
			t.Cleanup(release)
			f := startFixture(t, fixtureOpts{specs: []Spec{{Protocol: proto}}, stages: []pipeline.Stage{stage}, handler: testTransportCapture(current)})
			q := new(dns.Msg).SetAxfr("example.org.")
			q.SetTsig(xfrKey, dns.HmacSHA256, 30, time.Now().Unix())
			transfer := &dns.Transfer{TsigProvider: TSIGProvider(keys), ReadTimeout: 2 * time.Second}
			if proto == ProtoDoT {
				transfer.TLS = (&tlsPool{f.cert}).client("dot")
			}
			envelopes, err := transfer.In(q, f.addr(proto))
			if err != nil {
				t.Fatal("start actual signed transfer")
			}
			defer func() { _ = transfer.Close() }()
			select {
			case <-stage.entered:
			case <-time.After(time.Second):
				t.Fatal("first transfer frame not written")
			}
			rotated, _ := NewStaticKeyring(TSIGKey{Name: xfrKey, Algorithm: dns.HmacSHA256, Secret: otherSecret})
			current.Store(&transportTestOwner{keys: rotated, label: "successor"})
			if old.refs.Load() != 1 {
				t.Fatal("transfer lost original publication lease")
			}
			release()
			messages, records := 0, 0
			for env := range envelopes {
				if env.Error != nil {
					t.Fatal("transfer MAC chain rejected")
				}
				messages++
				records += len(env.RR)
				for _, rr := range env.RR {
					if txt, ok := rr.(*dns.TXT); ok && txt.Txt[0] != "original" {
						t.Fatal("transfer adopted successor publication")
					}
				}
			}
			if messages != 3 || records != 3 {
				t.Fatal("transfer incomplete")
			}
		})
	}
}

func TestCapturedNativeOriginalQueryDeadline(t *testing.T) {
	for _, proto := range []Protocol{ProtoUDP, ProtoTCP, ProtoDoT} {
		t.Run(string(proto), func(t *testing.T) {
			keys := testKeyring(t)
			stage := &capturePolicyStage{entered: make(chan struct{}), release: make(chan struct{})}
			configure := fixedTransportCapture(keys)
			f := startFixture(t, fixtureOpts{specs: []Spec{{Protocol: proto}}, stages: []pipeline.Stage{stage}, handler: func(h *Handler) { configure(h); h.QueryTimeout = 40 * time.Millisecond }})
			conn := captureNativeDial(t, f, proto)
			request := query("held.example.org.")
			request.SetTsig(xfrKey, dns.HmacSHA256, 30, time.Now().Unix())
			wire, _ := encryptedRequest(t, request, keys)
			_ = conn.SetDeadline(time.Now().Add(200 * time.Millisecond))
			if proto != ProtoUDP {
				wire = append(lenPrefix(wire), wire...)
			}
			if _, err := conn.Write(wire); err != nil {
				t.Fatal("write original-deadline query")
			}
			select {
			case <-stage.entered:
			case <-time.After(time.Second):
				t.Fatal("deadline query not captured")
			}
			// Stage returns when the ORIGINAL query context expires. It cannot
			// obtain a new timeout during resolve/sign/write after that point.
			result := make([]byte, 4096)
			if n, err := conn.Read(result); err == nil && n > 0 {
				t.Fatal("expired original query emitted a renewed signed response")
			}
		})
	}
}

// Keep the same QUIC connection/HTTP transport for all three original messages.
// Native same-socket tests alone cannot qualify encrypted multiplexed owners.
func captureEncryptedConnection(t *testing.T, f *fixture, proto Protocol) func([]byte) ([]byte, error) {
	t.Helper()
	if proto == ProtoDoQ {
		conn, err := doqDial(t, f.addr(proto), &tlsPool{f.cert})
		if err != nil {
			t.Fatal("open captured DoQ connection")
		}
		t.Cleanup(func() { closeQUIC(conn) })
		return func(wire []byte) ([]byte, error) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			st, err := conn.OpenStreamSync(ctx)
			if err != nil {
				return nil, err
			}
			defer func() { _ = st.Close() }()
			if err := st.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
				return nil, err
			}
			if _, err := st.Write(append(lenPrefix(wire), wire...)); err != nil {
				return nil, err
			}
			if err := st.Close(); err != nil {
				return nil, err
			}
			out, err := io.ReadAll(io.LimitReader(st, maxDNSMessage+3))
			if err != nil || len(out) < 2 || int(binary.BigEndian.Uint16(out)) != len(out)-2 {
				return nil, errors.New("invalid captured DoQ response")
			}
			return out[2:], nil
		}
	}
	var client *http.Client
	if proto == ProtoDoH3 {
		conn, err := quic.DialAddr(t.Context(), f.addr(proto), (&tlsPool{f.cert}).client("h3"), nil)
		if err != nil {
			t.Fatal("open captured HTTP3 connection")
		}
		t.Cleanup(func() { closeQUIC(conn) })
		var dials atomic.Int64
		transport := &http3.Transport{TLSClientConfig: (&tlsPool{f.cert}).client(), Dial: func(context.Context, string, *tls.Config, *quic.Config) (*quic.Conn, error) {
			if dials.Add(1) != 1 {
				return nil, errors.New("captured HTTP3 attempted successor connection")
			}
			return conn, nil
		}}
		t.Cleanup(func() { _ = transport.Close() })
		client = &http.Client{Transport: transport, Timeout: 3 * time.Second}
	} else {
		client = dohClient(t, f, true)
	}
	var original net.Conn
	return func(wire []byte) ([]byte, error) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://"+f.addr(proto)+DefaultDoHPath, bytes.NewReader(wire))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", dohMIME)
		// net/http reports actual HTTP2 TLS connection identity. quic-go does
		// not expose it through httptrace; the HTTP3 dialer above grants exactly
		// one preconnected QUIC connection and rejects every successor.
		var replaced bool
		if proto == ProtoDoH {
			req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
				if original == nil {
					original = info.Conn
				} else if original != info.Conn {
					replaced = true
				}
			}}))
		}
		response, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer func() { _ = response.Body.Close() }()
		want := 2
		if proto == ProtoDoH3 {
			want = 3
		}
		if replaced || response.ProtoMajor != want || response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "private, no-store" {
			return nil, errors.New("captured encrypted response lost transport or privacy")
		}
		out, err := io.ReadAll(io.LimitReader(response.Body, maxDNSMessage+1))
		if len(out) > maxDNSMessage {
			return nil, errors.New("oversized captured encrypted response")
		}
		return out, err
	}
}
