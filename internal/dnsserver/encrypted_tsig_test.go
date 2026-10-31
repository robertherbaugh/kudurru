package dnsserver

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/quic-go/quic-go/http3"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
)

type encryptedObservation struct {
	client pipeline.Client
	id     uint16
	native bool
	tsigs  int
}

type encryptedAnswer struct {
	mu      sync.Mutex
	seen    []encryptedObservation
	entered chan struct{}
	resume  chan struct{}
	block   bool
}

func (*encryptedAnswer) Name() string { return "authoritative" }
func (s *encryptedAnswer) Serve(req *pipeline.Request, _ pipeline.Next) (*pipeline.Response, error) {
	_, update := VerifiedTSIGFor(req, time.Now())
	_, notify := VerifiedNotifyFor(req, time.Now())
	count := 0
	for _, rr := range req.Msg.Extra {
		if rr.Header().Rrtype == dns.TypeTSIG {
			count++
		}
	}
	s.mu.Lock()
	s.seen = append(s.seen, encryptedObservation{req.Client, req.Msg.Id, update || notify, count})
	block := s.block
	s.block = false
	s.mu.Unlock()
	if block {
		close(s.entered)
		select {
		case <-s.resume:
		case <-req.Ctx.Done():
		}
	}
	if req.Msg.Opcode != dns.OpcodeQuery {
		return pipeline.Reply(req, dns.RcodeRefused, pipeline.StatusRefusedACL), nil
	}
	return localAnswer{}.Serve(req, nil)
}

func (s *encryptedAnswer) observations() []encryptedObservation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]encryptedObservation(nil), s.seen...)
}

// The clients below send exact signed wire bytes on real TLS/HTTP2, QUIC and
// HTTP3 listeners; a DNS proxy that rewrites the ID or TSIG cannot stand in for
// RFC 8945 verification. No assertions print messages, MACs or provider errors.
func encryptedExchange(t *testing.T, f *fixture, proto Protocol, method string, wire []byte, http1 ...bool) []byte {
	t.Helper()
	if proto == ProtoDoQ {
		conn, err := doqDial(t, f.addr(proto), &tlsPool{f.cert})
		if err != nil {
			t.Fatal("open actual DoQ connection")
		}
		defer closeQUIC(conn)
		ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
		defer cancel()
		st, err := conn.OpenStreamSync(ctx)
		if err != nil {
			t.Fatal("open actual DoQ stream")
		}
		_ = st.SetDeadline(time.Now().Add(4 * time.Second))
		if _, err := st.Write(append(lenPrefix(wire), wire...)); err != nil {
			t.Fatal("write exact DoQ request")
		}
		_ = st.Close()
		b, err := io.ReadAll(io.LimitReader(st, maxDNSMessage+3))
		if err != nil || len(b) < 2 || int(binary.BigEndian.Uint16(b)) != len(b)-2 {
			t.Fatal("read framed DoQ response")
		}
		return b[2:]
	}
	var c *http.Client
	if proto == ProtoDoH {
		c = dohClient(t, f, len(http1) == 0 || !http1[0])
	} else {
		tr := &http3.Transport{TLSClientConfig: (&tlsPool{f.cert}).client()}
		t.Cleanup(func() { _ = tr.Close() })
		c = &http.Client{Transport: tr, Timeout: 4 * time.Second}
	}
	url := "https://" + f.addr(proto) + DefaultDoHPath
	var body io.Reader = bytes.NewReader(wire)
	if method == http.MethodGet {
		url += "?dns=" + base64.RawURLEncoding.EncodeToString(wire)
		body = nil
	}
	req, err := http.NewRequestWithContext(t.Context(), method, url, body)
	if err != nil {
		t.Fatal("construct actual encrypted HTTP request")
	}
	req.Header.Set("Content-Type", dohMIME)
	r, err := c.Do(req)
	if err != nil {
		t.Fatal("actual encrypted HTTP exchange failed")
	}
	defer func() { _ = r.Body.Close() }()
	expectedHTTP := 2
	if len(http1) != 0 && http1[0] {
		expectedHTTP = 1
	}
	if r.StatusCode != http.StatusOK || (proto == ProtoDoH && r.ProtoMajor != expectedHTTP) || (proto == ProtoDoH3 && r.ProtoMajor != 3) {
		t.Fatal("encrypted listener did not use expected HTTP version/status")
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, maxDNSMessage+1))
	if err != nil || len(b) > maxDNSMessage {
		t.Fatal("read bounded encrypted HTTP response")
	}
	q := new(dns.Msg)
	if q.Unpack(wire) == nil && q.IsTsig() != nil && !strings.Contains(r.Header.Get("Cache-Control"), "no-store") {
		t.Fatal("TSIG response was cacheable")
	}
	return b
}

func encryptedRequest(t *testing.T, msg *dns.Msg, keys *StaticKeyring) ([]byte, string) {
	t.Helper()
	if msg.IsTsig() == nil {
		b, err := msg.Pack()
		if err != nil {
			t.Fatal("pack unsigned fixture request")
		}
		return b, ""
	}
	b, mac, err := dns.TsigGenerateWithProvider(msg.Copy(), TSIGProvider(keys), "", false)
	if err != nil {
		t.Fatal("sign fixture request")
	}
	return b, mac
}

func encryptedResponse(t *testing.T, b []byte) *dns.Msg {
	t.Helper()
	m := new(dns.Msg)
	if m.Unpack(b) != nil {
		t.Fatal("malformed encrypted DNS response")
	}
	return m
}

// Independent RFC 8945 MAC check also covers signed NOTAUTH/BADTIME. The
// miekg verifier intentionally refuses any NOTAUTH before checking its MAC.
func encryptedMACValid(t *testing.T, wire []byte, requestMAC string, secret []byte) bool {
	t.Helper()
	m := encryptedResponse(t, wire)
	ts := m.IsTsig()
	if ts == nil || ts.MAC == "" || len(wire) < dns.Len(ts)+12 {
		return false
	}
	request, err := hex.DecodeString(requestMAC)
	if err != nil {
		t.Fatal("fixture request MAC encoding")
	}
	buf := binary.BigEndian.AppendUint16(nil, uint16(len(request))) //nolint:gosec // fixed SHA256/SHA512 digest
	buf = append(buf, request...)
	unsigned := bytes.Clone(wire[:len(wire)-dns.Len(ts)])
	binary.BigEndian.PutUint16(unsigned, ts.OrigId)
	binary.BigEndian.PutUint16(unsigned[10:], binary.BigEndian.Uint16(unsigned[10:])-1)
	buf = append(buf, unsigned...)
	name := func(value string) {
		b := make([]byte, 256)
		n, err := dns.PackDomainName(dns.CanonicalName(value), b, 0, nil, false)
		if err != nil {
			t.Fatal("fixture TSIG name encoding")
		}
		buf = append(buf, b[:n]...)
	}
	name(ts.Hdr.Name)
	buf = binary.BigEndian.AppendUint16(buf, ts.Hdr.Class)
	buf = binary.BigEndian.AppendUint32(buf, ts.Hdr.Ttl)
	name(ts.Algorithm)
	var clock [8]byte
	binary.BigEndian.PutUint64(clock[:], ts.TimeSigned)
	buf = append(buf, clock[2:]...)
	buf = binary.BigEndian.AppendUint16(buf, ts.Fudge)
	buf = binary.BigEndian.AppendUint16(buf, ts.Error)
	buf = binary.BigEndian.AppendUint16(buf, ts.OtherLen)
	other, err := hex.DecodeString(ts.OtherData)
	if err != nil {
		return false
	}
	buf = append(buf, other...)
	h := hmac.New(sha256.New, secret)
	if ts.Algorithm == dns.HmacSHA512 {
		h = hmac.New(sha512.New, secret)
	}
	_, _ = h.Write(buf)
	got, err := hex.DecodeString(ts.MAC)
	return err == nil && hmac.Equal(got, h.Sum(nil))
}

func TestEncryptedTSIGWireListeners(t *testing.T) {
	keys := testKeyring(t)
	for _, proto := range []Protocol{ProtoDoH, ProtoDoQ, ProtoDoH3} {
		t.Run(string(proto), func(t *testing.T) {
			s := &encryptedAnswer{}
			f := startFixture(t, fixtureOpts{specs: []Spec{{Protocol: proto}}, tsig: keys, stages: []pipeline.Stage{&ingress.Stage{}, s}})
			for _, mode := range []string{"valid", "sha512", "unsigned", "unknown", "wrong", "expired", "future", "refused-query", "update", "notify", "status"} {
				t.Run(mode, func(t *testing.T) {
					m := query("example.org.")
					m.Id = 0
					m.Compress = true
					m.SetEdns0(1232, true)
					m.IsEdns0().Option = append(m.IsEdns0().Option, &dns.EDNS0_PADDING{})
					m.Extra = append(m.Extra, &dns.A{Hdr: dns.RR_Header{Name: "example.org.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: []byte{192, 0, 2, 4}})
					name, alg, secret := xfrKey, dns.HmacSHA256, xfrSecret
					want, tsigError := dns.RcodeSuccess, uint16(0)
					admitted := true
					switch mode {
					case "sha512":
						name, alg, secret = xfrKey512, dns.HmacSHA512, xfrSecret512
					case "unknown":
						name, want, tsigError, admitted = "unknown-key.", dns.RcodeNotAuth, dns.RcodeBadKey, false
					case "wrong":
						secret, want, tsigError, admitted = otherSecret, dns.RcodeNotAuth, dns.RcodeBadSig, false
					case "expired", "future":
						want, tsigError, admitted = dns.RcodeNotAuth, dns.RcodeBadTime, false
					case "refused-query":
						m.Question[0].Qtype, want, admitted = dns.TypeANY, dns.RcodeRefused, false
					case "update":
						m = signedUpdate()
						m.Id = 0
						m.Extra = nil
						want = dns.RcodeRefused
					case "notify":
						m.SetNotify("example.org.")
						m.Id = 0
						want = dns.RcodeRefused
					case "status":
						m.Opcode, want, admitted = dns.OpcodeStatus, dns.RcodeNotImplemented, false
					}
					if mode != "unsigned" {
						m.SetTsig(name, alg, 30, time.Now().Unix())
						if mode == "expired" {
							m.IsTsig().TimeSigned -= 3600
						}
						if mode == "future" {
							m.IsTsig().TimeSigned += 3600
						}
					}
					clientKeys, err := NewStaticKeyring(TSIGKey{Name: name, Algorithm: alg, Secret: secret})
					if err != nil {
						t.Fatal("fixture keyring")
					}
					wire, mac := encryptedRequest(t, m, clientKeys)
					before := len(s.observations())
					out := encryptedExchange(t, f, proto, http.MethodPost, wire)
					r := encryptedResponse(t, out)
					if r.Id != 0 || r.Rcode != want {
						t.Fatalf("response ID/rcode mismatch for %s", mode)
					}
					ts := r.IsTsig()
					if mode == "unsigned" {
						if ts != nil {
							t.Fatal("unsigned query received a TSIG")
						}
					} else {
						if ts == nil || ts.Error != tsigError {
							t.Fatal("missing expected TSIG response/error")
						}
						if tsigError == dns.RcodeBadKey || tsigError == dns.RcodeBadSig {
							if ts.MAC != "" {
								t.Fatal("BADKEY/BADSIG must not carry a MAC")
							}
						} else {
							serverSecret := secret
							if !encryptedMACValid(t, out, mac, serverSecret) {
								t.Fatal("response did not authenticate with original request MAC and key")
							}
							if tsigError == 0 && dns.TsigVerifyWithProvider(bytes.Clone(out), TSIGProvider(clientKeys), mac, false) != nil {
								t.Fatal("standard client rejected the signed response")
							}
							if tsigError == dns.RcodeBadTime && (ts.OtherLen != 6 || ts.TimeSigned != m.IsTsig().TimeSigned) {
								t.Fatal("BADTIME lost original request time/server clock")
							}
						}
					}
					seen := s.observations()
					if (len(seen) > before) != admitted {
						t.Fatal("signature/opcode admission reached wrong pipeline path")
					}
					if admitted {
						got := seen[len(seen)-1]
						if got.native || got.client.Proto != proto.Pipeline() || got.tsigs != 0 || (mode != "unsigned" && got.client.TSIGKey != name) || (mode == "unsigned" && got.client.TSIGKey != "") {
							t.Fatal("encrypted verification lost provenance or minted native mutation proof")
						}
					}
				})
			}
			if proto != ProtoDoQ {
				m := query("get.example.org.")
				m.Id = 0
				m.SetTsig(xfrKey, dns.HmacSHA256, 30, time.Now().Unix())
				wire, mac := encryptedRequest(t, m, keys)
				if out := encryptedExchange(t, f, proto, http.MethodGet, wire); !encryptedMACValid(t, out, mac, xfrSecret) {
					t.Fatal("GET response did not authenticate")
				}
				if proto == ProtoDoH {
					if out := encryptedExchange(t, f, proto, http.MethodPost, wire, true); !encryptedMACValid(t, out, mac, xfrSecret) {
						t.Fatal("HTTP/1.1 response did not authenticate")
					}
				}
			}
		})
	}
}

func TestEncryptedTSIGOriginalInstallationAndDeadline(t *testing.T) {
	for _, proto := range []Protocol{ProtoDoH, ProtoDoQ, ProtoDoH3} {
		for _, mode := range []string{"rotation", "deadline"} {
			t.Run(string(proto)+"/"+mode, func(t *testing.T) {
				keys := testKeyring(t)
				s := &encryptedAnswer{block: true, entered: make(chan struct{}), resume: make(chan struct{})}
				f := startFixture(t, fixtureOpts{specs: []Spec{{Protocol: proto}}, tsig: keys, stages: []pipeline.Stage{s}})
				release := sync.OnceFunc(func() { close(s.resume) })
				t.Cleanup(release)
				m := query("held.example.org.")
				m.Id = 0
				fudge := uint16(30)
				if mode == "deadline" {
					fudge = 2
				}
				m.SetTsig(xfrKey, dns.HmacSHA256, fudge, time.Now().Unix())
				wire, mac := encryptedRequest(t, m, keys)
				done := make(chan []byte, 1)
				go func() {
					defer close(done)
					done <- encryptedExchange(t, f, proto, http.MethodPost, wire)
				}()
				select {
				case <-s.entered:
				case <-time.After(3 * time.Second):
					t.Fatal("signed request did not enter its original installation")
				}
				if mode == "rotation" {
					rotated, err := NewStaticKeyring(TSIGKey{Name: xfrKey, Algorithm: TSIGHMACSHA256, Secret: otherSecret})
					if err != nil {
						t.Fatal("fixture rotated ring")
					}
					// Registry credentials are fixed at installation, as on native
					// listeners. Qualify replacement using another actual listener,
					// without racing mutation of the old listener's private env.
					freshStage := &encryptedAnswer{}
					fresh := startFixture(t, fixtureOpts{specs: []Spec{{Protocol: proto}}, tsig: rotated, stages: []pipeline.Stage{freshStage}})
					q := query("new.example.org.")
					q.Id = 0
					q.SetTsig(xfrKey, dns.HmacSHA256, 30, time.Now().Unix())
					newWire, newMAC := encryptedRequest(t, q, rotated)
					out := encryptedExchange(t, fresh, proto, http.MethodPost, newWire)
					if r := encryptedResponse(t, out); r.Rcode != dns.RcodeSuccess || !encryptedMACValid(t, out, newMAC, otherSecret) || encryptedMACValid(t, out, newMAC, xfrSecret) {
						t.Fatal("replacement installation did not exclusively use its new key")
					}
					oldWire, _ := encryptedRequest(t, q, keys)
					r := encryptedResponse(t, encryptedExchange(t, fresh, proto, http.MethodPost, oldWire))
					if r.Rcode != dns.RcodeNotAuth || len(r.Answer) != 0 || r.IsTsig() == nil || r.IsTsig().Error != dns.RcodeBadSig || len(freshStage.observations()) != 1 {
						t.Fatal("replacement installation accepted original credential")
					}
				} else {
					deadline := time.Unix(int64(m.IsTsig().TimeSigned)+int64(fudge), 0) //nolint:gosec // fixture's current positive Unix time
					timer := time.NewTimer(time.Until(deadline) + 20*time.Millisecond)
					defer timer.Stop()
					select {
					case <-timer.C:
					case <-t.Context().Done():
						t.Fatal("test canceled before original deadline")
					}
				}
				release()
				var out []byte
				select {
				case out = <-done:
				case <-time.After(4 * time.Second):
					t.Fatal("held signed request did not finish")
				}
				if len(out) == 0 || !encryptedMACValid(t, out, mac, xfrSecret) || encryptedMACValid(t, out, mac, otherSecret) {
					t.Fatal("held response borrowed a replacement signer or lost original request MAC")
				}
				r := encryptedResponse(t, out)
				if mode == "rotation" {
					if r.Rcode != dns.RcodeSuccess || r.IsTsig().Error != 0 {
						t.Fatal("read reply did not finish under original installation")
					}
				} else if r.Rcode != dns.RcodeNotAuth || r.IsTsig().Error != dns.RcodeBadTime || len(r.Answer) != 0 || r.IsTsig().TimeSigned != m.IsTsig().TimeSigned {
					t.Fatal("expired held request renewed its original time budget")
				}
			})
		}
	}
}

func TestEncryptedTSIGFramingAndOpaqueProviderFailClosed(t *testing.T) {
	keys := testKeyring(t)
	for _, proto := range []Protocol{ProtoDoH, ProtoDoQ, ProtoDoH3} {
		t.Run(string(proto), func(t *testing.T) {
			s := &encryptedAnswer{}
			f := startFixture(t, fixtureOpts{specs: []Spec{{Protocol: proto}}, tsig: keys, stages: []pipeline.Stage{s}})
			for _, mode := range []string{"nonfinal", "duplicate", "wrong-section", "trailing", "tampered-wire"} {
				t.Run(mode, func(t *testing.T) {
					m := query("example.org.")
					m.Id = 0
					m.SetTsig(xfrKey, dns.HmacSHA256, 30, time.Now().Unix())
					wire, _ := encryptedRequest(t, m, keys)
					switch mode {
					case "trailing":
						wire = append(wire, 0, 0)
					case "tampered-wire":
						wire[13] ^= 1
					default:
						parsed := encryptedResponse(t, wire)
						switch mode {
						case "nonfinal":
							parsed.SetEdns0(1232, false)
						case "duplicate":
							parsed.Extra = append(parsed.Extra, dns.Copy(parsed.IsTsig()))
						case "wrong-section":
							parsed.Answer, parsed.Extra = parsed.Extra, nil
						}
						var err error
						wire, err = parsed.Pack()
						if err != nil {
							t.Fatal("pack malformed signed fixture")
						}
					}
					out := encryptedExchange(t, f, proto, http.MethodPost, wire)
					r := encryptedResponse(t, out)
					if len(s.observations()) != 0 || len(r.Answer) != 0 {
						t.Fatal("malformed/tampered TSIG reached pipeline or disclosed answer")
					}
					if mode == "tampered-wire" {
						if r.Rcode != dns.RcodeNotAuth || r.IsTsig() == nil || r.IsTsig().Error != dns.RcodeBadSig {
							t.Fatal("tampered original wire did not receive BADSIG")
						}
					} else if r.Rcode != dns.RcodeFormatError {
						t.Fatal("malformed TSIG silently fell back to unsigned")
					}
				})
			}
			opaque := startFixture(t, fixtureOpts{specs: []Spec{{Protocol: proto}}, tsig: lookupOnly{keys}, stages: []pipeline.Stage{s}})
			m := query("example.org.")
			m.Id = 0
			m.SetTsig(xfrKey, dns.HmacSHA256, 30, time.Now().Unix())
			wire, _ := encryptedRequest(t, m, keys)
			r := encryptedResponse(t, encryptedExchange(t, opaque, proto, http.MethodPost, wire))
			if r.Rcode != dns.RcodeNotAuth || r.IsTsig() == nil || r.IsTsig().Error != dns.RcodeBadKey || len(s.observations()) != 0 {
				t.Fatal("opaque mutable provider manufactured an installed signing capability")
			}
		})
	}
}
