package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/zalando/go-keyring"
)

// KeyringService is the service name tokens are stored under.
const KeyringService = "kudurru"

// keyringTimeout bounds every keyring call: a Secret Service that waits
// for an unlock prompt nobody answers must not hang the CLI.
const keyringTimeout = 3 * time.Second

// ErrNotFound reports a missing keyring entry.
var ErrNotFound = keyring.ErrNotFound

// Keyring is the OS credential store (macOS Keychain, Windows Credential
// Manager, the freedesktop Secret Service on Linux).
type Keyring interface {
	Get(service, user string) (string, error)
	Set(service, user, secret string) error
	Delete(service, user string) error
}

// SystemKeyring is the zalando/go-keyring backend with a timeout on every
// call.
type SystemKeyring struct{}

// Get reads a secret.
func (SystemKeyring) Get(service, user string) (string, error) {
	var out string
	err := withTimeout(func() error {
		var err error
		out, err = keyring.Get(service, user)
		return err
	})
	return out, err
}

// Set stores a secret.
func (SystemKeyring) Set(service, user, secret string) error {
	return withTimeout(func() error { return keyring.Set(service, user, secret) })
}

// Delete removes a secret.
func (SystemKeyring) Delete(service, user string) error {
	return withTimeout(func() error { return keyring.Delete(service, user) })
}

func withTimeout(fn func() error) error {
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(keyringTimeout):
		return errors.New("keyring did not answer within 3s")
	}
}

// noKeyring is used when KUDURRU_KEYRING=file.
type noKeyring struct{}

var errKeyringDisabled = errors.New("keyring disabled by KUDURRU_KEYRING=file")

func (noKeyring) Get(string, string) (string, error) { return "", errKeyringDisabled }
func (noKeyring) Set(string, string, string) error   { return errKeyringDisabled }
func (noKeyring) Delete(string, string) error        { return errKeyringDisabled }

// Credentials stores context tokens in the keyring, falling back to the
// 0600 contexts file when the keyring is unavailable.
type Credentials struct {
	Keyring Keyring
}

// NewCredentials picks the backend from KUDURRU_KEYRING.
func NewCredentials(getenv func(string) string) *Credentials {
	if getenv(EnvKeyring) == TokenStoreFile {
		return &Credentials{Keyring: noKeyring{}}
	}
	return &Credentials{Keyring: SystemKeyring{}}
}

// account is the keyring user name of a context.
func account(ctx Context) string { return ctx.Name }

// Store saves token for ctx (updating ctx.Token/TokenStore in place) and
// returns where it went. When the keyring refuses, the token is kept in
// the contexts file and the keyring error is returned as the warning.
func (c *Credentials) Store(ctx *Context, token string) (store string, warning error) {
	if token == "" {
		ctx.Token, ctx.TokenStore = "", ""
		_ = c.Keyring.Delete(KeyringService, account(*ctx))
		return "", nil
	}
	if err := c.Keyring.Set(KeyringService, account(*ctx), token); err != nil {
		ctx.Token, ctx.TokenStore = token, TokenStoreFile
		return TokenStoreFile, fmt.Errorf("OS keyring unavailable (%w); token stored in the 0600 config file", err)
	}
	ctx.Token, ctx.TokenStore = "", TokenStoreKeyring
	return TokenStoreKeyring, nil
}

// Token returns the token of ctx and where it came from (keyring, file,
// or "" when the context has none).
func (c *Credentials) Token(ctx Context) (string, string, error) {
	if ctx.Token != "" {
		return ctx.Token, TokenStoreFile, nil
	}
	if ctx.TokenStore != TokenStoreKeyring {
		return "", "", nil
	}
	tok, err := c.Keyring.Get(KeyringService, account(ctx))
	if errors.Is(err, ErrNotFound) {
		return "", "", fmt.Errorf("context %q: token missing from the OS keyring; run `kudurru login` again", ctx.Name)
	}
	if err != nil {
		return "", "", fmt.Errorf("context %q: read token from the OS keyring: %w", ctx.Name, err)
	}
	return tok, TokenStoreKeyring, nil
}

// Forget removes the token of ctx from wherever it is stored.
func (c *Credentials) Forget(ctx *Context) {
	if ctx.TokenStore == TokenStoreKeyring {
		_ = c.Keyring.Delete(KeyringService, account(*ctx))
	}
	ctx.Token, ctx.TokenStore = "", ""
}
