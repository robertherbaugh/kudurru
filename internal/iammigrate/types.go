package iammigrate

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// JournalName is the reserved private admission journal, never a public kind.
const JournalName = "kudurru-iam-migration-v1"

const (
	journalType    = "kudurru.io/iam-migration-journal"
	archiveType    = "kudurru.io/iam-migration-source"
	internalType   = "kudurru.io/internal-object"
	kindLabel      = "kudurru.io/kind"
	namespaceLabel = "kudurru.io/namespace"
	nameAnnotation = "kudurru.io/name"
	journalVersion = 1
)

// State is safe to display in operator diagnostics.
type State string

// Admission states; only Native and Complete allow remote admission.
const (
	Native           State = "native"
	Required         State = "required"
	Running          State = "running"
	RecoveryRequired State = "recovery-required"
	Complete         State = "complete"
	Blocked          State = "blocked"
)

// Problem contains identifiers only, never underlying API/decode error text.
type Problem struct{ Code, Kind, Namespace, Name, Step string }

func (p *Problem) Error() string {
	return fmt.Sprintf("IAM migration %s (%s %s/%s, %s)", p.Code, p.Kind, p.Namespace, p.Name, p.Step)
}

// Counts describes a plan without exposing hashes or identity documents.
type Counts struct{ Sources, Public, Authorities, BoundTokens, UnboundTokens int }

// Status is a defensive diagnostic snapshot. Check errors never allow admission.
type Status struct {
	State                  State
	RemoteAllowed          bool
	JournalResourceVersion string
	MigrationID            string
	Counts                 Counts
	Problem                *Problem
}

// OfflineAcknowledgement attests that readers, writers and prior migrators are stopped.
// It cannot establish that fact remotely; the operator owns deployment isolation.
type OfflineAcknowledgement struct{ ReadersAndWritersStopped bool }

// Limits may lower, but never raise, the hard resource caps.
type Limits struct {
	Objects, ObjectBytes, TotalBytes, JournalBytes, PageSize int
	RequestTimeout                                           time.Duration
}

// Options contains host-owned capabilities, never public object selections.
type Options struct {
	Namespace    string
	AllowedFiles []string
	Limits       Limits
}

// Migrator uses only uncached API clients. It has no mutable request/auth cache.
type Migrator struct {
	core    kubernetes.Interface
	dynamic dynamic.Interface
	opts    Options
	files   map[string]bool
}

// New validates and copies host settings; it does no I/O.
func New(core kubernetes.Interface, dc dynamic.Interface, opts Options) (*Migrator, error) {
	if core == nil || dc == nil || len(validation.IsDNS1123Label(opts.Namespace)) != 0 {
		return nil, problem("invalid-options", "", "", "", "new")
	}
	if err := opts.Limits.defaults(); err != nil {
		return nil, err
	}
	files := make(map[string]bool, len(opts.AllowedFiles))
	for _, f := range opts.AllowedFiles {
		if !filepath.IsAbs(f) || filepath.Clean(f) != f {
			return nil, problem("credentials-denied", "", "", "", "new")
		}
		files[f] = true
	}
	opts.AllowedFiles = nil
	return &Migrator{core: core, dynamic: dc, opts: opts, files: files}, nil
}

func (l *Limits) defaults() error {
	values := []struct {
		value *int
		cap   int
	}{{&l.Objects, 1024}, {&l.ObjectBytes, 256 << 10}, {&l.TotalBytes, 16 << 20}, {&l.JournalBytes, 512 << 10}, {&l.PageSize, 128}}
	for _, v := range values {
		if *v.value == 0 {
			*v.value = v.cap
		}
		if *v.value < 1 || *v.value > v.cap {
			return problem("bounds-exceeded", "", "", "", "options")
		}
	}
	if l.RequestTimeout == 0 {
		l.RequestTimeout = 15 * time.Second
	}
	if l.RequestTimeout < 0 || l.RequestTimeout > 30*time.Second {
		return problem("bounds-exceeded", "", "", "", "options")
	}
	return nil
}

func (m *Migrator) request(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, m.opts.Limits.RequestTimeout)
}
func problem(code, kind, ns, name, step string) *Problem {
	return &Problem{Code: code, Kind: kind, Namespace: ns, Name: name, Step: step}
}
func secretName(kind, ns, name string) string {
	if ns != "" {
		return "kudurru-" + lower(kind) + "." + ns + "." + name
	}
	return "kudurru-" + lower(kind) + "." + name
}

var publicResources = map[string]schema.GroupVersionResource{
	"ServiceAccount":   {Group: "kudurru.io", Version: "v1alpha1", Resource: "serviceaccounts"},
	"RoleBinding":      {Group: "kudurru.io", Version: "v1alpha1", Resource: "rolebindings"},
	"IdentityProvider": {Group: "kudurru.io", Version: "v1alpha1", Resource: "identityproviders"},
}

type key struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}
type sourceRecord struct {
	Key     key       `json:"key"`
	Secret  string    `json:"secret"`
	UID     types.UID `json:"uid"`
	RV      string    `json:"rv"`
	Digest  string    `json:"digest"`
	Archive string    `json:"archive"`
}
type receipt struct {
	Key        key       `json:"key"`
	Digest     string    `json:"digest"`
	Intent     bool      `json:"intent"`
	UID        types.UID `json:"uid"`
	StatusDone bool      `json:"statusDone"`
}
type journal struct {
	Version      int                `json:"version"`
	Converter    int                `json:"converter"`
	Cluster      types.UID          `json:"cluster"`
	NamespaceUID types.UID          `json:"namespaceUID"`
	Namespace    string             `json:"namespace"`
	ID           string             `json:"id"`
	Owner        string             `json:"owner"`
	State        State              `json:"state"`
	Sources      []sourceRecord     `json:"sources"`
	Manifest     string             `json:"manifest"`
	Public       []receipt          `json:"public"`
	Private      []receipt          `json:"private"`
	Credentials  []credentialRecord `json:"credentials"`
	Counts       Counts             `json:"counts"`
	Completion   string             `json:"completion"`
}

func status(j *journal, rv string) Status {
	return Status{State: j.State, RemoteAllowed: j.State == Complete, MigrationID: j.ID, JournalResourceVersion: rv, Counts: j.Counts}
}
