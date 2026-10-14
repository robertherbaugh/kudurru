package secondary

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func transferCertificate(t *testing.T, hosts []string, before, after time.Time) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{
		SerialNumber: serial, NotBefore: before, NotAfter: after,
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			cert.IPAddresses = append(cert.IPAddresses, ip)
		} else {
			cert.DNSNames = append(cert.DNSNames, host)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	return pair, pool
}

func TestTransferTLSVerification(t *testing.T) {
	for _, mode := range []string{"verified IP", "verified hostname", "system trust rejects private CA", "wrong hostname", "expired", "not yet valid"} {
		t.Run(mode, func(t *testing.T) {
			hosts := []string{"127.0.0.1", "localhost"}
			before, after := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
			switch mode {
			case "wrong hostname":
				hosts = []string{"other.example"}
			case "expired":
				before, after = time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)
			case "not yet valid":
				before, after = time.Now().Add(time.Hour), time.Now().Add(2*time.Hour)
			}
			pair, pool := transferCertificate(t, hosts, before, after)
			ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12, NextProtos: []string{"dot"}})
			if err != nil {
				t.Fatal(err)
			}
			ring := transferRing(t, transferKey, "hmac-sha256", transferSecret)
			a := transferRR(t, "www.example.org. 31 IN A 192.0.2.31")
			address := transferPrimaryListener(t, ring, ln, func(w dns.ResponseWriter, q *dns.Msg) {
				defer func() { _ = w.Close() }()
				if w.TsigStatus() != nil {
					t.Errorf("TLS query authentication: %v", w.TsigStatus())
					return
				}
				var frames [][]dns.RR
				switch q.Question[0].Qtype {
				case dns.TypeSOA:
					frames = [][]dns.RR{{ixfrSOA(1)}}
				case dns.TypeAXFR:
					frames = [][]dns.RR{{ixfrSOA(1)}, {a, ixfrSOA(1)}}
				case dns.TypeIXFR:
					frames = [][]dns.RR{{ixfrSOA(1), transferSOA()}, {ixfrSOA(1), a}, {ixfrSOA(1)}}
				}
				for _, frame := range frames {
					m := transferReply(q, frame...)
					m.SetTsig(transferKey, dns.HmacSHA256, 300, time.Now().Unix())
					if err := w.WriteMsg(m); err != nil {
						return
					}
					w.TsigTimersOnly(true)
				}
			})
			if mode == "verified hostname" {
				_, port, _ := net.SplitHostPort(address)
				address = net.JoinHostPort("localhost", port)
			}
			client := &TransferClient{Keyring: ring, RootCAs: pool, Timeout: time.Second}
			if mode == "system trust rejects private CA" {
				client.RootCAs = nil
			}
			dz := transferZone(true)
			dz.Spec.Secondary.TransferProtocol = "tls"
			for _, operation := range []string{"SOA", "AXFR", "IXFR"} {
				var rrs []dns.RR
				var err error
				switch operation {
				case "SOA":
					var soa *dns.SOA
					soa, err = client.SOA(t.Context(), dz, address)
					if soa != nil {
						rrs = []dns.RR{soa}
					}
				case "AXFR":
					rrs, err = client.AXFR(t.Context(), dz, address)
				case "IXFR":
					rrs, err = client.IXFR(t.Context(), dz, address, []dns.RR{transferSOA()})
				}
				if mode == "verified IP" || mode == "verified hostname" {
					if err != nil || len(rrs) == 0 || rrs[0].(*dns.SOA).Serial != 1 {
						t.Fatalf("verified %s failed: %v, %v", operation, rrs, err)
					}
				} else {
					var verification *tls.CertificateVerificationError
					if !errors.As(err, &verification) || rrs != nil {
						t.Fatalf("%s did not reject certificate: %v, %v", operation, rrs, err)
					}
				}
			}
		})
	}
}

func TestTransferTLSHandshakeCancellation(t *testing.T) {
	for _, mode := range []string{"cancel", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			accepted := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				conn, err := ln.Accept()
				if err != nil {
					done <- err
					return
				}
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				close(accepted)
				var buf [4096]byte
				for {
					if _, err := conn.Read(buf[:]); err != nil {
						done <- err
						return
					}
				}
			}()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			client := &TransferClient{Timeout: 30 * time.Millisecond}
			dz := transferZone(false)
			dz.Spec.Secondary.TransferProtocol = "tls"
			result := make(chan error, 1)
			go func() { _, err := client.SOA(ctx, dz, ln.Addr().String()); result <- err }()
			select {
			case <-accepted:
			case <-time.After(time.Second):
				t.Fatal("TLS connection did not arrive")
			}
			want := context.DeadlineExceeded
			if mode == "cancel" {
				cancel()
				want = context.Canceled
			}
			select {
			case err := <-result:
				if !errors.Is(err, want) {
					t.Fatalf("TLS handshake lost cancellation: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("TLS handshake ignored cancellation")
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("TLS handshake cancellation did not close the connection")
			}
		})
	}
}
