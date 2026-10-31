// Package auditkeys owns independent AuditLog trust material. Its private ring
// is provisioned by an operator, never derived or repaired from session keys.
package auditkeys

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	auditv1 "github.com/robertherbaugh/kudurru/api/audit/v1alpha1"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	// MaxBytes bounds a private ring file or Secret value.
	MaxBytes = 64 << 10
	// MaxKeys retains public keys without silently pruning historical trust.
	MaxKeys = 128
)

var (
	// ErrUnavailable includes missing, unsupported, revoked or incomplete trust.
	ErrUnavailable = errors.New("audit keys: trusted verification material unavailable")
	// ErrIntegrity denotes a malformed seal or invalid signature under known trust.
	ErrIntegrity = errors.New("audit keys: batch signature does not verify")
)

type publicKey struct {
	ID      string `json:"id"`
	Public  []byte `json:"public"`
	Revoked bool   `json:"revoked,omitempty"`
}
type document struct {
	Version       int         `json:"version"`
	Domain        string      `json:"domain"`
	ClusterID     string      `json:"clusterID"`
	Namespace     string      `json:"namespace"`
	Active        string      `json:"active"`
	Seed          []byte      `json:"seed"`
	Keys          []publicKey `json:"keys"`
	LegacyBefore  *time.Time  `json:"legacyBefore,omitempty"`
	Legacy        [][]byte    `json:"legacy,omitempty"`
	LegacyRevoked bool        `json:"legacyRevoked,omitempty"`
}

// Ring is immutable. Private material has only an explicit protected-file codec.
type Ring struct{ doc document }

func (Ring) String() string { return "private audit key ring" }

// GoString also redacts diagnostic formatting of both pointer and value forms.
func (r Ring) GoString() string { return r.String() }

// MarshalJSON prevents accidental private-key disclosure through API/log DTOs.
func (Ring) MarshalJSON() ([]byte, error) { return []byte(`{"private":true}`), nil }

func fingerprint(public []byte) string {
	sum := sha256.Sum256(public)
	return hex.EncodeToString(sum[:])
}
func validScope(cluster, namespace string) bool {
	return cluster != "" && len(cluster) <= 512 && strings.TrimSpace(cluster) == cluster && len(validation.IsDNS1123Label(namespace)) == 0
}

// New initializes one independent trust domain. Legacy trust is explicit and
// bounded to a past cutover; rotation cannot change that cutover.
func New(cluster, namespace string, legacy [][]byte, before *time.Time, now time.Time) (*Ring, error) {
	if !validScope(cluster, namespace) || len(legacy) > 16 || (len(legacy) > 0) != (before != nil) || before != nil && (before.IsZero() || before.Nanosecond() != 0 || !before.Before(now)) {
		return nil, ErrUnavailable
	}
	seed := make([]byte, ed25519.SeedSize)
	_, _ = rand.Read(seed)
	domain := make([]byte, 16)
	_, _ = rand.Read(domain)
	public := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	d := document{Version: 1, Domain: hex.EncodeToString(domain), ClusterID: cluster, Namespace: namespace, Active: fingerprint(public), Seed: seed, Keys: []publicKey{{ID: fingerprint(public), Public: public}}}
	if before != nil {
		v := before.UTC()
		d.LegacyBefore = &v
	}
	for _, key := range legacy {
		if len(key) != 32 {
			return nil, ErrUnavailable
		}
		d.Legacy = append(d.Legacy, bytes.Clone(key))
	}
	return &Ring{doc: d}, nil
}

// EncodePrivate is only for a protected operator file or private Secret value.
// Ordinary JSON marshaling is always redacted.
func (r *Ring) EncodePrivate() ([]byte, error) {
	if r == nil {
		return nil, ErrUnavailable
	}
	return json.Marshal(r.doc)
}

// Parse strictly validates a detached immutable private ring.
func Parse(data []byte) (*Ring, error) {
	if len(data) == 0 || len(data) > MaxBytes || !uniqueJSON(data) {
		return nil, ErrUnavailable
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var d document
	if dec.Decode(&d) != nil || dec.Decode(new(any)) != io.EOF || d.Version != 1 || !validScope(d.ClusterID, d.Namespace) || len(d.Domain) != 32 || len(d.Keys) == 0 || len(d.Keys) > MaxKeys || len(d.Seed) != 32 || len(d.Legacy) > 16 || (len(d.Legacy) > 0) != (d.LegacyBefore != nil) {
		return nil, ErrUnavailable
	}
	if b, err := hex.DecodeString(d.Domain); err != nil || hex.EncodeToString(b) != d.Domain {
		return nil, ErrUnavailable
	}
	if d.LegacyBefore != nil && (d.LegacyBefore.IsZero() || d.LegacyBefore.Nanosecond() != 0 || !d.LegacyBefore.Before(time.Now())) {
		return nil, ErrUnavailable
	}
	active := false
	seen := map[string]bool{}
	for _, key := range d.Keys {
		if len(key.Public) != ed25519.PublicKeySize || key.ID != fingerprint(key.Public) || seen[key.ID] {
			return nil, ErrUnavailable
		}
		seen[key.ID] = true
		if key.ID == d.Active {
			active = !key.Revoked && bytes.Equal(key.Public, ed25519.NewKeyFromSeed(d.Seed).Public().(ed25519.PublicKey))
		}
	}
	if !active {
		return nil, ErrUnavailable
	}
	for _, key := range d.Legacy {
		if len(key) != 32 {
			return nil, ErrUnavailable
		}
	}
	return &Ring{doc: d}, nil
}

// ScopeMatches requires the original cluster and namespace, never a successor.
func (r *Ring) ScopeMatches(cluster, namespace string) bool {
	return r != nil && cluster != "" && r.doc.ClusterID == cluster && r.doc.Namespace == namespace
}

// ActiveID is a public fingerprint, not secret material.
func (r *Ring) ActiveID() string { return r.doc.Active }

// Count reports retained public verification keys.
func (r *Ring) Count() int { return len(r.doc.Keys) }

// Rotate changes only the active signer, retaining every original public key.
func (r *Ring) Rotate() (*Ring, error) {
	if r == nil || len(r.doc.Keys) >= MaxKeys {
		return nil, ErrUnavailable
	}
	data, _ := r.EncodePrivate()
	next, err := Parse(data)
	if err != nil {
		return nil, err
	}
	seed := make([]byte, 32)
	_, _ = rand.Read(seed)
	public := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	next.doc.Seed = seed
	next.doc.Active = fingerprint(public)
	next.doc.Keys = append(next.doc.Keys, publicKey{ID: next.doc.Active, Public: public})
	return next, nil
}

// Revoke retains evidence but stops asserting trust in a compromised old signer.
func (r *Ring) Revoke(id string) (*Ring, error) {
	if r == nil || id == r.doc.Active {
		return nil, ErrUnavailable
	}
	data, _ := r.EncodePrivate()
	next, err := Parse(data)
	if err != nil {
		return nil, err
	}
	for i := range next.doc.Keys {
		if next.doc.Keys[i].ID == id {
			next.doc.Keys[i].Revoked = true
			return next, nil
		}
	}
	return nil, ErrUnavailable
}

func (r *Ring) payload(id string, spec auditv1.AuditLogSpec) ([]byte, error) {
	spec.Seal = ""
	body, err := json.Marshal(spec)
	if err != nil {
		return nil, ErrUnavailable
	}
	var out bytes.Buffer
	for _, field := range []string{"kudurru/audit/ed25519/v1", r.doc.Domain, r.doc.ClusterID, r.doc.Namespace, id, string(body)} {
		out.WriteString(strconv.Itoa(len(field)))
		out.WriteByte(':')
		out.WriteString(field)
	}
	return out.Bytes(), nil
}

// Sign seals a new batch without changing its source events or hashes.
func (r *Ring) Sign(spec auditv1.AuditLogSpec) (string, error) {
	if r == nil || len(r.doc.Seed) != 32 {
		return "", ErrUnavailable
	}
	body, err := r.payload(r.doc.Active, spec)
	if err != nil {
		return "", err
	}
	sig := ed25519.Sign(ed25519.NewKeyFromSeed(r.doc.Seed), body)
	return "ed25519-v1:" + r.doc.Domain + ":" + r.doc.Active + ":" + base64.RawStdEncoding.EncodeToString(sig), nil
}

// Verify checks a signature under the captured ring. Legacy admission requires
// its explicit original cutoff and authenticated apiserver creation metadata.
func (r *Ring) Verify(spec auditv1.AuditLogSpec, created time.Time) error {
	if r == nil {
		return ErrUnavailable
	}
	if strings.Contains(spec.Seal, ":") {
		parts := strings.Split(spec.Seal, ":")
		if len(parts) != 4 {
			return ErrIntegrity
		}
		if parts[0] != "ed25519-v1" {
			return ErrUnavailable
		}
		if parts[1] != r.doc.Domain {
			return ErrUnavailable
		}
		for _, key := range r.doc.Keys {
			if key.ID == parts[2] {
				if key.Revoked {
					return ErrUnavailable
				}
				sig, err := base64.RawStdEncoding.DecodeString(parts[3])
				if err != nil || len(sig) != ed25519.SignatureSize {
					return ErrIntegrity
				}
				body, err := r.payload(key.ID, spec)
				if err != nil {
					return err
				}
				if !ed25519.Verify(key.Public, body, sig) {
					return ErrIntegrity
				}
				return nil
			}
		}
		return ErrUnavailable
	}
	got, err := hex.DecodeString(spec.Seal)
	if err != nil || len(got) != sha256.Size {
		return ErrIntegrity
	}
	if r.doc.LegacyRevoked || r.doc.LegacyBefore == nil || created.IsZero() || !created.Before(*r.doc.LegacyBefore) {
		return ErrUnavailable
	}
	spec.Seal = ""
	body, err := json.Marshal(spec)
	if err != nil {
		return ErrIntegrity
	}
	for _, key := range r.doc.Legacy {
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write(body)
		if hmac.Equal(got, mac.Sum(nil)) {
			return nil
		}
	}
	return ErrIntegrity
}

// VerificationOnly drops the active private seed from an immutable scan cut.
func (r *Ring) VerificationOnly() *Ring {
	if r == nil {
		return nil
	}
	d := r.doc
	d.Seed = nil
	return &Ring{doc: d}
}

// RevokeLegacy explicitly withdraws trust in all imported legacy HMAC keys.
func (r *Ring) RevokeLegacy() (*Ring, error) {
	if r == nil || len(r.doc.Legacy) == 0 {
		return nil, ErrUnavailable
	}
	data, _ := r.EncodePrivate()
	next, err := Parse(data)
	if err != nil {
		return nil, err
	}
	next.doc.LegacyRevoked = true
	return next, nil
}
