package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// ObjectPath is the /objects path of one object or, with an empty name, of
// a kind. Cluster-scoped kinds pass an empty namespace.
func ObjectPath(kind, namespace, name string) string {
	p := "/objects/" + Seg(kind)
	if name == "" {
		return p
	}
	if namespace != "" {
		p += "/" + Seg(namespace)
	}
	return p + "/" + Seg(name)
}

// ListOptions filter GET /objects/{kind}.
type ListOptions struct {
	// Namespace restricts namespaced kinds ("" lists every namespace).
	Namespace string
	// LabelSelector is a Kubernetes label selector ("app=web,tier!=db").
	LabelSelector string
	// PageSize is ?limit= per page (0: server default).
	PageSize int
}

// ListObjects returns every object of kind, following "continue" pages.
func (c *Client) ListObjects(ctx context.Context, kind string, opts ListOptions) ([]json.RawMessage, error) {
	q := url.Values{}
	if opts.Namespace != "" {
		q.Set("namespace", opts.Namespace)
	}
	if opts.LabelSelector != "" {
		q.Set("labelSelector", opts.LabelSelector)
	}
	return c.ListAll(ctx, ObjectPath(kind, "", ""), q, opts.PageSize)
}

// GetObject returns one object as raw JSON.
func (c *Client) GetObject(ctx context.Context, kind, namespace, name string) (json.RawMessage, error) {
	var out json.RawMessage
	return out, c.Get(ctx, ObjectPath(kind, namespace, name), nil, &out)
}

// PutObject replaces one object. A non-empty resourceVersion is sent as
// If-Match (412 on mismatch).
func (c *Client) PutObject(ctx context.Context, kind, namespace, name, resourceVersion string, obj any) (json.RawMessage, error) {
	b, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	if resourceVersion != "" {
		h.Set("If-Match", resourceVersion)
	}
	resp, err := c.Raw(ctx, Request{Method: http.MethodPut, Path: ObjectPath(kind, namespace, name), Body: bytes.NewReader(b), Header: h})
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := CheckResponse(resp); err != nil {
		return nil, err
	}
	var out json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("apiclient: decode PUT response: %w", err)
	}
	return out, nil
}

// DeleteObject deletes one object.
func (c *Client) DeleteObject(ctx context.Context, kind, namespace, name string) error {
	return c.Delete(ctx, ObjectPath(kind, namespace, name), nil)
}

// ApplyResult is one document's outcome of POST /apply.
type ApplyResult struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	// Result is created, updated, unchanged or error.
	Result string `json:"result"`
	Error  string `json:"error,omitempty"`
	// ErrorKey is the envelope key of the error (invalid, managed_by, ...).
	ErrorKey string `json:"errorKey,omitempty"`
	// Hint is the error hint (the owner for managed_by).
	Hint string `json:"hint,omitempty"`
	// ResourceVersion is the stored version after the write.
	ResourceVersion string `json:"resourceVersion,omitempty"`
}

// Message renders a failed result as "key: error (hint)".
func (r ApplyResult) Message() string {
	msg := r.Error
	if r.ErrorKey != "" && !strings.HasPrefix(msg, r.ErrorKey) {
		msg = r.ErrorKey + ": " + msg
	}
	switch {
	case r.Hint == "":
	case r.ErrorKey == "managed_by":
		msg += " (managed by " + r.Hint + ")"
	default:
		msg += " (hint: " + r.Hint + ")"
	}
	return msg
}

// Apply sends documents (JSON objects) to POST /apply as a JSON array and
// returns the per-document results in input order. The server answers
// either a bare array, {items:[...]} or {results:[...]}; all three are
// accepted.
func (c *Client) Apply(ctx context.Context, docs []json.RawMessage) ([]ApplyResult, error) {
	if docs == nil {
		docs = []json.RawMessage{}
	}
	var raw json.RawMessage
	if err := c.Post(ctx, "/apply", nil, docs, &raw); err != nil {
		return nil, err
	}
	return decodeApplyResults(raw)
}

func decodeApplyResults(raw json.RawMessage) ([]ApplyResult, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, nil
	}
	var out []ApplyResult
	if raw[0] == '[' {
		err := json.Unmarshal(raw, &out)
		return out, err
	}
	var env struct {
		Items   []ApplyResult `json:"items"`
		Results []ApplyResult `json:"results"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("apiclient: decode apply results: %w", err)
	}
	if env.Results != nil {
		return env.Results, nil
	}
	return env.Items, nil
}
