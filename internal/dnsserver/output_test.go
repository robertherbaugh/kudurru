package dnsserver

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// The client must receive all DNS bytes while Emit still holds its scope.
// HTTP requires explicit body/header flush; DoQ also requires stream FIN.
// This checks the actual transport handoff, not Store retirement admission.
type observedOutput struct {
	emitted  chan struct{}
	release  chan struct{}
	finished chan struct{}
	once     sync.Once
	calls    atomic.Int32
}

func (o *observedOutput) Emit(fn func() error) error {
	if o.calls.Add(1) != 1 {
		return errors.New("output reused")
	}
	defer close(o.finished)
	err := fn()
	close(o.emitted)
	<-o.release
	return err
}
func (o *observedOutput) Next() error { return errors.New("unexpected continuation") }
func (o *observedOutput) unblock()    { o.once.Do(func() { close(o.release) }) }

func TestOutputActualTransportDataInsideAdmission(t *testing.T) {
	for _, tc := range []struct {
		name     string
		protocol Protocol
		http1    bool
	}{
		{"udp", ProtoUDP, false}, {"tcp", ProtoTCP, false}, {"dot", ProtoDoT, false},
		{"http1", ProtoDoH, true}, {"http2", ProtoDoH, false}, {"http3", ProtoDoH3, false}, {"doq", ProtoDoQ, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := &observedOutput{emitted: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
			defer output.unblock()
			f := startFixture(t, fixtureOpts{specs: []Spec{{Protocol: tc.protocol}}, stages: []pipeline.Stage{localAnswer{}}, handler: func(h *Handler) {
				fixedTransportCapture(testKeyring(t))(h)
				h.Output = func(req *pipeline.Request) (Output, error) {
					if req.Serving == nil || req.Ctx.Err() != nil {
						return nil, pipeline.ErrServingCapture
					}
					return output, nil
				}
			}})
			q := query("scoped.example.")
			q.Id = 0
			wire, err := q.Pack()
			if err != nil {
				t.Fatal(err)
			}
			var answer []byte
			switch tc.protocol {
			case ProtoUDP, ProtoTCP, ProtoDoT:
				conn := captureNativeDial(t, f, tc.protocol)
				answer, err = captureNativeExchange(conn, tc.protocol, wire)
				if err != nil {
					t.Fatal("actual native scoped write", err)
				}
			case ProtoDoQ:
				answer = encryptedExchange(t, f, tc.protocol, http.MethodPost, wire, tc.http1)
			default:
				var client *http.Client
				if tc.protocol == ProtoDoH {
					client = dohClient(t, f, !tc.http1)
				} else {
					transport := &http3.Transport{TLSClientConfig: (&tlsPool{f.cert}).client()}
					defer func() { _ = transport.Close() }()
					client = &http.Client{Transport: transport, Timeout: 3 * time.Second}
				}
				request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://"+f.addr(tc.protocol)+DefaultDoHPath, bytes.NewReader(wire))
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Content-Type", dohMIME)
				response, err := client.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = response.Body.Close() }()
				version := 2
				if tc.http1 {
					version = 1
				}
				if tc.protocol == ProtoDoH3 {
					version = 3
				}
				if response.StatusCode != http.StatusOK || response.ProtoMajor != version || response.ContentLength <= 0 || response.ContentLength > maxDNSMessage {
					t.Fatal("actual HTTP header/length mismatch")
				}
				answer = make([]byte, int(response.ContentLength))
				if _, err := io.ReadFull(response.Body, answer); err != nil {
					t.Fatal("actual flushed HTTP body", err)
				}
			}
			checkA(t, encryptedResponse(t, answer))
			select {
			case <-output.emitted:
			case <-time.After(time.Second):
				t.Fatal("response did not complete inside callback")
			}
			select {
			case <-output.finished:
				t.Fatal("response completed after admission released")
			default:
			}
			output.unblock()
			select {
			case <-output.finished:
			case <-time.After(time.Second):
				t.Fatal("output did not join")
			}
		})
	}
}

func TestOutputEarlierStreamDeadlineSurvivesRelativeWrapper(t *testing.T) {
	left, right := net.Pipe()
	defer func() { _ = left.Close(); _ = right.Close() }()
	conn := &writeDeadlineConn{Conn: left, timeout: time.Second}
	if err := conn.SetWriteDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err := conn.Write([]byte("must not extend"))
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatal("relative write extended original expired deadline", err)
	}
}

func TestOutputBareResolveRefusesUnscopedExport(t *testing.T) {
	h := &Handler{Chain: pipeline.MustNew(localAnswer{}), Output: func(*pipeline.Request) (Output, error) { t.Fatal("unscoped capture"); return nil, nil }}
	if m := h.Resolve(context.Background(), query("scoped.example."), pipeline.Client{}); len(m.Answer) != 0 {
		t.Fatal("bare resolver exported guarded data")
	}
}

func TestOutputUDPOriginalDeadlineBoundsSharedSocketQueue(t *testing.T) {
	socket, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = socket.Close() }()
	packet := &nativePacket{PacketConn: socket, writeGate: make(chan struct{}, 1)}
	packet.writeGate <- struct{}{}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	deadline, _ := ctx.Deadline()
	frame := &nativeFrame{Addr: socket.LocalAddr(), deadline: deadline, writeCtx: ctx}
	if _, err := packet.WriteTo([]byte("expired"), frame); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("queued datagram ignored original lifetime", err)
	}
	<-packet.writeGate
	if _, err := packet.WriteTo([]byte("expired"), frame); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("expired datagram acquired a later socket deadline", err)
	}
}

// A later admission error is independent of an already completed local write.
// This controls the post-handoff boundary without changing transport behavior.
type completedErrorOutput struct{}

func (completedErrorOutput) Emit(fn func() error) error {
	if err := fn(); err != nil {
		return err
	}
	return errors.New("later original admission error")
}
func (completedErrorOutput) Next() error { return errors.New("terminal output") }

func TestOutputCompletedEncryptedHandoffKeepsBytesAfterLaterError(t *testing.T) {
	for _, tc := range []struct {
		name     string
		protocol Protocol
		http1    bool
	}{
		{"http1", ProtoDoH, true}, {"http2", ProtoDoH, false}, {"http3", ProtoDoH3, false}, {"doq", ProtoDoQ, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := startFixture(t, fixtureOpts{specs: []Spec{{Protocol: tc.protocol}}, stages: []pipeline.Stage{localAnswer{}}, handler: func(h *Handler) {
				fixedTransportCapture(testKeyring(t))(h)
				h.Output = func(*pipeline.Request) (Output, error) { return completedErrorOutput{}, nil }
			}})
			q := query("completed.example.")
			q.Id = 0
			wire, err := q.Pack()
			if err != nil {
				t.Fatal(err)
			}
			out := encryptedExchange(t, f, tc.protocol, http.MethodPost, wire, tc.http1)
			checkA(t, encryptedResponse(t, out))
		})
	}
}

type expiredOutputAnswer struct{}

func (expiredOutputAnswer) Name() string { return "local" }
func (expiredOutputAnswer) Serve(req *pipeline.Request, next pipeline.Next) (*pipeline.Response, error) {
	response, err := localAnswer{}.Serve(req, next)
	response.Security = pipeline.Security{State: pipeline.ValidationSecure, ValidatedAt: time.Now().Add(-time.Minute), ValidUntil: time.Now().Add(-time.Second)}
	return response, err
}

func TestOutputRetainsActualValidationDeadlineThroughEncoding(t *testing.T) {
	owner := &transportTestOwner{keys: testKeyring(t)}
	current := new(atomic.Pointer[transportTestOwner])
	current.Store(owner)
	h := &Handler{Chain: pipeline.MustNew(expiredOutputAnswer{}), Logger: quietLogger}
	testTransportCapture(current)(h)
	h.Output = func(*pipeline.Request) (Output, error) { return completedErrorOutput{}, nil }
	q := query("expired-proof.example.")
	wire, err := q.Pack()
	if err != nil {
		t.Fatal(err)
	}
	w := &capturedFailWriter{owner: owner, frame: &nativeFrame{Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 4000}, wire: wire, received: time.Now()}}
	serveCapturedNative(h, &env{}, pipeline.ProtoTCP, w, q)
	if w.writes != 0 || owner.refs.Load() != 0 {
		t.Fatal("expired original validation reached native output or leaked owner")
	}
	out, err := h.resolveEncrypted(t.Context(), &env{}, wire, q, pipeline.Client{Proto: pipeline.ProtoDoH})
	if err != nil || out == nil {
		t.Fatal("prepare original encoded proof", err)
	}
	defer out.Close()
	if out.current() {
		t.Fatal("encoded response lost original validation expiry")
	}
	if err := out.emit(func() error { t.Fatal("expired proof entered encrypted write"); return nil }); err == nil {
		t.Fatal("expired proof admitted")
	}
}
