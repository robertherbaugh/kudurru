package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
)

// IntegrationCall sends one bounded administration request through generated
// dispatch. Redirects and mutating transport replay are disabled. An
// unacknowledged effect is never retried.
func (c *Client) IntegrationCall(ctx context.Context, method, path string, query url.Values, in any) (json.RawMessage, error) {
	req := Request{Method: method, Path: path, Query: query}
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil || len(data) > 64<<10 {
			return nil, errors.New("apiclient: integration request exceeds its bound")
		}
		req.Body = bytes.NewReader(data)
	}
	response, err := c.raw(ctx, req, true)
	if err != nil {
		return nil, errors.New("apiclient: integration response unavailable; any effects are unacknowledged and were not retried")
	}
	defer func() { _ = response.Body.Close() }()
	if err := CheckResponse(response); err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("apiclient: invalid integration acknowledgment")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 || !json.Valid(data) || len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '{' {
		return nil, errors.New("apiclient: invalid integration acknowledgment; effects may have occurred")
	}
	return data, nil
}
