//go:build unix

package memberidentity

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"net"
	"testing"
	"time"
)

func peerAdmissionState(t *testing.T, short bool) (Trust, tls.ConnectionState, time.Time) {
	t.Helper()
	primary, _ := testIdentity(t)
	remote, _ := testIdentity(t)
	serverConfig, err := primary.ServerTLS([]Trust{remote.Trust()})
	if err != nil {
		t.Fatal(err)
	}
	clientConfig, err := remote.ClientTLS(primary.Trust())
	if err != nil {
		t.Fatal(err)
	}
	var expires time.Time
	if short {
		n, err := serial()
		if err != nil {
			t.Fatal(err)
		}
		pub, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		expires = time.Now().Add(3 * time.Second).Truncate(time.Second)
		template := &x509.Certificate{SerialNumber: n, Subject: pkix.Name{CommonName: memberName(remote.trust.member)}, DNSNames: []string{memberName(remote.trust.member)}, NotBefore: time.Now().Add(-time.Minute), NotAfter: expires, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
		der, err := x509.CreateCertificate(rand.Reader, template, remote.trust.root, pub, remote.key)
		if err != nil {
			t.Fatal(err)
		}
		leaf := &tls.Certificate{Certificate: [][]byte{der, remote.trust.root.Raw}, PrivateKey: key}
		clientConfig.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return leaf, nil }
	}
	a, b := net.Pipe()
	defer func() { _ = a.Close(); _ = b.Close() }()
	server, client := tls.Server(a, serverConfig), tls.Client(b, clientConfig)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.HandshakeContext(ctx) }()
	err = server.HandshakeContext(ctx)
	clientErr := <-done
	if err != nil || clientErr != nil {
		t.Fatal("actual mutual TLS failed", err, clientErr)
	}
	return remote.Trust(), server.ConnectionState(), expires
}

func TestPeerAdmissionRetainsOriginalCertificateExpiry(t *testing.T) {
	trust, state, expires := peerAdmissionState(t, true)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	proof, err := trust.CapturePeer(ctx, &state)
	if err != nil || proof.Check() != nil {
		t.Fatal("actual verified short leaf was not captured", err)
	}
	if !proof.Deadline().Equal(expires) {
		t.Fatal("output deadline did not retain actual leaf expiry")
	}
	// Mutating caller-owned TLS fields cannot replace captured original expiry.
	state.VerifiedChains[0][0].NotAfter = expires.Add(time.Hour)
	time.Sleep(time.Until(expires) + time.Millisecond)
	if err := proof.Check(); !errors.Is(err, ErrIdentity) {
		t.Fatal("original certificate was renewed by later TLS fields", err)
	}
	if err := proof.Check(); !errors.Is(err, ErrIdentity) {
		t.Fatal("expired certificate proof reopened", err)
	}
}

func TestPeerAdmissionOriginalContextAndCopiesShareTerminalState(t *testing.T) {
	trust, state, _ := peerAdmissionState(t, false)
	for _, mode := range []string{"cancel", "deadline", "clock-rollback"} {
		t.Run(mode, func(t *testing.T) {
			lifetime := 5 * time.Second
			if mode == "deadline" {
				lifetime = 30 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(t.Context(), lifetime)
			defer cancel()
			proof, err := trust.CapturePeer(ctx, &state)
			if err != nil {
				t.Fatal(err)
			}
			copyProof := *proof
			switch mode {
			case "cancel":
				cancel()
			case "deadline":
				<-ctx.Done()
			case "clock-rollback":
				proof.life.last.Store(time.Now().Add(time.Second).UnixNano())
			}
			if err := copyProof.Check(); err == nil {
				t.Fatal("original lifetime refusal granted")
			}
			proof.life.last.Store(time.Now().Add(-time.Second).UnixNano())
			if err := proof.Check(); !errors.Is(err, ErrIdentity) {
				t.Fatal("copy refusal did not close original", err)
			}
		})
	}
	for _, ctx := range []context.Context{nil, context.Background()} {
		if proof, err := trust.CapturePeer(ctx, &state); err == nil || proof != nil {
			t.Fatal("missing original finite context admitted")
		}
	}
	if err := new(PeerAdmission).Check(); !errors.Is(err, ErrIdentity) {
		t.Fatal("zero admission granted", err)
	}
}
