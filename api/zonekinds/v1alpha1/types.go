// Package v1alpha1 defines private committed-zone storage kinds. They are
// runtime data, not IAM secrets or user-editable desired resources. Register
// them with the store scheme and private Kubernetes Secret mapping explicitly.
package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Committed-zone format identifiers, bounds and lifecycle states.
const (
	FormatVersion       = 1
	KindZoneState       = "ZoneState"
	KindZoneChunk       = "ZoneChunk"
	MaxChunkBytes       = 256 << 10
	MaxHeadBytes        = 256 << 10
	LifecycleActive     = "active"
	LifecycleTombstoned = "tombstoned"
)

// Kinds lists private zone kinds for explicit store registration.
var Kinds = []string{KindZoneChunk, KindZoneState, KindZoneLifecycle}

// ZoneID fences cluster restore and delete/recreate, even for the same apex.
type ZoneID struct {
	ClusterID  string `json:"clusterID"`
	ObjectName string `json:"objectName"`
	UID        string `json:"uid"`
	Apex       string `json:"apex"`
}

// ZoneStateSpec identifies one durable head and its notification intent.
type ZoneStateSpec struct {
	FormatVersion  int    `json:"formatVersion"`
	Zone           ZoneID `json:"zone"`
	CommitSequence uint64 `json:"commitSequence"`
	Serial         uint32 `json:"serial"`
	AuthEpoch      uint64 `json:"authEpoch"`
	AuthDigest     string `json:"authDigest"`
	RootDigest     string `json:"rootDigest"`
	ContentHash    string `json:"contentHash"`
	Lifecycle      string `json:"lifecycle"`
	// NotifySequence records the data commit or explicit request requiring
	// coalesced delivery of the committed serial.
	// Root clears/acknowledges it through a later metadata CAS, never by
	// changing the committed data or serial independently.
	NotifySequence uint64 `json:"notifySequence,omitempty"`
	NotifySerial   uint32 `json:"notifySerial,omitempty"`
}

// ZoneChunkSpec stores bounded content addressed by its digest.
type ZoneChunkSpec struct {
	FormatVersion int    `json:"formatVersion"`
	Digest        string `json:"digest"`
	Data          []byte `json:"data"`
}

// ZoneState is the private durable head of a committed zone.
type ZoneState struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ZoneStateSpec `json:"spec"`
}

// ZoneStateList holds a collection of private zone heads.
type ZoneStateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ZoneState `json:"items"`
}

// ZoneChunk stores one immutable piece of committed zone content.
type ZoneChunk struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ZoneChunkSpec `json:"spec"`
}

// ZoneChunkList holds a collection of immutable zone chunks.
type ZoneChunkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ZoneChunk `json:"items"`
}

// Digest returns the canonical lowercase SHA-256 digest of data.
func Digest(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }

// ValidDigest reports whether s is a canonical lowercase SHA-256 digest.
func ValidDigest(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == sha256.Size && hex.EncodeToString(b) == s
}
func digestName(prefix, digest string) string { return prefix + digest[:32] + "." + digest[32:] }

// HeadName derives the private storage name from the full zone identity.
func HeadName(id ZoneID) string {
	return digestName("zs-", Digest([]byte(id.ClusterID+"\x00"+id.ObjectName+"\x00"+id.UID+"\x00"+id.Apex)))
}

// ChunkName derives the storage name for a valid digest, or returns empty.
func ChunkName(digest string) string {
	if !ValidDigest(digest) {
		return ""
	}
	return digestName("zc-", digest)
}

// Validate checks a head's identity, version, digests and notification bounds.
func (s *ZoneState) Validate() error {
	if s.Spec.FormatVersion != FormatVersion || s.Spec.CommitSequence == 0 || s.Spec.Zone.ClusterID == "" || s.Spec.Zone.ObjectName == "" || s.Spec.Zone.UID == "" || s.Spec.Zone.Apex == "" {
		return fmt.Errorf("invalid ZoneState identity/version/sequence")
	}
	for _, str := range []string{s.Spec.Zone.ClusterID, s.Spec.Zone.ObjectName, s.Spec.Zone.UID, s.Spec.Zone.Apex} {
		if len(str) > 512 {
			return fmt.Errorf("ZoneState identity too large")
		}
	}
	if s.Name != HeadName(s.Spec.Zone) || s.Namespace != "" || !ValidDigest(s.Spec.RootDigest) || !ValidDigest(s.Spec.ContentHash) || !ValidDigest(s.Spec.AuthDigest) {
		return fmt.Errorf("invalid ZoneState name/digests")
	}
	if s.Spec.Lifecycle != LifecycleActive && s.Spec.Lifecycle != LifecycleTombstoned {
		return fmt.Errorf("invalid ZoneState lifecycle")
	}
	if s.Spec.NotifySequence > s.Spec.CommitSequence {
		return fmt.Errorf("invalid notification sequence")
	}
	return nil
}

// Validate checks a chunk's identity, size and full content digest.
func (c *ZoneChunk) Validate() error {
	if c.Spec.FormatVersion != FormatVersion || len(c.Spec.Data) == 0 || len(c.Spec.Data) > MaxChunkBytes || !ValidDigest(c.Spec.Digest) || Digest(c.Spec.Data) != c.Spec.Digest || c.Name != ChunkName(c.Spec.Digest) || c.Namespace != "" {
		return fmt.Errorf("invalid ZoneChunk name/version/digest/data")
	}
	return nil
}
