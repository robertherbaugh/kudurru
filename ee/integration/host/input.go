// Package host coordinates elected background integrations. It consumes
// captured source objects and credentials, never query-path or live providers.
package host

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/ee/integration/ledger"
	"github.com/robertherbaugh/kudurru/ee/integration/plan"
	"github.com/robertherbaugh/kudurru/internal/credentials"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

// MaxInstances bounds captured instance and election state per host.
const MaxInstances = 128

// Capture errors are sanitized; resolved credential values never enter errors.
var (
	ErrInvalid     = errors.New("integration host: invalid capture")
	ErrCredentials = errors.New("integration host: credentials unavailable")
	ErrCapacity    = errors.New("integration host: instance capacity exceeded")
)

// Input is one immutable root-owned background capture. Sources must be
// original admitted objects (including public-only records); provider and DNS
// publication adapters must not substitute live objects under this capture.
type Input struct {
	Objects     zonecache.ObjectSet
	Credentials *credentials.Resolver
	Sources     []plan.Source
	Term        replication.Term
	BundleID    string
	Complete    bool
	// ReconcileDigests optionally scopes source wakeups to one captured instance.
	// Values are deterministic digests of its desired inputs, excluding the host's
	// own admitted observation/receipt/status writes. BundleID still fences reads.
	ReconcileDigests map[InstanceID]string
	// AuthorityDigests optionally binds each instance to only its complete
	// authority dependencies. Root must still validate original object versions
	// at effects; equal authority never permits replacing held request inputs.
	AuthorityDigests map[InstanceID]string
	// Resource is a root-owned immutable resource captured with these objects.
	// It is retained by original request frames, never serialized or acquired.
	Resource any `json:"-"`
}

// Instance retains one private definition and resolved named capabilities.
// It owns copies; callers never receive references to its mutable data.
type Instance struct {
	owner                          kinds.Owner
	definition                     *public.Integration
	providerSpec                   json.RawMessage
	configDigest, credentialDigest string
	fingerprints                   map[string]string
	secrets                        map[string][]byte
	term                           replication.Term
	bundleID                       string
	credentialError                bool
}

// CaptureInstances validates a complete capture before returning any instances.
// Credential failures pause only their own instance; they are reported without
// leaking reference/error bodies. Missing values are never silently reused.
func CaptureInstances(ctx context.Context, in Input) ([]*Instance, error) {
	if !in.Complete || in.Term.Validate() != nil || in.BundleID == "" || len(in.BundleID) > 512 {
		return nil, ErrInvalid
	}
	if len(in.ReconcileDigests) > MaxInstances || len(in.AuthorityDigests) > MaxInstances {
		return nil, ErrCapacity
	}
	for _, digests := range []map[InstanceID]string{in.ReconcileDigests, in.AuthorityDigests} {
		for owner, digest := range digests {
			if owner.Validate() != nil || !kinds.ValidDigest(digest) {
				return nil, ErrInvalid
			}
		}
	}
	objects := in.Objects.List(store.Kind("Integration"))
	if len(objects) > MaxInstances {
		return nil, ErrCapacity
	}
	out := make([]*Instance, 0, len(objects))
	seen := map[string]bool{}
	for _, obj := range objects {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		d, ok := obj.(*public.Integration)
		if !ok || d == nil {
			return nil, ErrInvalid
		}
		d = d.DeepCopy()
		owner := kinds.Owner{ClusterID: in.Term.ClusterID, Namespace: d.Namespace, Name: d.Name, UID: string(d.UID)}
		if owner.Validate() != nil || d.ResourceVersion == "" || len(d.ResourceVersion) > 512 || len(d.Spec.Credentials) > 16 {
			return nil, ErrInvalid
		}
		name := d.Namespace + "/" + d.Name
		if seen[name] {
			return nil, ErrInvalid
		}
		seen[name] = true
		spec, err := providerSpec(d.Spec)
		if err != nil {
			return nil, err
		}
		definitionJSON, err := json.Marshal(d.Spec)
		if err != nil || len(definitionJSON) > kinds.MaxObjectBytes {
			return nil, ErrInvalid
		}
		i := &Instance{owner: owner, definition: d, providerSpec: spec, configDigest: kinds.Digest(definitionJSON), fingerprints: map[string]string{}, secrets: map[string][]byte{}, term: in.Term, bundleID: in.BundleID}
		if digest, present := in.AuthorityDigests[owner]; present {
			i.bundleID = kinds.Digest([]byte(in.BundleID + "/" + digest))
		}
		names := slices.Sorted(maps.Keys(d.Spec.Credentials))
		if d.Spec.Type == "cloudflare" && d.Spec.Credentials["apiToken"] == (public.CredentialReference{}) || d.Spec.Type == "unifi" && d.Spec.Credentials["apiKey"] == (public.CredentialReference{}) {
			i.credentialError = true
		}
		for _, name := range names {
			if name == "" || len(name) > 128 || strings.ContainsAny(name, "\x00\r\n\t ") {
				return nil, ErrInvalid
			}
			if in.Credentials == nil {
				i.credentialError = true
				continue
			}
			value, err := in.Credentials.Resolve(ctx, d.Spec.Credentials[name])
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				i.credentialError = true
				continue
			}
			i.secrets[name] = slices.Clone(value.Bytes)
			i.fingerprints[name] = value.Fingerprint
			clear(value.Bytes)
		}
		if i.credentialError {
			for name, value := range i.secrets {
				clear(value)
				delete(i.secrets, name)
			}
		}
		b, err := json.Marshal(i.fingerprints)
		if err != nil {
			return nil, ErrInvalid
		}
		i.credentialDigest = kinds.Digest(b)
		out = append(out, i)
	}
	slices.SortFunc(out, func(a, b *Instance) int {
		if n := strings.Compare(a.owner.Namespace, b.owner.Namespace); n != 0 {
			return n
		}
		return strings.Compare(a.owner.Name, b.owner.Name)
	})
	return out, nil
}

func providerSpec(s public.IntegrationSpec) (json.RawMessage, error) {
	count := 0
	for _, p := range []bool{s.Cloudflare != nil, s.UniFi != nil, s.Webhook != nil} {
		if p {
			count++
		}
	}
	if count != 1 {
		return nil, ErrInvalid
	}
	var spec any
	switch s.Type {
	case "cloudflare":
		if s.Cloudflare == nil {
			return nil, ErrInvalid
		}
		spec = s.Cloudflare
	case "unifi":
		if s.UniFi == nil {
			return nil, ErrInvalid
		}
		spec = s.UniFi
	case "webhook":
		if s.Webhook == nil {
			return nil, ErrInvalid
		}
		spec = s.Webhook
	default:
		return nil, ErrInvalid
	}
	if s.Resync.Duration < 0 || s.Resync.Duration > 24*time.Hour || s.DeletionPolicy != "" && s.DeletionPolicy != "Retain" && s.DeletionPolicy != "Delete" {
		return nil, ErrInvalid
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return nil, ErrInvalid
	}
	return b, nil
}

// Owner identifies the captured instance, including its immutable object UID.
func (i *Instance) Owner() kinds.Owner { return i.owner }

// Definition returns a private copy of the captured public object.
func (i *Instance) Definition() *public.Integration { return i.definition.DeepCopy() }

// ProviderSpec returns the strict typed provider block without credentials.
func (i *Instance) ProviderSpec() json.RawMessage { return slices.Clone(i.providerSpec) }

// CredentialFingerprints returns keyed, non-secret change fingerprints.
func (i *Instance) CredentialFingerprints() map[string]string { return maps.Clone(i.fingerprints) }

// CredentialError means this instance must not be provisioned or mutated.
func (i *Instance) CredentialError() error {
	if i.credentialError {
		return ErrCredentials
	}
	return nil
}

// BundleID pins root's opaque serving/config capture identity.
func (i *Instance) BundleID() string { return i.bundleID }

// Guard binds one elected lease incarnation; errors prevent execution.
func (i *Instance) Guard(leaseID string) (ledger.Guard, error) {
	if i.credentialError {
		return ledger.Guard{}, ErrCredentials
	}
	g := ledger.Guard{Owner: i.owner, ConfigDigest: i.configDigest, CredentialDigest: i.credentialDigest, IntegrationResourceVersion: i.definition.ResourceVersion, LeaseID: leaseID, Term: i.term}
	if err := g.Validate(); err != nil {
		return ledger.Guard{}, err
	}
	return g, nil
}

// Secrets adapts only named delegated capabilities to the existing integration
// lifecycle. A provider cannot resolve arbitrary paths/env/Secret references.
func (i *Instance) Secrets() integration.SecretResolver { return namedSecrets{i: i} }

type namedSecrets struct{ i *Instance }

func (s namedSecrets) Resolve(ctx context.Context, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s.i.credentialError {
		return "", ErrCredentials
	}
	value, ok := s.i.secrets[name]
	if !ok {
		return "", credentials.ErrDenied
	}
	return string(value), nil
}
