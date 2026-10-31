// Package auditlog keeps audit events in AuditLog objects
// (audit.kudurru.io/v1alpha1) in the release namespace: the cluster-wide
// audit store of mode=kubernetes without a central database
// (dev-docs/design-record.md §11.4, D157).
//
// Every node ships its audit spool into immutable batches (Target, through
// audit.Shipper); every replica reads all batches (Reader), so GET /audit
// gives the same answer whichever replica serves it, and the events
// survive pod restarts. The leader deletes batches older than the
// retention (Prune). Each batch carries its events' hash chain and an
// independent Ed25519 signature; historical HMAC seals require explicit
// retained trust and cutoff admission (Verify).
package auditlog

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	auditv1 "github.com/robertherbaugh/kudurru/api/audit/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/audit"
)

// toEvent converts a stored event of a batch of node.
func toEvent(spec *auditv1.AuditLogSpec, e *auditv1.AuditLogEvent) audit.Event {
	node := e.Node
	if node == "" {
		node = spec.Node
	}
	ev := audit.Event{
		ID: e.ID, Time: e.Time.UTC(), Node: node,
		Actor:    audit.Actor{Kind: e.Actor.Kind, ID: e.Actor.ID, Name: e.Actor.Name, Groups: e.Actor.Groups, Auth: e.Actor.Auth},
		Action:   e.Action,
		Resource: audit.Resource{Kind: e.Resource.Kind, Namespace: e.Resource.Namespace, Name: e.Resource.Name},
		Result:   e.Result, Reason: e.Reason, Authz: e.Authz, Manager: e.Manager, ResourceVersion: e.ResourceVersion,
		Request:  audit.Request{ID: e.Request.ID, IP: e.Request.IP, UA: e.Request.UA, Method: e.Request.Method, Path: e.Request.Path},
		PrevHash: e.PrevHash, Hash: e.Hash,
	}
	if e.Before != "" {
		ev.Before = json.RawMessage(e.Before)
	}
	if e.After != "" {
		ev.After = json.RawMessage(e.After)
	}
	return ev
}

// fromEvent converts a spool event for a batch of node; stub drops what
// the hourly budget does not keep.
func fromEvent(node string, ev audit.Event, stub bool) auditv1.AuditLogEvent {
	e := auditv1.AuditLogEvent{
		ID: ev.ID, Time: metav1.NewMicroTime(ev.Time.UTC().Truncate(time.Microsecond)),
		Actor:    auditv1.AuditActor{Kind: ev.Actor.Kind, ID: ev.Actor.ID, Name: ev.Actor.Name, Groups: ev.Actor.Groups, Auth: ev.Actor.Auth},
		Action:   ev.Action,
		Resource: auditv1.AuditResource{Kind: ev.Resource.Kind, Namespace: ev.Resource.Namespace, Name: ev.Resource.Name},
		Result:   ev.Result, Authz: ev.Authz, Manager: ev.Manager, ResourceVersion: ev.ResourceVersion,
		PrevHash: ev.PrevHash, Hash: ev.Hash,
	}
	if ev.Node != node {
		e.Node = ev.Node
	}
	if stub {
		e.Stub = true
		return e
	}
	e.Reason = ev.Reason
	e.Before, e.After = string(ev.Before), string(ev.After)
	e.Request = auditv1.AuditRequest{ID: ev.Request.ID, IP: ev.Request.IP, UA: ev.Request.UA, Method: ev.Request.Method, Path: ev.Request.Path}
	return e
}

// SealKeyInfo is the HKDF info that derives the seal key from the cluster
// encryption key.
const SealKeyInfo = "kudurru/audit-seal/v1"

// SealKey derives historical HMAC verification material for explicit import,
// which every node shares; nil for an empty key (batches are not sealed).
func SealKey(encryptionKey []byte) ([]byte, error) {
	if len(encryptionKey) == 0 {
		return nil, nil
	}
	return hkdf.Key(sha256.New, encryptionKey, nil, SealKeyInfo, 32)
}

// seal is the HMAC-SHA256 of the batch (its spec without the seal).
func seal(key []byte, spec auditv1.AuditLogSpec) (string, error) {
	spec.Seal = ""
	b, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	m := hmac.New(sha256.New, key)
	m.Write(b)
	return hex.EncodeToString(m.Sum(nil)), nil
}

// sealOK reports whether the batch's seal verifies with one of keys.
func sealOK(keys [][]byte, spec auditv1.AuditLogSpec) bool {
	got, err := hex.DecodeString(spec.Seal)
	if err != nil {
		return false
	}
	for _, k := range keys {
		if len(k) == 0 {
			continue
		}
		want, err := seal(k, spec)
		if err != nil {
			return false
		}
		w, _ := hex.DecodeString(want)
		if hmac.Equal(got, w) {
			return true
		}
	}
	return false
}
