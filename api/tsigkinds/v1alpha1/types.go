// Package v1alpha1 defines private encrypted TSIG carriers, never public CRDs.
package v1alpha1

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Private carrier format and aggregate preparation bounds.
const (
	KindTSIGKey    = "TSIGKey"
	FormatVersion  = 1
	MaxKeys        = 256
	MaxSealedBytes = 8192
	MaxObjectBytes = 16 << 10
)

// ErrInvalid deliberately includes no encrypted or plaintext field values.
var ErrInvalid = errors.New("tsig key: invalid private carrier")

// TSIGKeySpec contains only public binding metadata and ciphertext.
type TSIGKeySpec struct {
	FormatVersion   int    `json:"formatVersion"`
	ClusterID       string `json:"clusterID"`
	KeyName         string `json:"keyName"`
	Algorithm       string `json:"algorithm"`
	CredentialEpoch string `json:"credentialEpoch"`
	Sealed          string `json:"sealed"`
}

// TSIGKey is a private SQLite row or Kubernetes Secret payload.
type TSIGKey struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              TSIGKeySpec `json:"spec"`
}

// TSIGKeyList is the private storage list carrier.
type TSIGKeyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TSIGKey `json:"items"`
}

// CanonicalName accepts plain canonical wire names, including underscore labels.
func CanonicalName(name string) string {
	name = dns.CanonicalName(strings.TrimSpace(name))
	if name == "." || len(name) > 254 || strings.ContainsAny(name, "\\\x00\r\n\t ") {
		return ""
	}
	for _, c := range []byte(name) {
		if c > 127 {
			return ""
		}
	}
	if _, ok := dns.IsDomainName(name); !ok {
		return ""
	}
	return name
}

// ObjectName is a lossless equality mapping to a DNS1123-safe Secret name.
func ObjectName(name string) string {
	if CanonicalName(name) != name || name == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("kudurru/tsig/name/v1\x00" + name))
	return "tsig-" + hex.EncodeToString(sum[:])
}

// ValidID bounds cluster and owner identifiers without disclosing them on error.
func ValidID(s string) bool {
	return s != "" && len(s) <= 256 && utf8.ValidString(s) && !strings.ContainsAny(s, "\x00\r\n\t")
}

// ValidEpoch identifies one durable credential incarnation independently of bytes.
func ValidEpoch(epoch string) bool {
	b, err := hex.DecodeString(epoch)
	return err == nil && len(b) == 16 && hex.EncodeToString(b) == epoch
}

// Validate checks public bindings and the bounded versioned AEAD envelope.
// Only an encryption owner can validate its authenticated contents.
func (k *TSIGKey) Validate() error {
	if k == nil || k.Namespace != "" || k.Name != ObjectName(k.Spec.KeyName) || k.Name == "" ||
		k.Spec.FormatVersion != FormatVersion || !ValidID(k.Spec.ClusterID) || !ValidEpoch(k.Spec.CredentialEpoch) ||
		(k.Spec.Algorithm != dns.HmacSHA256 && k.Spec.Algorithm != dns.HmacSHA512) ||
		len(k.Spec.Sealed) == 0 || len(k.Spec.Sealed) > MaxSealedBytes {
		return ErrInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(k.Spec.Sealed)
	if err != nil || len(raw) < 33 || raw[0] != 1 || base64.RawURLEncoding.EncodeToString(raw) != k.Spec.Sealed {
		return ErrInvalid
	}
	return nil
}

// PrivateCarrierLimit requests strict bounded decoding from a Secret driver.
// Malformed credential carriers cannot be treated as an absent key inventory.
func (*TSIGKey) PrivateCarrierLimit() int { return MaxObjectBytes }

// DeepCopy preserves opaque ciphertext while detaching mutable metadata.
func (k *TSIGKey) DeepCopy() *TSIGKey {
	if k == nil {
		return nil
	}
	out := *k
	out.ObjectMeta = *k.ObjectMeta.DeepCopy()
	return &out
}

// DeepCopyObject implements runtime.Object without generated public files.
func (k *TSIGKey) DeepCopyObject() runtime.Object {
	if k == nil {
		return nil
	}
	return k.DeepCopy()
}

// DeepCopyObject detaches all list members and metadata.
func (l *TSIGKeyList) DeepCopyObject() runtime.Object {
	if l == nil {
		return nil
	}
	out := *l
	l.DeepCopyInto(&out.ListMeta)
	out.Items = make([]TSIGKey, len(l.Items))
	for i := range l.Items {
		out.Items[i] = *l.Items[i].DeepCopy()
	}
	return &out
}

// AddToScheme registers only the private TSIG kind and list.
func AddToScheme(s *runtime.Scheme) error {
	s.AddKnownTypes(public.GroupVersion, &TSIGKey{}, &TSIGKeyList{})
	metav1.AddToGroupVersion(s, public.GroupVersion)
	return nil
}
