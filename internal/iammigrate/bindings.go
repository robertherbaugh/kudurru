package iammigrate

import (
	"encoding/json"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// CredentialBinding proves only incarnation and original issuance floor, not rights.
type CredentialBinding struct {
	Owner                    internalv1.TokenOwner
	OriginalAccountUID       types.UID
	OriginalAccountCreatedAt time.Time
	TokenUID                 types.UID
}

type credentialRecord struct {
	Token       key                   `json:"token"`
	TokenUID    types.UID             `json:"tokenUID"`
	Spec        string                `json:"spec"`
	Issued      metav1.Time           `json:"issued"`
	Owner       internalv1.TokenOwner `json:"owner"`
	OriginalUID types.UID             `json:"originalUID"`
	Created     metav1.Time           `json:"created"`
}

// Bindings is an immutable private journal projection. It cannot authorize a mutation.
type Bindings struct {
	records map[types.UID]credentialRecord
}

func (b *Bindings) String() string { return "IAM credential bindings (private)" }

// GoString protects diagnostic formatting of private provenance.
func (b *Bindings) GoString() string { return b.String() }

// MarshalJSON reveals no credential verifier or UID mapping.
func (b *Bindings) MarshalJSON() ([]byte, error) { return []byte(`{"private":true}`), nil }

// ResolveToken rechecks the CURRENT token and account. Root must still verify
// hash, expiry, grants/scopes, current private authority and mutation admission.
func (b *Bindings) ResolveToken(token *internalv1.APIToken, currentAccount store.Object) (CredentialBinding, bool) {
	if b == nil || token == nil || currentAccount == nil || token.DeletionTimestamp != nil || currentAccount.GetDeletionTimestamp() != nil {
		return CredentialBinding{}, false
	}
	r, ok := b.records[token.UID]
	if !ok || r.Token.Name != token.Name || r.Token.Namespace != token.Namespace || r.Spec != digest(token.Spec) || !r.Issued.Equal(&token.CreationTimestamp) || r.Owner.UID != string(currentAccount.GetUID()) || r.Owner.Name != currentAccount.GetName() || r.Owner.Namespace != currentAccount.GetNamespace() {
		return CredentialBinding{}, false
	}
	switch account := currentAccount.(type) {
	case *publicv1.ServiceAccount:
		if r.Owner.Kind != internalv1.OwnerServiceAccount || account.Spec.Disabled {
			return CredentialBinding{}, false
		}
	case *internalv1.LocalUser:
		if r.Owner.Kind != internalv1.OwnerUser || r.Owner.Provider != "local" || account.Spec.Disabled {
			return CredentialBinding{}, false
		}
	default:
		return CredentialBinding{}, false
	}
	return CredentialBinding{Owner: r.Owner, OriginalAccountUID: r.OriginalUID, OriginalAccountCreatedAt: r.Created.Time, TokenUID: r.TokenUID}, true
}

func (p *Plan) credentials(targets map[key]types.UID) ([]credentialRecord, int, int, error) {
	accounts := map[key]*source{}
	for _, s := range p.sources {
		accounts[sourceKey(s)] = s
	}
	var result []credentialRecord
	unbound := 0
	for _, s := range p.sources {
		tok, ok := s.obj.(*internalv1.APIToken)
		if !ok {
			continue
		}
		o := tok.Spec.Owner
		var k key
		switch {
		case o.Kind == internalv1.OwnerServiceAccount:
			k = key{"ServiceAccount", o.Namespace, o.Name}
			if o.ID != "sa:"+o.Namespace+"/"+o.Name || (o.Provider != "" && o.Provider != "kudurru") {
				return nil, 0, 0, problem("invalid-owner", "APIToken", "", tok.Name, "binding")
			}
			o.Provider = "kudurru"
		case o.Kind == internalv1.OwnerUser && (o.Provider == "local" || o.ID == "local:"+o.Name):
			k = key{"LocalUser", "", o.Name}
			if o.Namespace != "" || o.ID != "local:"+o.Name || (o.Provider != "" && o.Provider != "local") {
				return nil, 0, 0, problem("invalid-owner", "APIToken", "", tok.Name, "binding")
			}
			o.Provider = "local"
		default:
			continue // external credentials are not assigned an invented UID
		}
		account := accounts[k]
		if account == nil {
			unbound++
			continue
		}
		created := account.obj.GetCreationTimestamp()
		issued := tok.CreationTimestamp
		if o.UID != "" && o.UID != string(account.obj.GetUID()) {
			return nil, 0, 0, problem("private-conflict", "APIToken", "", tok.Name, "binding")
		}
		if issued.IsZero() || created.IsZero() || issued.Before(&created) || (o.UID == "" && !issued.After(created.Time)) {
			unbound++
			continue
		}
		target := account.obj.GetUID()
		if k.Kind == "ServiceAccount" {
			target = targets[k]
			if target == "" {
				return nil, 0, 0, problem("missing-receipt", k.Kind, k.Namespace, k.Name, "binding")
			}
		}
		o.UID = string(target)
		result = append(result, credentialRecord{Token: sourceKey(s), TokenUID: tok.UID, Spec: digest(tok.Spec), Issued: issued, Owner: o, OriginalUID: account.obj.GetUID(), Created: created})
	}
	return result, len(result), unbound, nil
}

func newBindings(records []credentialRecord) *Bindings {
	b := &Bindings{records: make(map[types.UID]credentialRecord, len(records))}
	for _, r := range records {
		b.records[r.TokenUID] = r
	}
	return b
}

// Keep private JSON output deliberate even when embedding Bindings in diagnostics.
var _ json.Marshaler = (*Bindings)(nil)
