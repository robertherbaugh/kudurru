// Package memberidentity supplies persistent node-local TLS identity. An
// enrollment owner must independently authorize each Trust and retain its
// original membership cut through requests; this package grants no membership.
package memberidentity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ErrIdentity reports invalid, missing or untrusted identity without key data.
var ErrIdentity = errors.New("memberidentity: invalid or untrusted member identity")

const maxFileBytes = 8192

type diskIdentity struct {
	Version     int    `json:"version"`
	Member      string `json:"member"`
	PrivateKey  []byte `json:"privateKey"`
	Certificate []byte `json:"certificate"`
}

// Identity owns one private node CA. Its signing key never enters replica images
// or shared credentials. Leaf certificates rotate without replacing enrollment.
type Identity struct {
	trust Trust
	key   ed25519.PrivateKey
	mu    sync.Mutex
	leaf  *tls.Certificate
}

// Trust is one independently enrolled member CA. Its certificate is public, but
// importing it does not prove that the member is authorized by current policy.
type Trust struct {
	member string
	root   *x509.Certificate
}

func memberName(member string) string { return member + ".member.kudurru.invalid" }

func validMember(member string) bool {
	id, err := uuid.Parse(member)
	return err == nil && id != uuid.Nil && id.String() == member
}

// ParseTrust accepts a bounded, self-signed Ed25519 CA for the exact member UUID.
// Enrollment must authenticate these public bytes through an independent path.
func ParseTrust(member string, der []byte) (Trust, error) {
	if !validMember(member) || len(der) == 0 || len(der) > 4096 {
		return Trust{}, ErrIdentity
	}
	root, err := x509.ParseCertificate(bytes.Clone(der))
	if err != nil || !root.IsCA || !root.BasicConstraintsValid || !root.MaxPathLenZero || !bytes.Equal(root.RawSubject, root.RawIssuer) || root.Subject.CommonName != memberName(member) || root.KeyUsage != x509.KeyUsageCertSign || len(root.UnhandledCriticalExtensions) != 0 {
		return Trust{}, ErrIdentity
	}
	if _, ok := root.PublicKey.(ed25519.PublicKey); !ok || root.CheckSignatureFrom(root) != nil {
		return Trust{}, ErrIdentity
	}
	return Trust{member: member, root: root}, nil
}

// MemberID returns the independently bound UUID, never a peer's claimed header.
func (t Trust) MemberID() string { return t.member }

// Certificate returns detached public enrollment data, never private key bytes.
func (t Trust) Certificate() []byte {
	if t.root == nil {
		return nil
	}
	return bytes.Clone(t.root.Raw)
}

// Fingerprint identifies the exact enrolled CA certificate for review.
func (t Trust) Fingerprint() string {
	if t.root == nil {
		return ""
	}
	sum := sha256.Sum256(t.root.Raw)
	return hex.EncodeToString(sum[:])
}

// Trust returns this node's public identity for explicit enrollment.
func (i *Identity) Trust() Trust {
	if i == nil {
		return Trust{}
	}
	return i.trust
}

func serial() (*big.Int, error) {
	// A positive 128-bit serial avoids the invalid zero serial without retries.
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	return n.Add(n, big.NewInt(1)), nil
}

// Create exclusively creates a new 0600 identity file in an operator-controlled
// directory and syncs file and directory before returning. It never overwrites,
// follows a final symlink, repairs, or removes an existing or partial identity.
func Create(path, member string) (*Identity, error) {
	if !validMember(member) {
		return nil, ErrIdentity
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	n, err := serial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: n, Subject: pkix.Name{CommonName: memberName(member)},
		NotBefore: now.Add(-5 * time.Minute), NotAfter: now.AddDate(10, 0, 0),
		IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true,
		KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	if err != nil {
		return nil, err
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	// #nosec G117 -- private key persistence goes only through exclusive protected 0600 createFile, never a public response or replica image
	raw, err := json.Marshal(diskIdentity{Version: 1, Member: member, PrivateKey: encoded, Certificate: der})
	if err != nil {
		return nil, err
	}
	identity, err := decode(raw, member)
	if err != nil {
		return nil, err
	}
	if err := createFile(path, raw); err != nil {
		return nil, err
	}
	return identity, nil
}

// Load requires the existing identity for the original local member. Missing,
// malformed, indirect, permissive or wrong-member files refuse without repair.
func Load(path, member string) (*Identity, error) {
	raw, err := readFile(path)
	if err != nil {
		return nil, err
	}
	return decode(raw, member)
}

func decode(raw []byte, member string) (*Identity, error) {
	if len(raw) == 0 || len(raw) > maxFileBytes {
		return nil, ErrIdentity
	}
	var disk diskIdentity
	if json.Unmarshal(raw, &disk) != nil || disk.Version != 1 || disk.Member != member {
		return nil, ErrIdentity
	}
	// This is a private generated artifact. Exact canonical framing rejects
	// duplicate/unknown fields, alternate encodings and trailing documents.
	// #nosec G117 -- in-memory canonical comparison of the protected private key artifact; bytes are not exported
	canonical, err := json.Marshal(disk)
	if err != nil || !bytes.Equal(canonical, raw) {
		return nil, ErrIdentity
	}
	trust, err := ParseTrust(member, disk.Certificate)
	if err != nil {
		return nil, err
	}
	parsed, err := x509.ParsePKCS8PrivateKey(disk.PrivateKey)
	if err != nil {
		return nil, ErrIdentity
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok || !bytes.Equal(key.Public().(ed25519.PublicKey), trust.root.PublicKey.(ed25519.PublicKey)) {
		return nil, ErrIdentity
	}
	return &Identity{trust: trust, key: key}, nil
}

func (i *Identity) certificate() (*tls.Certificate, error) {
	if i == nil || i.trust.root == nil {
		return nil, ErrIdentity
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	now := time.Now()
	root := i.trust.root
	if now.Before(root.NotBefore) || !now.Before(root.NotAfter) {
		return nil, ErrIdentity
	}
	if i.leaf != nil && !now.Before(i.leaf.Leaf.NotBefore) && now.Before(i.leaf.Leaf.NotAfter.Add(-time.Hour)) {
		return i.leaf, nil
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	n, err := serial()
	if err != nil {
		return nil, err
	}
	end := now.Add(24 * time.Hour)
	if root.NotAfter.Before(end) {
		end = root.NotAfter
	}
	template := &x509.Certificate{
		SerialNumber: n, Subject: pkix.Name{CommonName: memberName(i.trust.member)},
		DNSNames:  []string{memberName(i.trust.member)},
		NotBefore: now.Add(-5 * time.Minute), NotAfter: end,
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, root, pub, i.key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	i.leaf = &tls.Certificate{Certificate: [][]byte{der, root.Raw}, PrivateKey: key, Leaf: leaf}
	return i.leaf, nil
}
