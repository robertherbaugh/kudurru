package dnsserver

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

type capturedForeignSignatureStage struct{}

func (capturedForeignSignatureStage) Name() string { return "local" }
func (capturedForeignSignatureStage) Serve(req *pipeline.Request, _ pipeline.Next) (*pipeline.Response, error) {
	r, err := localAnswer{}.Serve(req, nil)
	r.Msg.SetTsig("foreign.example.", dns.HmacSHA512, tsigFudge, time.Now().Unix())
	return r, err
}

func TestCapturedNativeDiscardsForeignResponseSignature(t *testing.T) {
	for _, proto := range []Protocol{ProtoUDP, ProtoTCP, ProtoDoT} {
		t.Run(string(proto), func(t *testing.T) {
			keys := testKeyring(t)
			f := startFixture(t, fixtureOpts{specs: []Spec{{Protocol: proto}}, stages: []pipeline.Stage{capturedForeignSignatureStage{}}, handler: fixedTransportCapture(keys)})
			conn := captureNativeDial(t, f, proto)
			for _, signed := range []bool{false, true} {
				q := query("foreign.example.")
				var wire []byte
				var mac string
				if signed {
					q.SetTsig(xfrKey, dns.HmacSHA256, tsigFudge, time.Now().Unix())
					wire, mac = encryptedRequest(t, q, keys)
				} else {
					wire, _ = q.Pack()
				}
				out, err := captureNativeExchange(conn, proto, wire)
				if err != nil {
					t.Fatal("foreign response signature prevented emission")
				}
				m := encryptedResponse(t, out)
				checkA(t, m)
				if !signed {
					if m.IsTsig() != nil {
						t.Fatal("unsigned query adopted foreign response signature")
					}
				} else if m.IsTsig() == nil || m.IsTsig().Hdr.Name != xfrKey || m.IsTsig().Algorithm != dns.HmacSHA256 || !encryptedMACValid(t, out, mac, xfrSecret) {
					t.Fatal("signed query adopted foreign signer or algorithm")
				}
			}
		})
	}
}

type capturedFailWriter struct {
	failingWriter
	frame *nativeFrame
	owner *transportTestOwner
	alive bool
}

func (w *capturedFailWriter) RemoteAddr() net.Addr { return w.frame }
func (w *capturedFailWriter) Write([]byte) (int, error) {
	w.writes++
	w.alive = w.owner.refs.Load() == 1
	return 0, errors.New("closed test socket")
}

func TestCapturedNativeWriteFailureReleasesOriginal(t *testing.T) {
	for _, proto := range []pipeline.Proto{pipeline.ProtoUDP, pipeline.ProtoTCP, pipeline.ProtoDoT} {
		t.Run(proto.String(), func(t *testing.T) {
			keys := testKeyring(t)
			owner := &transportTestOwner{keys: keys}
			current := new(atomic.Pointer[transportTestOwner])
			current.Store(owner)
			stage := &cleanupAnswer{}
			h := &Handler{Chain: pipeline.MustNew(stage), Logger: quietLogger}
			testTransportCapture(current)(h)
			q := query("failure.example.")
			q.SetTsig(xfrKey, dns.HmacSHA256, tsigFudge, time.Now().Unix())
			wire, _ := encryptedRequest(t, q, keys)
			q = encryptedResponse(t, wire) // same parsed request supplied by the native decoder
			w := &capturedFailWriter{owner: owner, frame: &nativeFrame{Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 4000}, wire: wire, received: time.Now()}}
			serveCapturedNative(h, &env{}, proto, w, q)
			if w.writes != 1 || !w.alive || stage.closed.Load() != 1 || owner.refs.Load() != 0 {
				t.Fatal("write failure lost original emission owner or failed cleanup/join")
			}
		})
	}
}

func TestCapturedEncryptedCancellationBeforeEmission(t *testing.T) {
	owner := &transportTestOwner{keys: testKeyring(t)}
	current := new(atomic.Pointer[transportTestOwner])
	current.Store(owner)
	h := &Handler{Chain: pipeline.MustNew(localAnswer{}), Logger: quietLogger}
	testTransportCapture(current)(h)
	q := query("cancel.example.")
	q.SetTsig(xfrKey, dns.HmacSHA256, tsigFudge, time.Now().Unix())
	wire, mac := encryptedRequest(t, q, owner.keys)
	q = encryptedResponse(t, wire)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	out, err := h.resolveEncrypted(ctx, &env{}, wire, q, pipeline.Client{Proto: pipeline.ProtoDoH})
	if err != nil || !out.current() || owner.refs.Load() != 1 || !encryptedMACValid(t, out.wire, mac, xfrSecret) {
		t.Fatal("encoded response did not retain original owner through emission")
	}
	defer out.Close()
	cancel()
	if out.current() {
		t.Fatal("canceled original response remains eligible for emission")
	}
	out.Close()
	out.Close()
	if owner.refs.Load() != 0 {
		t.Fatal("canceled encoded response leaked original owner")
	}
}

func TestCapturedPartialFailureReleasesOriginal(t *testing.T) {
	for _, keysFail := range []bool{false, true} {
		owner := &transportTestOwner{keys: testKeyring(t)}
		current := new(atomic.Pointer[transportTestOwner])
		current.Store(owner)
		h := &Handler{}
		testTransportCapture(current)(h)
		if keysFail {
			h.Keys = func(*pipeline.Request) (*StaticKeyring, error) { return nil, pipeline.ErrServingCapture }
		} else {
			capture := h.Capture
			h.Capture = func(req *pipeline.Request) (pipeline.ServingLease, error) {
				lease, _ := capture(req)
				return lease, pipeline.ErrServingCapture
			}
		}
		_, _, closeCapture, err := h.captureTransport(t.Context(), query("failed.example."), pipeline.Client{}, time.Now())
		if err == nil || owner.refs.Load() != 1 {
			t.Fatal("partial capture not retained for cleanup")
		}
		closeCapture()
		closeCapture()
		if owner.refs.Load() != 0 {
			t.Fatal("partial capture leaked ownership")
		}
	}
}

func TestCapturedNativeIgnoredFrameCannotRetainOrReuseLease(t *testing.T) {
	for _, proto := range []Protocol{ProtoUDP, ProtoTCP, ProtoDoT} {
		t.Run(string(proto), func(t *testing.T) {
			keys := testKeyring(t)
			owner := &transportTestOwner{keys: keys}
			current := new(atomic.Pointer[transportTestOwner])
			current.Store(owner)
			var captured atomic.Int64
			f := startFixture(t, fixtureOpts{specs: []Spec{{Protocol: proto}}, stages: []pipeline.Stage{localAnswer{}}, handler: func(h *Handler) {
				testTransportCapture(current)(h)
				capture := h.Capture
				h.Capture = func(req *pipeline.Request) (pipeline.ServingLease, error) { captured.Add(1); return capture(req) }
			}})
			conn := captureNativeDial(t, f, proto)
			ignored := query("ignored.example.")
			ignored.Response = true // miekg MsgIgnore before handler/capture
			ignoredWire, _ := ignored.Pack()
			if proto != ProtoUDP {
				ignoredWire = append(lenPrefix(ignoredWire), ignoredWire...)
			}
			if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Write(ignoredWire); err != nil {
				t.Fatal("write ignored original frame")
			}
			q := query("valid.example.")
			q.SetTsig(xfrKey, dns.HmacSHA256, tsigFudge, time.Now().Unix())
			wire, mac := encryptedRequest(t, q, keys)
			out, err := captureNativeExchange(conn, proto, wire)
			if err != nil || !encryptedMACValid(t, out, mac, xfrSecret) || captured.Load() != 1 {
				t.Fatal("next valid message reused ignored frame or acquired excess leases")
			}
			checkA(t, encryptedResponse(t, out))
			// Reading the reply may precede the handler's final unwind.
			until := time.Now().Add(time.Second)
			for owner.refs.Load() != 0 && time.Now().Before(until) {
				time.Sleep(time.Millisecond)
			}
			if owner.refs.Load() != 0 {
				t.Fatal("ignored or following valid frame leaked a lease")
			}
		})
	}
}
