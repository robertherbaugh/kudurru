// Package testcert generates throw-away self-signed certificates for tests
// and spike measurements of the TLS listeners. Nothing outside tests and
// the spike tooling may import it.
package testcert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Pair is a generated certificate.
type Pair struct {
	CertFile, KeyFile string
	CertPEM, KeyPEM   []byte
	// Pool trusts exactly this certificate.
	Pool *x509.CertPool
	Cert *x509.Certificate
}

// Write generates an ECDSA P-256 certificate valid for one day for
// localhost, 127.0.0.1 and ::1 (plus any extra hosts), and writes it as
// cert.pem / key.pem with the given name prefix into dir.
func Write(dir, prefix string, extraHosts ...string) (*Pair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "kudurru test " + serial.String()},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	for _, h := range extraHosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	p := &Pair{
		CertFile: filepath.Join(dir, prefix+"cert.pem"),
		KeyFile:  filepath.Join(dir, prefix+"key.pem"),
		CertPEM:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:   pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		Pool:     x509.NewCertPool(),
		Cert:     cert,
	}
	p.Pool.AddCert(cert)
	if err := os.WriteFile(p.CertFile, p.CertPEM, 0o600); err != nil {
		return nil, fmt.Errorf("testcert: %w", err)
	}
	if err := os.WriteFile(p.KeyFile, p.KeyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("testcert: %w", err)
	}
	return p, nil
}
