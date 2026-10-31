// Package downloadpolicy applies transport restrictions to external list data.
package downloadpolicy

import (
	"errors"
	"net/http"
)

// ErrHTTPSRequired is a permanent refusal, without URL credentials or tokens.
var ErrHTTPSRequired = errors.New("download policy: HTTPS is required by blocking.httpsOnly")

// HTTPSOnly returns a private client copy whose transport checks every initial
// request and redirect. The original transport (including custom CA roots),
// proxy behavior, timeout, cookie jar and redirect callback remain intact.
// Callers must finish configuring the original client before calling this.
func HTTPSOnly(client *http.Client) *http.Client {
	clone := *client
	base := clone.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	clone.Transport = httpsTransport{base: base}
	return &clone
}

type httpsTransport struct{ base http.RoundTripper }

func (t httpsTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func (t httpsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL == nil || req.URL.Scheme != "https" {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, ErrHTTPSRequired
	}
	return t.base.RoundTrip(req)
}
