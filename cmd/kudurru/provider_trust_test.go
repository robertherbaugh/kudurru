package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
)

func providerTestCA(t *testing.T, serial int64, ca bool) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(serial), NotBefore: time.Unix(1000, 0), NotAfter: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC), IsCA: ca, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestProviderTrustCapturesCanonicalActualCertificates(t *testing.T) {
	a, b := providerTestCA(t, 1, true), providerTestCA(t, 2, true)
	input := append([]byte("# generated bundle comment\r\n\n"), a...)
	got, err := canonicalProviderTrust([][]byte{input, b, a})
	if err != nil {
		t.Fatal(err)
	}
	reordered, err := canonicalProviderTrust([][]byte{b, a})
	if err != nil || !bytes.Equal(got, reordered) || bytes.Count(got, []byte("BEGIN CERTIFICATE")) != 2 {
		t.Fatal("CA normalization lost bytes, kept duplicates or depended on bundle order", err)
	}
	// The real resource owner consumes this exact trust evidence. Equal roots
	// agree despite duplicate input; changing a root changes its authority ID.
	host := func(trust []byte) *providerresources.HostCut {
		t.Helper()
		h, err := providerresources.NewHost(providerresources.HostOptions{
			Identity: "test-host", CredentialPolicy: "test-policy", EntitlementIdentity: "test-denied", FingerprintKeyID: "test-key",
			ConfigSources: []servingauth.ProviderSource{}, AllowedOrigins: []string{"https://login.example"}, LocalOrigins: []string{}, Algorithms: []string{"RS256"}, TrustPEM: trust,
		})
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	if host(got).NetworkIdentity() != host(reordered).NetworkIdentity() || host(got).NetworkIdentity() == host(a).NetworkIdentity() {
		t.Fatal("resource trust identity did not bind actual CA bytes")
	}
	input[0] = '!'
	if !bytes.Equal(got, reordered) {
		t.Fatal("trust result aliases caller input")
	}
}

func TestProviderTrustRejectsMalformedAndUnboundedInputs(t *testing.T) {
	ca := providerTestCA(t, 1, true)
	block, _ := pem.Decode(ca)
	for name, data := range map[string][]byte{
		"leaf":                   providerTestCA(t, 2, false),
		"private type":           pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: block.Bytes}),
		"headers":                pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Headers: map[string]string{"Comment": "invalid"}, Bytes: block.Bytes}),
		"garbage before":         append([]byte("unverified\n"), ca...),
		"garbage after":          append(bytes.Clone(ca), []byte("unverified\n")...),
		"malformed before valid": append([]byte("-----BEGIN CERTIFICATE-----\n!!!\n-----END CERTIFICATE-----\n"), ca...),
		"noncertificate DER":     pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{1, 2, 3}}),
		"bytes":                  bytes.Repeat([]byte(" "), providerresources.MaxTrustBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := canonicalProviderTrust([][]byte{data}); err == nil {
				t.Fatal("invalid trust silently accepted or skipped")
			}
		})
	}
	if _, err := canonicalProviderTrust([][]byte{bytes.Repeat([]byte(" "), providerresources.MaxTrustBytes), ca}); !errors.Is(err, providerresources.ErrBounds) {
		t.Fatal("aggregate raw inputs bypassed preallocation cap", err)
	}
	var many []byte
	for i := range providerresources.MaxTrustCertificates + 1 {
		many = append(many, providerTestCA(t, int64(i+1), true)...)
	}
	if _, err := canonicalProviderTrust([][]byte{many}); !errors.Is(err, providerresources.ErrBounds) {
		t.Fatal("certificate count was truncated or unbounded", err)
	}
}

func TestProviderTrustBoundedFileCaptureAndSystemSelection(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	ca := providerTestCA(t, 1, true)
	system := write("system.pem", ca)
	operator := write("operator.pem", append([]byte("# operator copy\n"), ca...))
	policy := config.ProviderResources{SystemTrust: true, CAFiles: []string{operator}}
	captured, err := loadProviderTrust(t.Context(), config.ProviderResources{CAFiles: []string{operator}})
	if err != nil || !bytes.Equal(captured, ca) {
		t.Fatal("production loader did not use the explicit operator bundle", err)
	}
	got, err := readProviderTrust(t.Context(), policy, []string{filepath.Join(dir, "absent"), system})
	if err != nil || !bytes.Equal(got, ca) {
		t.Fatal("actual file capture did not canonicalize duplicate roots", err)
	}
	broken := write("broken.pem", []byte("not a certificate"))
	if _, err := readProviderTrust(t.Context(), policy, []string{broken, system}); err == nil {
		t.Fatal("malformed first system bundle was skipped")
	}
	if _, err := readProviderTrust(t.Context(), policy, nil); err == nil {
		t.Fatal("missing system trust silently fell back to operator roots")
	}
	if _, err := readProviderTrust(t.Context(), config.ProviderResources{CAFiles: []string{filepath.Join(dir, "missing")}}, nil); err == nil {
		t.Fatal("missing operator trust accepted")
	}
	large := write("large.pem", bytes.Repeat([]byte(" "), providerresources.MaxTrustBytes+1))
	if _, err := readProviderTrust(t.Context(), config.ProviderResources{CAFiles: []string{large}}, nil); !errors.Is(err, providerresources.ErrBounds) {
		t.Fatal("unbounded CA file accepted", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := readProviderTrust(ctx, policy, []string{system}); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled load read trust", err)
	}
	if _, err := readProviderTrust(t.Context(), config.ProviderResources{CAFiles: []string{write("empty.pem", nil)}}, nil); err == nil {
		t.Fatal("empty CA file became trust")
	}
	if empty, err := readProviderTrust(t.Context(), config.ProviderResources{}, nil); err != nil || len(empty) != 0 {
		t.Fatal("explicit no-network/no-trust policy rejected", err)
	}
	if len(providerSystemBundles("windows")) != 0 {
		t.Fatal("invented system PEM provenance on unsupported platform")
	}
}
