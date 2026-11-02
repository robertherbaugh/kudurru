package dnsserver

import (
	"bytes"
	"context"
	"math"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/clientid"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// capturedNativeWriter signs only with this request's original provider/MAC.
// Embedding preserves the native writer's real socket/session/error behavior.
type capturedNativeWriter struct {
	key, algorithm string
	dns.ResponseWriter
	provider                      dns.TsigProvider
	ctx                           context.Context
	mac                           string
	timersOnly                    bool
	status                        error
	verifiedAt, signatureDeadline time.Time
}

func (w *capturedNativeWriter) TsigStatus() error         { return w.status }
func (w *capturedNativeWriter) TsigTimersOnly(value bool) { w.timersOnly = value }
func (w *capturedNativeWriter) current() error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if !w.signatureDeadline.IsZero() {
		now := time.Now()
		if now.Before(w.verifiedAt) || !now.Before(w.signatureDeadline) {
			return errOriginalDeadline
		}
	}
	return nil
}
func (w *capturedNativeWriter) WriteMsg(message *dns.Msg) error {
	if err := w.current(); err != nil {
		return err
	}
	copyMessage := message.Copy()
	if w.status == nil {
		// A cached/upstream TSIG must never choose this hop's signer or MAC.
		for i := len(copyMessage.Extra) - 1; i >= 0; i-- {
			if copyMessage.Extra[i].Header().Rrtype == dns.TypeTSIG {
				copyMessage.Extra = append(copyMessage.Extra[:i], copyMessage.Extra[i+1:]...)
			}
		}
		if !w.signatureDeadline.IsZero() {
			copyMessage.SetTsig(w.key, w.algorithm, tsigFudge, time.Now().Unix())
		}
	}
	var wire []byte
	var err error
	if copyMessage.IsTsig() != nil {
		wire, w.mac, err = dns.TsigGenerateWithProvider(copyMessage, w.provider, w.mac, w.timersOnly)
	} else {
		wire, err = copyMessage.Pack()
	}
	if err != nil {
		return err
	}
	if err := w.current(); err != nil {
		return err
	}
	_, err = w.Write(wire)
	return err
}

func serveCapturedNative(h *Handler, e *env, proto pipeline.Proto, w dns.ResponseWriter, r *dns.Msg, clientIDDomain ...string) {
	frame, ok := w.RemoteAddr().(*nativeFrame)
	if !ok || !encryptedTSIGFrame(r, frame.wire) {
		_ = w.WriteMsg(new(dns.Msg).SetRcode(r, dns.RcodeFormatError))
		return
	}
	client := clientFrom(frame, proto)
	if client.ClientID == "" && proto == pipeline.ProtoDoT && len(clientIDDomain) > 0 {
		if stateWriter, ok := w.(dns.ConnectionStater); ok {
			if state := stateWriter.ConnectionState(); state != nil {
				client.ClientID = clientid.FromSNI(state.ServerName, clientIDDomain[0])
			}
		}
	}
	request, keys, cleanup, err := h.captureTransport(context.Background(), r, client, frame.received)
	defer cleanup()
	if err != nil {
		if request.Ctx.Err() == nil {
			_ = w.WriteMsg(servfail(r))
		}
		return
	}
	writer := &capturedNativeWriter{ResponseWriter: w, provider: TSIGProvider(keys), ctx: request.Ctx}
	localEnv := env{}
	if e != nil {
		localEnv = *e
	}
	localEnv.tsig = writer.provider
	if ts := r.IsTsig(); ts != nil {
		writer.mac = ts.MAC
		writer.key, writer.algorithm = ts.Hdr.Name, ts.Algorithm
		writer.status = dns.TsigVerifyWithProvider(bytes.Clone(frame.wire), writer.provider, "", false)
		if writer.status == nil {
			if ts.TimeSigned > math.MaxInt64-uint64(ts.Fudge) {
				writer.status = dns.ErrTime
			} else {
				now := time.Now()
				deadline := time.Unix(int64(ts.TimeSigned)+int64(ts.Fudge), 0) //nolint:gosec // bounded before conversion
				if !now.Before(deadline) {
					writer.status = dns.ErrTime
				} else {
					writer.verifiedAt, writer.signatureDeadline = now, deadline
				}
			}
		}
	}
	sig, ok := verifyTSIG(h, &localEnv, writer, r, &client)
	if !ok {
		return
	}
	ctx := request.Ctx
	if sig.proof != nil {
		ctx = context.WithValue(ctx, verifiedTSIGKey{}, sig.proof)
	}
	padding := paddingFor(r, proto)
	message, stream, finish := h.resolve(ctx, r, client)
	defer finish()
	if message == nil {
		if proto != pipeline.ProtoUDP {
			_ = w.Close()
		}
		return
	}
	if stream != nil && proto != pipeline.ProtoUDP {
		writeStream(h, writer, stream, sig, padding)
		return
	}
	message = message.Copy()
	if proto == pipeline.ProtoUDP {
		size := dns.MinMsgSize
		if opt := r.IsEdns0(); opt != nil && int(opt.UDPSize()) > size {
			size = int(opt.UDPSize())
		}
		if sig.signed() {
			size -= tsigReserve
		}
		message.Truncate(size)
	}
	sig.sign(message)
	if err := writer.WriteMsg(padding.apply(message)); err != nil {
		h.logger().Debug("write failed", "err", err, "proto", proto.String())
	}
}
