// Package v1alpha1 defines private Integration execution records. These are
// shared durable runtime data, not CRDs, public desired objects or credentials.
package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Runtime format, kind names, and conservative persistence bounds.
const (
	FormatVersion            = 1
	KindIntegrationLedger    = "IntegrationLedger"
	KindIntegrationOperation = "IntegrationOperation"
	KindIntegrationBudget    = "IntegrationBudget"
	KindIntegrationDelivery  = "IntegrationDelivery"
	MaxObjectBytes           = 256 << 10
	MaxTargets               = 128
	MaxPayloadBytes          = 64 << 10
)

// Kinds must be registered as private kinds in every carrier and replication
// manifest. Objects are cluster-scoped; source namespace is part of Owner.
var Kinds = []string{KindIntegrationBudget, KindIntegrationDelivery, KindIntegrationLedger, KindIntegrationOperation}

// Owner identifies one Integration incarnation, including its cluster.
type Owner struct {
	ClusterID string `json:"clusterID"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

// Destination isolates provider account and remote zone ownership.
type Destination struct {
	Provider string `json:"provider"`
	Account  string `json:"account"`
	Zone     string `json:"zone"`
}

// RecordKey identifies a canonical RRset, including provider routing identity.
type RecordKey struct {
	Name          string `json:"name"`
	Type          string `json:"type"`
	SetIdentifier string `json:"setIdentifier,omitempty"`
}

// RecordIdentity survives spec/credential rotation but never UID recreation.
type RecordIdentity struct {
	Owner       Owner       `json:"owner"`
	Destination Destination `json:"destination"`
	Key         RecordKey   `json:"key"`
}

// Pending reserves one immutable operation against the current baseline.
type Pending struct {
	OperationID string `json:"operationID"`
	PlanDigest  string `json:"planDigest"`
	BeforeHash  string `json:"beforeHash,omitempty"`
	AfterHash   string `json:"afterHash,omitempty"`
}

// IntegrationLedgerSpec retains the last confirmed baseline through failures.
type IntegrationLedgerSpec struct {
	FormatVersion int            `json:"formatVersion"`
	Record        RecordIdentity `json:"record"`
	Sequence      uint64         `json:"sequence,string"`
	ContentHash   string         `json:"contentHash,omitempty"`
	// Policy retains the last applied deletion restriction after source removal.
	Policy          string    `json:"policy,omitempty"`
	ProviderIDs     []string  `json:"providerIDs,omitempty"`
	LastApplied     time.Time `json:"lastApplied,omitempty"`
	LastOperationID string    `json:"lastOperationID,omitempty"`
	Pending         *Pending  `json:"pending,omitempty"`
}

// TargetResult records confirmed partial outcomes without treating them as a
// complete RRset. The executor rereads ambiguous remote state before retrying.
type TargetResult struct {
	Target     string `json:"target"`
	ProviderID string `json:"providerID,omitempty"`
	Applied    bool   `json:"applied"`
	ErrorClass string `json:"errorClass,omitempty"`
}

// Receipt records bounded provider facts; partial results never confirm a ledger.
type Receipt struct {
	Complete    bool           `json:"complete"`
	ContentHash string         `json:"contentHash,omitempty"`
	ProviderIDs []string       `json:"providerIDs,omitempty"`
	Targets     []TargetResult `json:"targets,omitempty"`
	ObservedAt  time.Time      `json:"observedAt"`
	ErrorClass  string         `json:"errorClass,omitempty"`
}

// IntegrationOperationSpec is an immutable intent plus a monotone receipt. Empty
// Before/AfterHash means absence, never an empty RRset's content digest.
type IntegrationOperationSpec struct {
	FormatVersion    int              `json:"formatVersion"`
	Record           RecordIdentity   `json:"record"`
	OperationID      string           `json:"operationID"`
	PlanDigest       string           `json:"planDigest"`
	ConfigDigest     string           `json:"configDigest"`
	CredentialDigest string           `json:"credentialDigest"`
	Action           string           `json:"action"`
	Policy           string           `json:"policy,omitempty"`
	BeforeHash       string           `json:"beforeHash,omitempty"`
	AfterHash        string           `json:"afterHash,omitempty"`
	Before           *public.Endpoint `json:"before,omitempty"`
	After            *public.Endpoint `json:"after,omitempty"`
	ProviderIDs      []string         `json:"providerIDs,omitempty"`
	RemoteVersions   []string         `json:"remoteVersions,omitempty"`
	CreatedAt        time.Time        `json:"createdAt"`
	// AbandonedAt terminalizes an untouched intent before releasing its ledger.
	AbandonedAt time.Time `json:"abandonedAt,omitempty"`
	Receipt     *Receipt  `json:"receipt,omitempty"`
}

// BudgetReservation records one idempotent shared request-budget charge.
type BudgetReservation struct {
	ID   string `json:"id"`
	Cost uint32 `json:"cost"`
}

// IntegrationBudgetSpec coordinates a fixed request window across elected nodes.
// Account is a provider account identifier, never a credential value/hash.
type IntegrationBudgetSpec struct {
	FormatVersion int                 `json:"formatVersion"`
	ClusterID     string              `json:"clusterID"`
	Provider      string              `json:"provider"`
	Account       string              `json:"account"`
	Sequence      uint64              `json:"sequence,string"`
	WindowStart   time.Time           `json:"windowStart"`
	WindowSeconds uint32              `json:"windowSeconds"`
	Capacity      uint32              `json:"capacity"`
	Reservations  []BudgetReservation `json:"reservations,omitempty"`
}

// IntegrationDeliverySpec stores a bounded sanitized event and retry state. It
// never grants authorization to invoke actions or resolve credential references.
type IntegrationDeliverySpec struct {
	FormatVersion int             `json:"formatVersion"`
	Owner         Owner           `json:"owner"`
	DeliveryID    string          `json:"deliveryID"`
	Topic         string          `json:"topic"`
	Payload       json.RawMessage `json:"payload"`
	CreatedAt     time.Time       `json:"createdAt"`
	Deadline      time.Time       `json:"deadline"`
	NextAttempt   time.Time       `json:"nextAttempt"`
	Attempts      uint32          `json:"attempts"`
	Acknowledged  bool            `json:"acknowledged"`
	DeadLetter    bool            `json:"deadLetter"`
	ErrorClass    string          `json:"errorClass,omitempty"`
}

// IntegrationLedger is private shared ownership state, never public configuration.
type IntegrationLedger struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              IntegrationLedgerSpec `json:"spec"`
}

// IntegrationLedgerList is a private ledger carrier list.
type IntegrationLedgerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IntegrationLedger `json:"items"`
}

// IntegrationOperation is a private durable intent and its provider receipt.
type IntegrationOperation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              IntegrationOperationSpec `json:"spec"`
}

// IntegrationOperationList is a private operation carrier list.
type IntegrationOperationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IntegrationOperation `json:"items"`
}

// IntegrationBudget is the private shared account request-budget carrier.
type IntegrationBudget struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              IntegrationBudgetSpec `json:"spec"`
}

// IntegrationBudgetList is a private budget carrier list.
type IntegrationBudgetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IntegrationBudget `json:"items"`
}

// IntegrationDelivery is a private bounded webhook delivery carrier.
type IntegrationDelivery struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              IntegrationDeliverySpec `json:"spec"`
}

// IntegrationDeliveryList is a private delivery carrier list.
type IntegrationDeliveryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IntegrationDelivery `json:"items"`
}

// Digest returns a lowercase SHA256 content fingerprint.
func Digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

// ValidDigest accepts only canonical SHA256 hexadecimal fingerprints.
func ValidDigest(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == sha256.Size && hex.EncodeToString(b) == s
}
func objectName(prefix string, v any) string {
	b, _ := json.Marshal(v)
	d := Digest(b)
	return prefix + d[:32] + "." + d[32:]
}

// LedgerName binds durable baseline name to UID, destination and RRset.
func LedgerName(id RecordIdentity) string { return objectName("il-", id) }

// OperationName returns a bounded object name for a valid operation digest.
func OperationName(id string) string {
	if !ValidDigest(id) {
		return ""
	}
	return "io-" + id[:32] + "." + id[32:]
}

// BudgetName binds shared budget state to cluster and provider account.
func BudgetName(cluster, provider, account string) string {
	return objectName("ib-", []string{cluster, provider, account})
}

// DeliveryName isolates stable event delivery IDs by Integration incarnation.
func DeliveryName(owner Owner, id string) string {
	return objectName("id-", struct {
		Owner Owner
		ID    string
	}{owner, id})
}

// IsInternal recognizes only the private runtime kind names.
func IsInternal(kind string) bool {
	for _, k := range Kinds {
		if kind == k {
			return true
		}
	}
	return false
}
