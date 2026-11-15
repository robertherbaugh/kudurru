package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// Forwarded object operations.
const (
	ForwardedWrite  = "write"
	ForwardedDelete = "delete"
)

// ForwardedObject is one normalized canonical object mutation that an enrolled
// SQLite follower hands to its original primary. It carries no permission: the
// primary repeats normalization, reauthorizes the original raw credential and
// runs the same transaction body as its local route.
type ForwardedObject struct {
	Operation  string          `json:"operation"`
	Kind       string          `json:"kind"`
	Namespace  string          `json:"namespace,omitempty"`
	Name       string          `json:"name"`
	Manager    string          `json:"manager,omitempty"`
	CreateOnly bool            `json:"createOnly,omitempty"`
	IfMatch    string          `json:"ifMatch,omitempty"`
	Document   json.RawMessage `json:"document,omitempty"`
}

// ForwardedObjectResult is the primary's committed write result. It does not
// assert that the follower has installed or is serving that generation.
type ForwardedObjectResult struct {
	Result string          `json:"result"`
	Object json.RawMessage `json:"object,omitempty"`
}

// ObjectForward hands one mutation to the enrolled primary. handled=false means
// this node runs its own current command; handled=true never falls back, and
// an error after handling may be an unknown outcome that must not be replayed.
type ObjectForward func(context.Context, ForwardedObject) (ForwardedObjectResult, bool, error)

func forwardedWriteResult(k kindInfo, key store.Key, out ForwardedObjectResult) (writeResult, error) {
	switch out.Result {
	case ResultCreated, ResultUpdated, ResultUnchanged:
	default:
		return writeResult{}, errUnavailable("primary returned an invalid write result")
	}
	obj := k.newObject()
	d := json.NewDecoder(bytes.NewReader(out.Object))
	d.DisallowUnknownFields()
	if err := d.Decode(obj); err != nil || store.KeyOf(obj).Name != key.Name || store.KeyOf(obj).Namespace != key.Namespace {
		return writeResult{}, errUnavailable("primary returned an invalid object")
	}
	return writeResult{obj: obj, result: out.Result, key: key}, nil
}

// ForwardedObjectCommand rebuilds one forwarded mutation's exact command body.
// The returned run function must execute inside the primary's actual current
// command with an authorize that verifies the original raw credential.
func (s *Server) ForwardedObjectCommand(in ForwardedObject) (store.Key, func(context.Context, *sqlite.CommandTx, ObjectAuthorize) (ForwardedObjectResult, error), error) {
	if s == nil {
		return store.Key{}, nil, errUnavailable("object forwarding unavailable")
	}
	if err := s.readOnly(); err != nil {
		return store.Key{}, nil, err
	}
	var k kindInfo
	found := false
	for _, candidate := range kinds {
		if string(candidate.kind) == in.Kind {
			k, found = candidate, true
		}
	}
	if !found || in.Name == "" || (!k.namespaced && in.Namespace != "") || (k.namespaced && in.Namespace == "") {
		return store.Key{}, nil, errInvalid("forwarded object: unsupported kind or key")
	}
	switch in.Operation {
	case ForwardedWrite:
		var body map[string]any
		d := json.NewDecoder(bytes.NewReader(in.Document))
		d.UseNumber()
		if len(in.Document) == 0 || d.Decode(&body) != nil || d.More() {
			return store.Key{}, nil, errInvalid("forwarded object: invalid document")
		}
		req := writeRequest{kind: k, body: body, namespace: in.Namespace, name: in.Name, ifMatch: in.IfMatch, createOnly: in.CreateOnly, manager: in.Manager}
		manager, u, key, err := s.normalizeWrite(req)
		if err != nil {
			return store.Key{}, nil, err
		}
		if key.Name != in.Name || key.Namespace != in.Namespace {
			return store.Key{}, nil, errInvalid("forwarded object: document key does not match")
		}
		return key, func(ctx context.Context, tx *sqlite.CommandTx, authorize ObjectAuthorize) (ForwardedObjectResult, error) {
			var result writeResult
			if err := s.writeCommandBody(req, manager, u, key, &result)(ctx, tx, authorize); err != nil {
				return ForwardedObjectResult{}, err
			}
			if result.obj == nil {
				return ForwardedObjectResult{}, errUnavailable("current object command returned no result")
			}
			raw, err := json.Marshal(result.obj)
			return ForwardedObjectResult{Result: result.result, Object: raw}, err
		}, nil
	case ForwardedDelete:
		if len(in.Document) != 0 || in.CreateOnly || in.Manager != "" {
			return store.Key{}, nil, errInvalid("forwarded object: invalid delete")
		}
		key := keyFor(k, in.Namespace, in.Name)
		return key, func(ctx context.Context, tx *sqlite.CommandTx, authorize ObjectAuthorize) (ForwardedObjectResult, error) {
			if err := deleteCommandBody(k, key, in.IfMatch)(ctx, tx, authorize); err != nil {
				return ForwardedObjectResult{}, err
			}
			return ForwardedObjectResult{Result: "deleted"}, nil
		}, nil
	default:
		return store.Key{}, nil, errInvalid("forwarded object: unsupported operation")
	}
}

// ForwardedObjectRefusal classifies a primary-side command error. A client
// refusal (4xx other than authorization) returns its public error so that the
// follower relays it exactly; other errors stay transport/authority failures.
func ForwardedObjectRefusal(err error) (*Error, bool) {
	var e *Error
	if !errors.As(storeError(err), &e) || e.Status < 400 || e.Status >= 500 || e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden {
		return nil, false
	}
	return e, true
}
