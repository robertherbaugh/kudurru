// Package cli holds the client-side pieces of the kudurru CLI that are not
// cobra wiring: the contexts file (~/.config/kudurru/config.yaml), token
// storage in the OS keyring with a 0600 file fallback, kind aliases and
// kubectl-style tables, manifest reading, and the admin unix-socket client.
package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// Environment variables the CLI reads.
const (
	EnvServer    = "KUDURRU_SERVER"
	EnvToken     = "KUDURRU_TOKEN"
	EnvContext   = "KUDURRU_CONTEXT"
	EnvCLIConfig = "KUDURRU_CLI_CONFIG"
	// EnvKeyring selects token storage: "system" (default: the OS keyring,
	// falling back to the file) or "file" (never touch the keyring).
	EnvKeyring = "KUDURRU_KEYRING"
)

// Token storage locations recorded in a context.
const (
	TokenStoreKeyring = "keyring"
	TokenStoreFile    = "file"
)

// ConfigKind is the kind of the contexts file.
const ConfigKind = "Config"

// Context is one named API endpoint with its credentials.
type Context struct {
	Name   string `json:"name"`
	Server string `json:"server"`
	// Token is the API token when it is stored in this file (TokenStore
	// "file"); empty when it lives in the OS keyring.
	Token string `json:"token,omitempty"`
	// TokenStore records where the token lives: keyring or file.
	TokenStore string `json:"tokenStore,omitempty"`
	// Namespace is the default namespace for namespaced kinds.
	Namespace          string `json:"namespace,omitempty"`
	InsecureSkipVerify bool   `json:"insecureSkipVerify,omitempty"`
	CAFile             string `json:"caFile,omitempty"`
	// User is informational: the principal name seen at login.
	User string `json:"user,omitempty"`
}

// Config is the contexts file.
type Config struct {
	APIVersion     string    `json:"apiVersion,omitempty"`
	Kind           string    `json:"kind,omitempty"`
	CurrentContext string    `json:"currentContext,omitempty"`
	Contexts       []Context `json:"contexts"`

	// path is where the file was loaded from and is saved to.
	path string
}

// DefaultConfigPath returns $KUDURRU_CLI_CONFIG, else
// $XDG_CONFIG_HOME/kudurru/config.yaml (~/.config/kudurru/config.yaml).
func DefaultConfigPath(getenv func(string) string) (string, error) {
	if p := getenv(EnvCLIConfig); p != "" {
		return p, nil
	}
	dir := getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home := getenv("HOME")
		if home == "" {
			h, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("cli: cannot locate the config directory: %w", err)
			}
			home = h
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "kudurru", "config.yaml"), nil
}

// LoadConfig reads the contexts file at path. A missing file yields an
// empty config that Save will create.
func LoadConfig(path string) (*Config, error) {
	cfg := &Config{path: path}
	b, err := os.ReadFile(path) //nolint:gosec // the user names their own config file
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cli: read %s: %w", path, err)
	}
	if err := yaml.UnmarshalStrict(b, cfg); err != nil {
		return nil, fmt.Errorf("cli: parse %s: %w", path, err)
	}
	cfg.path = path
	seen := map[string]bool{}
	for _, c := range cfg.Contexts {
		if c.Name == "" {
			return nil, fmt.Errorf("cli: %s: a context has no name", path)
		}
		if seen[c.Name] {
			return nil, fmt.Errorf("cli: %s: context %q is defined twice", path, c.Name)
		}
		seen[c.Name] = true
	}
	return cfg, nil
}

// Path is the file the config is saved to.
func (c *Config) Path() string { return c.path }

// Save writes the file atomically with mode 0600 (it may hold tokens) in a
// 0700 directory.
func (c *Config) Save() error {
	if c.path == "" {
		return errors.New("cli: config has no path")
	}
	c.APIVersion, c.Kind = "kudurru.io/v1alpha1", ConfigKind
	sort.SliceStable(c.Contexts, func(i, j int) bool { return c.Contexts[i].Name < c.Contexts[j].Name })
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("cli: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.yaml")
	if err != nil {
		return fmt.Errorf("cli: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cli: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cli: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cli: %w", err)
	}
	if err := os.Rename(tmp.Name(), c.path); err != nil {
		return fmt.Errorf("cli: %w", err)
	}
	return nil
}

// Context returns the named context.
func (c *Config) Context(name string) (*Context, bool) {
	for i := range c.Contexts {
		if c.Contexts[i].Name == name {
			return &c.Contexts[i], true
		}
	}
	return nil, false
}

// Names returns the context names, sorted.
func (c *Config) Names() []string {
	out := make([]string, 0, len(c.Contexts))
	for _, x := range c.Contexts {
		out = append(out, x.Name)
	}
	sort.Strings(out)
	return out
}

// Upsert adds ctx or replaces the context of the same name.
func (c *Config) Upsert(ctx Context) {
	if cur, ok := c.Context(ctx.Name); ok {
		*cur = ctx
		return
	}
	c.Contexts = append(c.Contexts, ctx)
}

// Remove deletes the named context; it reports whether one existed. The
// current context is cleared when it was the one removed.
func (c *Config) Remove(name string) bool {
	i := slices.IndexFunc(c.Contexts, func(x Context) bool { return x.Name == name })
	if i < 0 {
		return false
	}
	c.Contexts = slices.Delete(c.Contexts, i, i+1)
	if c.CurrentContext == name {
		c.CurrentContext = ""
	}
	return true
}

// UseContext makes name the current context.
func (c *Config) UseContext(name string) error {
	if _, ok := c.Context(name); !ok {
		return fmt.Errorf("cli: no context %q (have: %s)", name, strings.Join(c.Names(), ", "))
	}
	c.CurrentContext = name
	return nil
}

// ContextNameFor derives a context name from a server URL: its host
// without the port's default, e.g. https://dns.example → dns.example.
func ContextNameFor(server string) string {
	s := server
	if _, rest, ok := strings.Cut(s, "://"); ok {
		s = rest
	}
	s, _, _ = strings.Cut(s, "/")
	if s == "" {
		return "default"
	}
	return s
}

// Overrides are the per-invocation flags that take precedence over the
// environment and the contexts file.
type Overrides struct {
	Context            string
	Server             string
	Token              string
	Namespace          string
	InsecureSkipVerify bool
	CAFile             string
}

// Resolved is the endpoint a command talks to.
type Resolved struct {
	// ContextName is the context used ("" when none applied).
	ContextName        string
	Server             string
	Token              string
	Namespace          string
	InsecureSkipVerify bool
	CAFile             string
	// TokenSource says where the token came from: flag, env, keyring,
	// file or "" (none).
	TokenSource string
}

// Resolve applies, in order of precedence, the flags, the environment
// (KUDURRU_SERVER, KUDURRU_TOKEN, KUDURRU_CONTEXT) and the selected
// context. A context's stored token is only used when the server is the
// context's own server, so --server pointing elsewhere never leaks it.
func Resolve(cfg *Config, creds *Credentials, o Overrides, getenv func(string) string) (Resolved, error) {
	var r Resolved
	name := o.Context
	explicit := name != ""
	if name == "" {
		name = getenv(EnvContext)
		explicit = name != ""
	}
	if name == "" {
		name = cfg.CurrentContext
	}
	var ctx *Context
	if name != "" {
		c, ok := cfg.Context(name)
		switch {
		case ok:
			ctx = c
		case explicit:
			return r, fmt.Errorf("cli: no context %q in %s (have: %s)", name, cfg.Path(), strings.Join(cfg.Names(), ", "))
		}
	}
	if ctx != nil {
		r.ContextName = ctx.Name
		r.Server, r.Namespace = ctx.Server, ctx.Namespace
		r.InsecureSkipVerify, r.CAFile = ctx.InsecureSkipVerify, ctx.CAFile
	}
	sameServer := true
	switch {
	case o.Server != "":
		sameServer = ctx != nil && sameURL(o.Server, ctx.Server)
		r.Server = o.Server
	case getenv(EnvServer) != "":
		sameServer = ctx != nil && sameURL(getenv(EnvServer), ctx.Server)
		r.Server = getenv(EnvServer)
	}
	if !sameServer {
		// A different server than the context's: its TLS settings do not
		// apply either.
		r.InsecureSkipVerify, r.CAFile, r.ContextName = false, "", ""
	}
	switch {
	case o.Token != "":
		r.Token, r.TokenSource = o.Token, "flag"
	case getenv(EnvToken) != "":
		r.Token, r.TokenSource = getenv(EnvToken), "env"
	case ctx != nil && sameServer:
		tok, src, err := creds.Token(*ctx)
		if err != nil {
			return r, err
		}
		r.Token, r.TokenSource = tok, src
	}
	if o.Namespace != "" {
		r.Namespace = o.Namespace
	}
	if o.InsecureSkipVerify {
		r.InsecureSkipVerify = true
	}
	if o.CAFile != "" {
		r.CAFile = o.CAFile
	}
	if r.Server == "" {
		return r, errors.New("no server configured: run `kudurru login --server https://…`, pass --server, or set KUDURRU_SERVER")
	}
	return r, nil
}

func sameURL(a, b string) bool {
	norm := func(s string) string {
		s = strings.TrimSuffix(strings.TrimSpace(s), "/")
		if !strings.Contains(s, "://") {
			s = "https://" + s
		}
		return strings.ToLower(s)
	}
	return norm(a) == norm(b)
}
