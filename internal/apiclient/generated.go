package apiclient

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// send uses the matching generated operation for every documented API route.
// The generated builder owns the method and path; an editor supplies the CLI's
// existing query/header/body policy without substituting another operation.
// Raw also supports undocumented endpoints and other API versions, for which
// Dispatch explicitly returns unmatched. A matched refusal or construction
// error is final and can never trigger this compatibility fallback.
func (c *Client) send(ctx context.Context, hc *http.Client, prepared *http.Request) (*http.Response, error) {
	if path, ok := strings.CutPrefix(prepared.URL.EscapedPath(), c.apiPath); ok {
		// Per-call transport options (streaming and single-use auth) must never
		// mutate the shared generated client, cookie jar or caller's client.
		generated := *c.generated
		generated.Client = hc
		response, matched, err := generated.Dispatch(ctx, prepared.Method, path, prepared.Header.Get("Content-Type"), prepared.Body,
			func(_ context.Context, request *http.Request) error {
				if request.Method != prepared.Method || !sameOperationPath(request.URL, prepared.URL) || request.URL.Host != prepared.URL.Host || request.URL.Scheme != prepared.URL.Scheme {
					return errors.New("apiclient: generated operation changed the requested method or path")
				}
				request.URL.RawQuery = prepared.URL.RawQuery
				request.Header = prepared.Header.Clone()
				// Preserve exact body/replay ownership, including auth's cleared
				// GetBody and idempotency headers. Never remarshal a single-use
				// proof through a generated model or read the body a second time.
				request.Body = prepared.Body
				request.ContentLength = prepared.ContentLength
				request.GetBody = prepared.GetBody
				return nil
			})
		if matched || err != nil {
			return response, err
		}
	}
	return hc.Do(prepared)
}

func sameOperationPath(actual, expected *url.URL) bool {
	if actual.Path != expected.Path {
		return false
	}
	// Go's router unescapes each segment separately. Equal decoded paths
	// alone cannot distinguish an encoded slash inside a name from a real
	// separator. Equivalent percent encodings may differ in case, however.
	a, b := strings.Split(actual.EscapedPath(), "/"), strings.Split(expected.EscapedPath(), "/")
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, err := url.PathUnescape(a[i])
		if err != nil {
			return false
		}
		y, err := url.PathUnescape(b[i])
		if err != nil || x != y {
			return false
		}
	}
	return true
}
