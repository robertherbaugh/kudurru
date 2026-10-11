package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// Principal is the authenticated subject of GET /auth/me.
type Principal struct {
	Kind   string   `json:"kind"`
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Groups []string `json:"groups,omitempty"`
}

// Me is GET /auth/me.
type Me struct {
	Principal   Principal           `json:"principal"`
	Roles       []string            `json:"roles"`
	CSRF        string              `json:"csrf,omitempty"`
	Permissions map[string][]string `json:"permissions,omitempty"`
}

// Can reports whether the permission matrix grants verb on resource.
func (m *Me) Can(resource, verb string) bool {
	for _, v := range m.Permissions[resource] {
		if v == verb || v == "*" {
			return true
		}
	}
	return false
}

// AuthProvider is one login option of GET /auth/status.
type AuthProvider struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

// AuthStatus is GET /auth/status (unauthenticated).
type AuthStatus struct {
	LocalLogin            any            `json:"localLogin"`
	Providers             []AuthProvider `json:"providers"`
	HasDefaultCredentials bool           `json:"hasDefaultCredentials"`
}

// Token is one API token (POST|GET /auth/tokens). Secret is only present
// in the POST response.
type Token struct {
	ID          string     `json:"id"`
	Name        string     `json:"name,omitempty"`
	Description string     `json:"description,omitempty"`
	Secret      string     `json:"-"`
	ExpiresAt   *time.Time `json:"expiresAt,omitempty"`
	CreatedAt   *time.Time `json:"createdAt,omitempty"`
	LastUsedAt  *time.Time `json:"lastUsedAt,omitempty"`
}

// UnmarshalJSON accepts the secret under "token", "secret" or "value"
// (the contract names it "the kdr_ secret" without fixing the field).
func (t *Token) UnmarshalJSON(b []byte) error {
	type plain Token
	var aux struct {
		plain
		TokenField string `json:"token"`
		Secret     string `json:"secret"`
		Value      string `json:"value"`
	}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	*t = Token(aux.plain)
	for _, s := range []string{aux.TokenField, aux.Secret, aux.Value} {
		if s != "" {
			t.Secret = s
			break
		}
	}
	return nil
}

// CreateTokenRequest is the POST /auth/tokens body.
type CreateTokenRequest struct {
	// Description labels the token (the server field is "description").
	Description string `json:"description,omitempty"`
	// ExpiresIn is a Go duration string ("720h"); empty uses the server
	// default (auth.tokens.maxLifetime).
	ExpiresIn string `json:"expiresIn,omitempty"`
}

// Status calls GET /auth/status.
func (c *Client) Status(ctx context.Context) (*AuthStatus, error) {
	var out AuthStatus
	return &out, c.Get(ctx, "/auth/status", nil, &out)
}

// Me calls GET /auth/me and remembers the returned CSRF token for
// cookie-authenticated writes.
func (c *Client) Me(ctx context.Context) (*Me, error) {
	var out Me
	if err := c.Get(ctx, "/auth/me", nil, &out); err != nil {
		return nil, err
	}
	if out.CSRF != "" {
		c.SetCSRF(out.CSRF)
	}
	return &out, nil
}

// Login performs POST /auth/login with a local username and password. The
// session cookie lands in the client's cookie jar; Login then calls Me so
// the CSRF token is set for the writes that follow.
func (c *Client) Login(ctx context.Context, username, password string) (*Me, error) {
	if c.http.Jar == nil {
		return nil, errors.New("apiclient: password login needs a cookie jar")
	}
	body := map[string]string{"username": username, "password": password}
	var resp struct {
		CSRF string `json:"csrf"`
	}
	if err := c.Do(ctx, http.MethodPost, "/auth/login", nil, body, &resp); err != nil {
		return nil, err
	}
	if resp.CSRF != "" {
		c.SetCSRF(resp.CSRF)
	}
	return c.Me(ctx)
}

// Logout calls POST /auth/logout (clears the session cookie).
func (c *Client) Logout(ctx context.Context) error {
	return c.Post(ctx, "/auth/logout", nil, nil, nil)
}

// CreateToken mints an API token for the authenticated principal. The
// returned Token carries the kdr_ secret, shown only once.
func (c *Client) CreateToken(ctx context.Context, req CreateTokenRequest) (*Token, error) {
	var out Token
	if err := c.Post(ctx, "/auth/tokens", nil, req, &out); err != nil {
		return nil, err
	}
	if out.Secret == "" {
		return nil, errors.New("apiclient: POST /auth/tokens returned no token secret")
	}
	return &out, nil
}
