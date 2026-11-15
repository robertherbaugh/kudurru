package network_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
)

func TestEncryptedTSIGCannotCrossProductionMutationGates(t *testing.T) {
	f := newFixture(t, public.ZonePolicy{})
	var captures atomic.Int64
	registry, tlsConfig := nativeListeners(t, f, func(*pipeline.Request, string) (pr.VerifiedUpdate, error) {
		captures.Add(1)
		return pr.VerifiedUpdate{}, pr.ErrUnavailable
	}, nil, dnsserver.ProtoDoH, dnsserver.ProtoDoQ, dnsserver.ProtoDoH3)
	before, err := f.s.ReadGeneration(t.Context())
	check(t, err)
	keys := ring(t, "test-original-secret")
	for _, proto := range []dnsserver.Protocol{dnsserver.ProtoDoH, dnsserver.ProtoDoQ, dnsserver.ProtoDoH3} {
		t.Run(string(proto), func(t *testing.T) {
			for _, opcode := range []int{dns.OpcodeUpdate, dns.OpcodeNotify} {
				m := update(t, "example.", "held.example. 60 IN A 192.0.2.7", false)
				if opcode == dns.OpcodeNotify {
					m = new(dns.Msg).SetNotify("example.")
				}
				m.Id = 0
				m.SetTsig("key.", dns.HmacSHA256, 30, time.Now().Unix())
				wire, mac, err := dns.TsigGenerateWithProvider(m, dnsserver.TSIGProvider(keys), "", false)
				check(t, err)
				var out []byte
				if proto == dnsserver.ProtoDoQ {
					ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
					defer cancel()
					cfg := tlsConfig.Clone()
					cfg.NextProtos = []string{"doq"}
					conn, err := quic.DialAddr(ctx, registry.Addr(proto).String(), cfg, &quic.Config{})
					check(t, err)
					defer func() { _ = conn.CloseWithError(0, "") }()
					st, err := conn.OpenStreamSync(ctx)
					check(t, err)
					_ = st.SetDeadline(time.Now().Add(3 * time.Second))
					_, err = st.Write(binary.BigEndian.AppendUint16(nil, uint16(len(wire)))) //nolint:gosec // small fixed fixture
					check(t, err)
					_, err = st.Write(wire)
					check(t, err)
					check(t, st.Close())
					b, err := io.ReadAll(io.LimitReader(st, 65538))
					check(t, err)
					if len(b) < 2 || int(binary.BigEndian.Uint16(b)) != len(b)-2 {
						t.Fatal("mutation refusal has invalid DoQ framing")
					}
					out = b[2:]
				} else {
					var transport http.RoundTripper
					if proto == dnsserver.ProtoDoH {
						tr := &http.Transport{TLSClientConfig: tlsConfig.Clone(), ForceAttemptHTTP2: true}
						t.Cleanup(tr.CloseIdleConnections)
						transport = tr
					} else {
						tr := &http3.Transport{TLSClientConfig: tlsConfig.Clone()}
						t.Cleanup(func() { _ = tr.Close() })
						transport = tr
					}
					client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
					req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://"+registry.Addr(proto).String()+"/dns-query", bytes.NewReader(wire))
					check(t, err)
					req.Header.Set("Content-Type", "application/dns-message")
					r, err := client.Do(req)
					check(t, err)
					if r.StatusCode != http.StatusOK {
						t.Fatal("mutation refusal missing DNS response")
					}
					out, err = io.ReadAll(io.LimitReader(r.Body, 65536))
					check(t, err)
					_ = r.Body.Close()
				}
				r := new(dns.Msg)
				if r.Unpack(out) != nil || r.Rcode != dns.RcodeRefused || len(r.Answer) != 0 || r.IsTsig() == nil || r.IsTsig().Error != 0 {
					t.Fatal("authenticated encrypted mutation escaped original transport refusal")
				}
				if dns.TsigVerifyWithProvider(bytes.Clone(out), dnsserver.TSIGProvider(keys), mac, false) != nil {
					t.Fatal("production mutation refusal lost original transport signature")
				}
			}
		})
	}
	after, err := f.s.ReadGeneration(t.Context())
	check(t, err)
	if captures.Load() != 0 || !before.Generation().Same(after.Generation()) {
		t.Fatal("refused encrypted mutation reached native issuer or changed durable state")
	}
}
