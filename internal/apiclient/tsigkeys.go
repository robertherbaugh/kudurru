package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// maxTSIGKeyRequest matches the server's 4 KiB command body bound; a larger
// body is refused here instead of being sent.
const maxTSIGKeyRequest = 4096

// maxTSIGKeyReceipt bounds a successful receipt, which holds metadata only.
const maxTSIGKeyReceipt = 64 << 10

// ErrTSIGKeyOutcomeUnknown marks a stored TSIG key command whose response was
// missing or unusable. The command may have committed; it was not retried.
var ErrTSIGKeyOutcomeUnknown = errors.New("apiclient: stored TSIG key command outcome unknown; it was not retried; inspect fresh metadata before any further action")

// TSIGKeyCommand sends one stored TSIG key command (create, replace, reseal
// or revoke) exactly once through generated dispatch. Redirects, replayable
// bodies and idempotency headers are disabled, as for the other reviewed
// single-attempt commands. body is the caller's JSON and may carry a secret:
// it is sent as is and never copied into an error.
//
// It returns the HTTP status (201 or 200 for an acknowledged publication, 202
// for a commit without bounded publication acknowledgment) and the raw
// receipt. A non-2xx answer is the server's *Error. A missing response or a
// success status outside the operation's contract returns an error wrapping
// ErrTSIGKeyOutcomeUnknown.
func (c *Client) TSIGKeyCommand(ctx context.Context, operation, keyName string, body []byte) (int, json.RawMessage, error) {
	path, success, ok := tsigKeyCommandPath(operation, keyName)
	if !ok {
		return 0, nil, errors.New("apiclient: invalid stored TSIG key operation")
	}
	if len(body) == 0 || len(body) > maxTSIGKeyRequest {
		return 0, nil, errors.New("apiclient: stored TSIG key command body exceeds 4 KiB")
	}
	response, err := c.raw(ctx, Request{Method: http.MethodPost, Path: path, Body: bytes.NewReader(body)}, true)
	if err != nil {
		return 0, nil, ErrTSIGKeyOutcomeUnknown
	}
	defer func() { _ = response.Body.Close() }()
	if err := CheckResponse(response); err != nil {
		return 0, nil, err
	}
	if response.StatusCode != success && response.StatusCode != http.StatusAccepted {
		return 0, nil, ErrTSIGKeyOutcomeUnknown
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxTSIGKeyReceipt+1))
	trimmed := bytes.TrimSpace(data)
	if err != nil || len(data) > maxTSIGKeyReceipt || len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return 0, nil, ErrTSIGKeyOutcomeUnknown
	}
	return response.StatusCode, trimmed, nil
}

// tsigKeyCommandPath returns the route and its acknowledged-publication
// status. Create posts to the collection; the others name one key.
func tsigKeyCommandPath(operation, keyName string) (string, int, bool) {
	switch operation {
	case "create":
		return "/tsigkeys", http.StatusCreated, keyName == ""
	case "replace", "reseal", "revoke":
		if keyName == "" || len(keyName) > 254 || strings.ContainsAny(keyName, "/\\\x00\r\n") {
			return "", 0, false
		}
		return "/tsigkeys/" + url.PathEscape(keyName) + "/" + operation, http.StatusOK, true
	}
	return "", 0, false
}
