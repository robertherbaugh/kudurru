package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

const maxObjectReadCursor = 8192

// Captured cursors are separate from opaque driver cursors. They carry equality
// tokens only, and never infer ordering from a revision or resourceVersion.
type objectReadCursor struct {
	Version int                    `json:"v"`
	Base    replication.Generation `json:"base"`
	Query   string                 `json:"query"`
	NS      string                 `json:"ns"`
	Name    string                 `json:"name"`
}

func objectReadUnavailable() error {
	return newError(http.StatusServiceUnavailable, KeyUnavailable, "original object generation unavailable", "")
}

func (s *Server) capturedObjects(ctx context.Context) (zonecache.ObjectSet, replication.Generation, error) {
	if ctx == nil || ctx.Err() != nil {
		return zonecache.ObjectSet{}, replication.Generation{}, objectReadUnavailable()
	}
	objects, base, err := s.opts.ObjectReads.ObjectSnapshot(ctx)
	if err != nil || base.Validate() != nil || !slices.Equal(objects.Kinds(), zonecache.DefaultKinds()) || ctx.Err() != nil {
		return zonecache.ObjectSet{}, replication.Generation{}, objectReadUnavailable()
	}
	return objects, base, nil
}

func objectReadQuery(k kindInfo, namespace string, selector map[string]string) string {
	// JSON map encoding sorts equality keys; a=b and a==b are the same query.
	b, _ := json.Marshal(struct {
		Kind      store.Kind
		Namespace string
		Selector  map[string]string
	}{k.kind, namespace, selector})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func decodeObjectReadCursor(raw string) (objectReadCursor, error) {
	var c objectReadCursor
	if len(raw) > maxObjectReadCursor {
		return c, errInvalid("object continue token is too large")
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil || !utf8.Valid(b) {
		return c, errInvalid("invalid object continue token")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil || d.Decode(new(any)) != io.EOF || c.Version != 1 || c.Base.Validate() != nil ||
		len(c.Query) != 64 || strings.Trim(c.Query, "0123456789abcdef") != "" || c.Name == "" ||
		len(c.Name) > 1024 || len(c.NS) > 1024 || !utf8.ValidString(c.Name) || !utf8.ValidString(c.NS) {
		return objectReadCursor{}, errInvalid("invalid object continue token")
	}
	return c, nil
}

func (s *Server) listCapturedObjects(ctx context.Context, k kindInfo, in *listObjectsInput, selector map[string]string) (*objectListOutput, error) {
	objects, base, err := s.capturedObjects(ctx)
	if err != nil {
		return nil, err
	}
	namespace := in.Namespace
	if !k.namespaced {
		namespace = ""
	}
	query := objectReadQuery(k, namespace, selector)
	var after objectReadCursor
	if in.Continue != "" {
		after, err = decodeObjectReadCursor(in.Continue)
		if err != nil {
			return nil, err
		}
		if !after.Base.Same(base) || after.Query != query {
			return nil, newError(http.StatusConflict, KeyConflict, "object pagination generation or query changed", "restart pagination without continue")
		}
	}
	limit := clampLimit(in.Limit, 500, 1000)
	items := make([]Object, 0, limit)
	var last store.Key
	var next string
	for _, obj := range objects.Select(k.kind, selector) {
		if namespace != "" && obj.GetNamespace() != namespace {
			continue
		}
		// Apply authorization before either page boundaries or cursor keys. In
		// particular, records use the captured spec.zoneRef, never a live one.
		if !stateFrom(ctx).clusterAllowed && !s.allowed(ctx, k.resource, "read", objectScope(k, obj)) {
			continue
		}
		key := store.KeyOf(obj)
		if in.Continue != "" && (key.Namespace < after.NS || key.Namespace == after.NS && key.Name <= after.Name) {
			continue
		}
		if len(items) == limit {
			// Only the last emitted authorized key enters the continuation.
			base.Term.PrimaryName = ""
			b, err := json.Marshal(objectReadCursor{Version: 1, Base: base, Query: query, NS: last.Namespace, Name: last.Name})
			if err != nil || base64.RawURLEncoding.EncodedLen(len(b)) > maxObjectReadCursor {
				return nil, objectReadUnavailable()
			}
			next = base64.RawURLEncoding.EncodeToString(b)
			break
		}
		m, err := objectMap(k, obj)
		if err != nil {
			return nil, err
		}
		items, last = append(items, m), key
	}
	if ctx.Err() != nil {
		return nil, objectReadUnavailable()
	}
	return &objectListOutput{Body: &ObjectList{Items: items, Continue: next}}, nil
}

func (s *Server) getCapturedObject(ctx context.Context, k kindInfo, namespace, name string) (*objectOutput, error) {
	objects, _, err := s.capturedObjects(ctx)
	if err != nil {
		return nil, err
	}
	obj, found := objects.Get(store.Key{Kind: k.kind, Namespace: namespace, Name: name})
	if !found {
		if err := s.require(ctx, k.resource, "read", s.guessScope(k, name), name); err != nil {
			return nil, err
		}
		return nil, errNotFound(k.plural + "/" + name)
	}
	if err := s.require(ctx, k.resource, "read", objectScope(k, obj), name); err != nil {
		return nil, err
	}
	m, err := objectMap(k, obj)
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, objectReadUnavailable()
	}
	return &objectOutput{ETag: obj.GetResourceVersion(), Body: m, Status: http.StatusOK}, nil
}
