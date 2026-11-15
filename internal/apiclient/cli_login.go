package apiclient

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/clioidc"
)

// CLILoginVerified contains effective bearer identity, not a replacement token.
type CLILoginVerified struct {
	Me        Me        `json:"me"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// CLILoginProviders lists advertised public-client capabilities.
func (c *Client) CLILoginProviders(ctx context.Context) ([]clioidc.Provider, error) {
	var out struct {
		Items []clioidc.Provider `json:"items"`
	}
	e := c.do(ctx, http.MethodGet, "/auth/cli/providers", nil, nil, &out, true)
	return out.Items, safeAuthError(e)
}

// PrepareCLILogin correlates the source without creating credentials.
func (c *Client) PrepareCLILogin(ctx context.Context, id string, q clioidc.PrepareRequest) (clioidc.Preparation, error) {
	var out clioidc.Preparation
	e := c.authPost(ctx, "/auth/cli/"+url.PathEscape(id)+"/prepare", q, &out)
	return out, e
}

// VerifyCLILogin submits original raw bearer and token-response proof once.
func (c *Client) VerifyCLILogin(ctx context.Context, id string, q clioidc.VerifyRequest) (CLILoginVerified, error) {
	var out CLILoginVerified
	e := c.authPost(ctx, "/auth/cli/"+url.PathEscape(id)+"/verify", q, &out)
	return out, e
}
