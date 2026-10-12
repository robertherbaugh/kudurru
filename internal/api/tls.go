package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/robertherbaugh/kudurru/internal/config"
)

// Self-signed certificate files under <node.dataDir>/keys/ (§6.2,
// listeners.api.tls.auto: selfsigned).
const (
	SelfSignedCertFile = "api.crt"
	SelfSignedKeyFile  = "api.key"
	selfSignedValidity = 5 * 365 * 24 * time.Hour
)

// tlsConfig returns the listener TLS configuration, or nil for plain HTTP.
func (s *Server) tlsConfig(cfg *config.Config) (*tls.Config, error) {
	t := cfg.Listeners.API.TLS
	certFile, keyFile := t.CertFile, t.KeyFile
	switch {
	case certFile != "":
	case t.Auto == config.APITLSSelfSigned:
		var err error
		certFile, keyFile, err = EnsureSelfSigned(filepath.Join(cfg.Node.DataDir, "keys"), cfg.Node.Name)
		if err != nil {
			return nil, err
		}
		s.log.Info("api: serving a self-signed certificate", "cert", certFile)
	default:
		return nil, nil
	}
	rc := &reloadingCert{certFile: certFile, keyFile: keyFile}
	if _, err := rc.get(); err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		NextProtos:     []string{"h2", "http/1.1"},
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return rc.get() },
	}, nil
}

// reloadingCert re-reads the key pair when the certificate file changes
// (cert-manager and ACME clients rotate files in place).
type reloadingCert struct {
	certFile, keyFile string

	mu      sync.Mutex
	cert    *tls.Certificate
	modTime time.Time
	checked time.Time
}

func (r *reloadingCert) get() (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if r.cert != nil && now.Sub(r.checked) < 10*time.Second {
		return r.cert, nil
	}
	r.checked = now
	st, err := os.Stat(r.certFile)
	if err != nil {
		if r.cert != nil {
			return r.cert, nil
		}
		return nil, fmt.Errorf("api: tls: %w", err)
	}
	if r.cert != nil && st.ModTime().Equal(r.modTime) {
		return r.cert, nil
	}
	c, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		if r.cert != nil {
			return r.cert, nil
		}
		return nil, fmt.Errorf("api: tls: %w", err)
	}
	r.cert, r.modTime = &c, st.ModTime()
	return r.cert, nil
}

// EnsureSelfSigned returns the self-signed certificate and key in dir,
// generating them on first use (ECDSA P-256, SANs: node name, localhost
// and the loopback addresses). An existing pair is reused as long as it
// parses and has not expired, so the fingerprint users pinned survives
// restarts.
func EnsureSelfSigned(dir, nodeName string) (certFile, keyFile string, err error) {
	certFile, keyFile = filepath.Join(dir, SelfSignedCertFile), filepath.Join(dir, SelfSignedKeyFile)
	if err := EnsureSelfSignedPair(certFile, keyFile, nodeName); err != nil {
		return "", "", err
	}
	return certFile, keyFile, nil
}

// EnsureSelfSignedPair is EnsureSelfSigned for any file pair, with extra
// IP SANs (the block page uses it for <node.dataDir>/keys/blockpage.crt).
func EnsureSelfSignedPair(certFile, keyFile, nodeName string, extraIPs ...net.IP) error {
	if c, err := tls.LoadX509KeyPair(certFile, keyFile); err == nil {
		if leaf, err := x509.ParseCertificate(c.Certificate[0]); err == nil && time.Now().Before(leaf.NotAfter) {
			return nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		if _, statErr := os.Stat(certFile); statErr == nil {
			return fmt.Errorf("tls: %s exists but does not load: %w", certFile, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(certFile), 0o700); err != nil {
		return fmt.Errorf("tls: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return err
	}
	names := []string{"localhost"}
	if nodeName != "" && nodeName != "localhost" {
		names = append([]string{nodeName}, names...)
	}
	if h, err := os.Hostname(); err == nil && h != nodeName && h != "localhost" {
		names = append(names, h)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: names[0], Organization: []string{"kudurru self-signed"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(selfSignedValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              names,
		IPAddresses:           append([]net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}, extraIPs...),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		return err
	}
	return writeFileAtomic(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

func writeFileAtomic(name string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(name), "."+filepath.Base(name)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), name)
}
