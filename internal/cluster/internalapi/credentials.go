package internalapi

import (
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"sync"
	"time"
)

// HKDF info strings (D47). Changing either invalidates every running
// node's trust at once, so they are versioned.
const (
	TokenInfo = "kudurru/internal/v1"
	CAInfo    = "kudurru/internal-ca/v1"
)

// DefaultServerName is the DNS SAN of every derived node certificate and
// the name clients verify. Nodes are dialled by IP or pod DNS name, which
// the certificate cannot know in advance; the CA (derived from the shared
// key) is what authenticates the peer.
const DefaultServerName = "internal.kudurru.invalid"

// Node certificate lifetime and the point at which it is re-issued.
const (
	nodeCertLifetime = 90 * 24 * time.Hour
	nodeCertRenew    = 30 * 24 * time.Hour
)

// Fixed validity of the derived CA: identical bytes on every node.
var (
	caNotBefore = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	caNotAfter  = time.Date(2125, 1, 1, 0, 0, 0, 0, time.UTC)
)

// ErrNoCredentials is returned by NewCredentials when neither an
// encryption key nor a token file is configured.
var ErrNoCredentials = errors.New("internalapi: no encryption key and no token file: the internal listener needs one (KUDURRU_ENCRYPTION_KEY or listeners.internal.tokenFile)")

// CredentialOptions selects where the internal plane's token and TLS
// material come from.
type CredentialOptions struct {
	// Key is the cluster encryption key (KUDURRU_ENCRYPTION_KEY, decoded).
	// At least 32 bytes when set.
	Key []byte
	// PreviousKey is KUDURRU_ENCRYPTION_KEY_PREVIOUS during a rotation: its
	// token is accepted and its CA trusted, so nodes on either key
	// interoperate while the rollout proceeds.
	PreviousKey []byte
	// NodeName goes into the node certificate's subject and SANs.
	NodeName string
	// TokenFile replaces the derived token (listeners.internal.tokenFile);
	// surrounding white space is trimmed.
	TokenFile string
	// CertFile and KeyFile replace the derived node certificate.
	CertFile, KeyFile string
	// CAFile replaces the derived trust anchors for dialling peers. With
	// certificate files but neither CAFile nor Key, the system roots are
	// used.
	CAFile string
	// ServerName is the name clients verify; default DefaultServerName.
	// Set it to a SAN of CertFile when certificates are provided.
	ServerName string
	// Now is the clock for node certificates; default time.Now.
	Now func() time.Time
}

// Credentials hold the bearer tokens and TLS material of one node. They
// are safe for concurrent use.
type Credentials struct {
	tokens     [][]byte // tokens[0] is sent; all are accepted
	roots      *x509.CertPool
	serverName string
	nodeName   string
	now        func() time.Time

	fixed *tls.Certificate // from CertFile/KeyFile
	ca    *derivedCA       // signs node certificates when fixed is nil

	mu   sync.Mutex
	leaf *tls.Certificate
}

type derivedCA struct {
	cert *x509.Certificate
	key  ed25519.PrivateKey
}

// NewCredentials derives or loads the token and TLS material.
func NewCredentials(opts CredentialOptions) (*Credentials, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.ServerName == "" {
		opts.ServerName = DefaultServerName
	}
	for _, k := range [][]byte{opts.Key, opts.PreviousKey} {
		if len(k) > 0 && len(k) < 32 {
			return nil, fmt.Errorf("internalapi: encryption key is %d bytes, want at least 32", len(k))
		}
	}
	c := &Credentials{serverName: opts.ServerName, nodeName: opts.NodeName, now: opts.Now}

	// Token.
	switch {
	case opts.TokenFile != "":
		b, err := os.ReadFile(opts.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("internalapi: token file: %w", err)
		}
		tok := strings.TrimSpace(string(b))
		if len(tok) < 16 {
			return nil, fmt.Errorf("internalapi: token file %s holds %d characters, want at least 16", opts.TokenFile, len(tok))
		}
		c.tokens = [][]byte{[]byte(tok)}
	case len(opts.Key) > 0:
		for _, k := range [][]byte{opts.Key, opts.PreviousKey} {
			if len(k) == 0 {
				continue
			}
			tok, err := DeriveToken(k)
			if err != nil {
				return nil, err
			}
			c.tokens = append(c.tokens, []byte(tok))
		}
	default:
		return nil, ErrNoCredentials
	}

	// Trust anchors.
	switch {
	case opts.CAFile != "":
		pem, err := os.ReadFile(opts.CAFile)
		if err != nil {
			return nil, fmt.Errorf("internalapi: CA file: %w", err)
		}
		c.roots = x509.NewCertPool()
		if !c.roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("internalapi: CA file %s holds no PEM certificate", opts.CAFile)
		}
	case len(opts.Key) > 0:
		c.roots = x509.NewCertPool()
		for _, k := range [][]byte{opts.Key, opts.PreviousKey} {
			if len(k) == 0 {
				continue
			}
			ca, err := deriveCA(k)
			if err != nil {
				return nil, err
			}
			c.roots.AddCert(ca.cert)
		}
	}

	// Node certificate.
	switch {
	case opts.CertFile != "" || opts.KeyFile != "":
		if opts.CertFile == "" || opts.KeyFile == "" {
			return nil, errors.New("internalapi: tls certFile and keyFile must be set together")
		}
		pair, err := tls.LoadX509KeyPair(opts.CertFile, opts.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("internalapi: node certificate: %w", err)
		}
		c.fixed = &pair
	case len(opts.Key) > 0:
		// During a rotation every node trusts the previous CA (old nodes as
		// their current one, new nodes as the previous one), so node
		// certificates are issued by it until PreviousKey is removed.
		signer := opts.Key
		if len(opts.PreviousKey) > 0 {
			signer = opts.PreviousKey
		}
		ca, err := deriveCA(signer)
		if err != nil {
			return nil, err
		}
		c.ca = ca
		if _, err := c.certificate(); err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("internalapi: the internal listener is always TLS: set an encryption key or listeners.internal.tls certFile and keyFile")
	}
	return c, nil
}

// DeriveToken returns the bearer token every node holding key derives:
// base64url(HKDF-SHA256(key, salt nil, info TokenInfo, 32 bytes)).
func DeriveToken(key []byte) (string, error) {
	b, err := hkdf.Key(sha256.New, key, nil, TokenInfo, 32)
	if err != nil {
		return "", fmt.Errorf("internalapi: derive token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// DeriveCACertificate returns the self-signed CA certificate every node
// holding key derives; the bytes are identical on every node.
func DeriveCACertificate(key []byte) (*x509.Certificate, error) {
	ca, err := deriveCA(key)
	if err != nil {
		return nil, err
	}
	return ca.cert, nil
}

func deriveCA(key []byte) (*derivedCA, error) {
	seed, err := hkdf.Key(sha256.New, key, nil, CAInfo, ed25519.SeedSize)
	if err != nil {
		return nil, fmt.Errorf("internalapi: derive CA seed: %w", err)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	ski := sha256.Sum256(pub)
	tmpl := &x509.Certificate{
		SerialNumber:          new(big.Int).SetBytes(ski[:16]),
		Subject:               pkix.Name{CommonName: "kudurru internal CA", Organization: []string{"kudurru"}},
		NotBefore:             caNotBefore,
		NotAfter:              caNotAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
		SubjectKeyId:          ski[:20],
	}
	// Ed25519 signatures are deterministic, so the DER is identical on
	// every node; the reader is unused for Ed25519.
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return nil, fmt.Errorf("internalapi: create CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &derivedCA{cert: cert, key: priv}, nil
}

// certificate returns the node certificate, issuing a new one from the
// derived CA when none exists or the current one is close to expiry.
func (c *Credentials) certificate() (*tls.Certificate, error) {
	if c.fixed != nil {
		return c.fixed, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.leaf != nil && c.leaf.Leaf != nil && now.Before(c.leaf.Leaf.NotAfter.Add(-nodeCertRenew)) {
		return c.leaf, nil
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	dns := []string{c.serverName}
	if c.nodeName != "" && c.nodeName != c.serverName {
		dns = append(dns, c.nodeName)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: c.nodeName, Organization: []string{"kudurru"}},
		DNSNames:     dns,
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(nodeCertLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.ca.cert, pub, c.ca.key)
	if err != nil {
		return nil, fmt.Errorf("internalapi: issue node certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	c.leaf = &tls.Certificate{Certificate: [][]byte{der, c.ca.cert.Raw}, PrivateKey: priv, Leaf: leaf}
	return c.leaf, nil
}

// ServerTLS returns the listener's TLS configuration (TLS 1.3 only).
func (c *Credentials) ServerTLS() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return c.certificate()
		},
		NextProtos: []string{"h2", "http/1.1"},
	}
}

// ClientTLS returns the TLS configuration for dialling peers: the derived
// (or configured) CA, verified against ServerName.
func (c *Credentials) ClientTLS() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    c.roots,
		ServerName: c.serverName,
	}
}

// Token returns the bearer token this node sends.
func (c *Credentials) Token() string { return string(c.tokens[0]) }

// tokensToTry returns every token in the order a client should try them.
func (c *Credentials) tokensToTry() []string {
	out := make([]string, len(c.tokens))
	for i, t := range c.tokens {
		out[i] = string(t)
	}
	return out
}

// ValidToken reports in constant time whether got is an accepted token.
func (c *Credentials) ValidToken(got string) bool {
	ok := 0
	for _, t := range c.tokens {
		ok |= subtle.ConstantTimeCompare([]byte(got), t)
	}
	return ok == 1
}
