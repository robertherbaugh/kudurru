package fuzz

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
)

var (
	keySHA256 = dnsserver.TSIGKey{Name: "xfr-key.", Algorithm: "hmac-sha256", Secret: []byte("0123456789abcdef0123456789abcdef")}
	keySHA512 = dnsserver.TSIGKey{Name: "other.key.", Algorithm: "hmac-sha512", Secret: []byte("fedcba9876543210fedcba9876543210")}
)

// signed returns a query signed with key (alg overrides its algorithm).
func signed(f *testing.F, key dnsserver.TSIGKey, alg string, qtype uint16) []byte {
	f.Helper()
	m := new(dns.Msg)
	m.SetQuestion("example.com.", qtype)
	m.SetTsig(key.Name, alg, 300, time.Now().Unix())
	ring, err := dnsserver.NewStaticKeyring(key)
	if err != nil {
		f.Fatal(err)
	}
	b, _, err := dns.TsigGenerateWithProvider(m, dnsserver.TSIGProvider(ring), "", false)
	if err != nil {
		f.Fatal(err)
	}
	return b
}

// FuzzTSIGVerify verifies arbitrary messages against the static keyring
// through the provider the DNS server installs (RFC 8945). Verification
// may fail but must not panic, and a message that verifies must carry, as
// its last record, a TSIG of a configured key with that key's algorithm.
func FuzzTSIGVerify(f *testing.F) {
	ring, err := dnsserver.NewStaticKeyring(keySHA256, keySHA512)
	if err != nil {
		f.Fatal(err)
	}
	provider := dnsserver.TSIGProvider(ring)
	f.Add(signed(f, keySHA256, dns.HmacSHA256, dns.TypeAXFR))
	f.Add(signed(f, keySHA512, dns.HmacSHA512, dns.TypeSOA))
	f.Add(signed(f, dnsserver.TSIGKey{Name: keySHA256.Name, Algorithm: "hmac-sha512", Secret: keySHA256.Secret}, dns.HmacSHA512, dns.TypeA)) // wrong algorithm for the key
	f.Add(signed(f, dnsserver.TSIGKey{Name: "unknown.", Algorithm: "hmac-sha256", Secret: []byte("x")}, dns.HmacSHA256, dns.TypeAXFR))
	unsigned := new(dns.Msg)
	unsigned.SetQuestion("example.com.", dns.TypeA)
	b, _ := unsigned.Pack()
	f.Add(b)
	f.Fuzz(func(t *testing.T, wire []byte) {
		// Verification works on the buffer it is given; keep the original.
		orig := bytes.Clone(wire)
		if err := dns.TsigVerifyWithProvider(wire, provider, "", false); err != nil {
			return
		}
		m := new(dns.Msg)
		if err := m.Unpack(orig); err != nil {
			t.Fatalf("verified a message that does not unpack: %v", err)
		}
		ts := m.IsTsig()
		if ts == nil {
			t.Fatal("verified a message without TSIG")
		}
		_, alg, ok := ring.Lookup(ts.Hdr.Name)
		if !ok {
			t.Fatalf("verified with an unknown key %q", ts.Hdr.Name)
		}
		if !strings.EqualFold(dns.Fqdn(ts.Algorithm), alg) {
			t.Fatalf("key %s (%s) verified a MAC with algorithm %s", ts.Hdr.Name, alg, ts.Algorithm)
		}
	})
}

// answerStage answers every A question with 192.0.2.1 in the upstream
// position, so the DoH listener runs the real ingress stage.
type answerStage struct{}

func (answerStage) Name() string { return "upstream" }

func (answerStage) Serve(req *pipeline.Request, _ pipeline.Next) (*pipeline.Response, error) {
	m := new(dns.Msg)
	m.SetReply(req.Msg)
	if len(req.Msg.Question) == 1 && req.Msg.Question[0].Qtype == dns.TypeA {
		m.Answer = append(m.Answer, &dns.A{Hdr: dns.RR_Header{Name: req.Msg.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.IPv4(192, 0, 2, 1)})
	}
	return &pipeline.Response{Msg: m, Status: pipeline.StatusForwarded}, nil
}

// selfSigned writes a throwaway certificate pair into dir.
func selfSigned(f *testing.F, dir string) (certFile, keyFile string) {
	f.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		f.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fuzz"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		f.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		f.Fatal(err)
	}
	certFile, keyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		f.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		f.Fatal(err)
	}
	return certFile, keyFile
}

// FuzzDoHRequest sends arbitrary RFC 8484 requests (method, path, content
// type, body or dns= parameter) to a real DoH listener. Every request must
// get an HTTP answer (a handler panic resets the connection); a 200 must
// be an application/dns-message that unpacks and echoes the query id and
// question; malformed input must be a 4xx, never a 5xx.
func FuzzDoHRequest(f *testing.F) {
	dir := f.TempDir()
	certFile, keyFile := selfSigned(f, dir)
	chain, err := pipeline.New(&ingress.Stage{NodeID: "fuzz", Hostname: "fuzz", Version: "fuzz"}, answerStage{})
	if err != nil {
		f.Fatal(err)
	}
	reg, err := dnsserver.NewRegistry(dnsserver.Options{
		Handler: &dnsserver.Handler{Chain: chain, Logger: slog.New(slog.DiscardHandler)},
		Logger:  slog.New(slog.DiscardHandler),
	})
	if err != nil {
		f.Fatal(err)
	}
	if err := reg.Add(dnsserver.Spec{Protocol: dnsserver.ProtoDoH, Address: "127.0.0.1:0", CertFile: certFile, KeyFile: keyFile}); err != nil {
		f.Fatal(err)
	}
	if err := reg.Bind(); err != nil {
		f.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = reg.Run(ctx); close(done) }()
	f.Cleanup(func() { cancel(); <-done })
	base := "https://" + reg.Addr(dnsserver.ProtoDoH).String()
	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // throwaway local certificate
		ForceAttemptHTTP2: true, MaxIdleConnsPerHost: 8,
	}}

	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	q.Id = 0x1234
	wire, _ := q.Pack()
	f.Add(uint8(0), "/dns-query", "application/dns-message", wire)
	f.Add(uint8(1), "/dns-query", "", wire)
	f.Add(uint8(0), "/dns-query", "application/dns-message; charset=x", []byte{0, 0})
	f.Add(uint8(2), "/other", "text/plain", []byte("hello"))
	f.Add(uint8(1), "/dns-query?dns=", "", []byte("AAABAAABAAAAAAAAB2V4YW1wbGUDY29tAAABAAE="))
	f.Fuzz(func(t *testing.T, method uint8, path, ctype string, body []byte) {
		if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, " \r\n#") || len(path) > 512 {
			return
		}
		var req *http.Request
		var err error
		switch method % 4 {
		case 0:
			req, err = http.NewRequest(http.MethodPost, base+path, bytes.NewReader(body))
			if err == nil && ctype != "" && printable(ctype) {
				req.Header.Set("Content-Type", ctype)
			}
		case 1:
			sep := "?"
			if strings.Contains(path, "?") {
				sep = "&"
			}
			req, err = http.NewRequest(http.MethodGet, base+path+sep+"dns="+base64.RawURLEncoding.EncodeToString(body), nil)
		case 2:
			req, err = http.NewRequest(http.MethodHead, base+path+"?dns="+base64.RawURLEncoding.EncodeToString(body), nil)
		default:
			req, err = http.NewRequest(http.MethodPut, base+path, bytes.NewReader(body))
		}
		if err != nil {
			return
		}
		resp, err := hc.Do(req)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("DoH request timed out: %v", err)
			}
			t.Fatalf("DoH request failed (handler panic or reset?): %v", err)
		}
		out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<17))
		_ = resp.Body.Close()
		if resp.StatusCode >= 500 {
			t.Fatalf("%s %s: %d %q", req.Method, path, resp.StatusCode, out)
		}
		if resp.StatusCode != http.StatusOK || req.Method == http.MethodHead {
			return
		}
		if resp.Header.Get("Content-Type") != "application/dns-message" {
			t.Fatalf("200 with content type %q", resp.Header.Get("Content-Type"))
		}
		in := new(dns.Msg)
		if err := in.Unpack(body); err != nil {
			t.Fatalf("200 for a body that does not unpack: %v", err)
		}
		r := new(dns.Msg)
		if err := r.Unpack(out); err != nil {
			t.Fatalf("answer does not unpack: %v", err)
		}
		if r.Id != in.Id || !r.Response {
			t.Fatalf("answer id %d response %v for query id %d", r.Id, r.Response, in.Id)
		}
		if len(in.Question) == 1 && (len(r.Question) != 1 || !strings.EqualFold(r.Question[0].Name, in.Question[0].Name)) {
			t.Fatalf("question not echoed: %v vs %v", r.Question, in.Question)
		}
	})
}

// printable reports whether s is a valid HTTP header value for the client
// (visible ASCII and spaces).
func printable(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}
