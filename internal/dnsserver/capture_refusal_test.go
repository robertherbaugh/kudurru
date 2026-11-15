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

type refusalWriter struct {
	failingWriter
	frame *nativeFrame
	wire  []byte
}

func (w *refusalWriter) RemoteAddr() net.Addr { return w.frame }
func (w *refusalWriter) Write(wire []byte) (int, error) {
	w.wire = append([]byte(nil), wire...)
	return len(wire), nil
}

func TestCapturedRefusalUnsignedAndOriginalLifetime(t *testing.T) {
	for _, failure := range []string{"capture", "output", "keys"} {
		t.Run(failure, func(t *testing.T) {
			owner := &transportTestOwner{keys: testKeyring(t)}
			current := new(atomic.Pointer[transportTestOwner])
			current.Store(owner)
			h := &Handler{Chain: pipeline.MustNew(localAnswer{}), Logger: quietLogger}
			testTransportCapture(current)(h)
			capture := h.Capture
			if failure == "capture" {
				h.Capture = func(req *pipeline.Request) (pipeline.ServingLease, error) {
					l, _ := capture(req)
					return l, pipeline.ErrServingCapture
				}
			}
			h.Output = func(*pipeline.Request) (Output, error) {
				if failure == "capture" {
					t.Error("failed capture reached output")
				}
				if failure == "output" {
					return nil, pipeline.ErrServingCapture
				}
				return completedErrorOutput{}, nil
			}
			h.Keys = func(*pipeline.Request) (*StaticKeyring, error) {
				if failure != "keys" {
					t.Error("failed admission reached signer selection")
				}
				return nil, pipeline.ErrServingCapture
			}
			q := query("private.example.")
			q.SetTsig(xfrKey, dns.HmacSHA256, tsigFudge, time.Now().Unix())
			wire, _ := encryptedRequest(t, q, owner.keys)
			q = encryptedResponse(t, wire)
			frame := &nativeFrame{Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 4000}, wire: wire, received: time.Now()}
			w := &refusalWriter{frame: frame}
			serveCapturedNative(h, &env{}, pipeline.ProtoTCP, w, q)
			m := encryptedResponse(t, w.wire)
			if m.Rcode != dns.RcodeServerFailure || len(m.Answer) != 0 || len(m.Ns) != 0 || len(m.Extra) != 0 || owner.refs.Load() != 0 || !frame.deadline.Equal(frame.received.Add(h.timeout())) {
				t.Fatal("native capture failure borrowed a signer/data or lost lifetime/cleanup")
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			out, err := h.resolveEncrypted(ctx, &env{}, wire, q, pipeline.Client{Proto: pipeline.ProtoDoH})
			if err != nil || out == nil {
				t.Fatal("capture refusal was dropped", err)
			}
			defer out.Close()
			m = encryptedResponse(t, out.wire)
			deadline, _ := ctx.Deadline()
			if m.Rcode != dns.RcodeServerFailure || len(m.Answer) != 0 || len(m.Ns) != 0 || len(m.Extra) != 0 || out.output != nil || out.cache != "no-store" || !out.deadline.Equal(deadline) || owner.refs.Load() != 1 {
				t.Fatal("encrypted refusal borrowed data/admission or lost original bound/lease")
			}
			if err := out.emit(func() error { return nil }); err != nil {
				t.Fatal("live empty refusal failed", err)
			}
			cancel()
			if err := out.emit(func() error { t.Error("canceled refusal emitted"); return nil }); !errors.Is(err, errOriginalDeadline) {
				t.Fatal("refusal lost cancellation", err)
			}
			out.Close()
			if owner.refs.Load() != 0 {
				t.Fatal("refusal retained source after close")
			}
			late := &refusalWriter{frame: &nativeFrame{Addr: frame.Addr, wire: wire, received: time.Now().Add(-2 * h.timeout())}}
			serveCapturedNative(h, &env{}, pipeline.ProtoTCP, late, q)
			if len(late.wire) != 0 || owner.refs.Load() != 0 {
				t.Fatal("expired native refusal emitted or leaked")
			}
		})
	}
}
