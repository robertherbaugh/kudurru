// Package adminsock serves the break-glass admin socket of
// dev-docs/design-record.md §8.7 (/run/kudurru/kudurru.sock by default) that
// `kudurru admin` uses from `docker exec` and `kubectl exec`.
//
// The protocol is HTTP/1.1 with JSON bodies over the unix socket, so the
// CLI needs nothing but net/http with a unix dialer:
//
//	POST /admin/v1/reset-password  {"username":"admin","password":"","create":false}
//	  → 200 {"username":"admin","password":"<generated, only when none was sent>","mustChangePassword":true}
//	POST /admin/v1/create-token    {"user":"","description":"","expiresIn":"24h"}
//	  → 201 {"id":"…","token":"kdr_…","owner":{…},"expiresAt":"RFC 3339 or null"}
//	GET  /admin/v1/list-users
//	  → 200 {"users":[{"name","displayName","builtinAdmin","disabled","mustChangePassword","groups","lastLoginAt","passwordChangedAt"}]}
//
// Errors use the API contract body {"error":{"key","message","hint"},"took_ms"}.
// Only peers admitted by auth.PeerPolicy (uid 0, the server's own uid, or
// the configured gid; SO_PEERCRED) get past the first byte of a request;
// they act as the local root (Decision.Path "local"). Requests outside
// /admin/v1/ go to Options.API when set, so the same socket can serve the
// full REST API to the local root.
package adminsock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// DefaultPath is the socket path of §6.2.
const DefaultPath = "/run/kudurru/kudurru.sock"

// BreakGlassAccount is the builtin-admin service account that owns tokens
// created without a user.
const BreakGlassAccount = "break-glass"

// DefaultTokenLifetime is the lifetime of a break-glass token when the
// request names none (auth.tokens.maxLifetime caps it further).
const DefaultTokenLifetime = 24 * time.Hour

// Options configures a Server.
type Options struct {
	// Policy admits peers. GID < 0 disables the group rule.
	Policy auth.PeerPolicy
	// Users and Tokens are the identity stores. Required.
	Users  *users.Users
	Tokens *tokens.Tokens
	// MaxTokenLifetime is auth.tokens.maxLifetime (0: unlimited).
	MaxTokenLifetime time.Duration
	// API, when set, serves every other path to the local root.
	API http.Handler
	// Logger; default discards.
	Logger *slog.Logger
}

// Server is the admin socket server.
type Server struct {
	opts Options
	mux  *http.ServeMux
}

// New validates opts.
func New(opts Options) (*Server, error) {
	if opts.Users == nil || opts.Tokens == nil {
		return nil, errors.New("adminsock: Users and Tokens are required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	s := &Server{opts: opts, mux: http.NewServeMux()}
	s.mux.HandleFunc("POST /admin/v1/reset-password", s.resetPassword)
	s.mux.HandleFunc("POST /admin/v1/create-token", s.createToken)
	s.mux.HandleFunc("GET /admin/v1/list-users", s.listUsers)
	if opts.API != nil {
		s.mux.Handle("/", opts.API)
	}
	return s, nil
}

// Handler returns the socket's handler: the unix-peer authenticator, then
// the admin routes (and Options.API).
func (s *Server) Handler() http.Handler {
	steps := auth.Options{Steps: []auth.Authenticator{auth.UnixPeer{Policy: s.opts.Policy}}}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.PrincipalFrom(r.Context())
		if !ok || p.Kind != auth.KindLocalRoot {
			auth.WriteError(w, r, &auth.Error{Status: http.StatusForbidden, Key: "forbidden", Message: "the admin socket needs peer credentials (Linux SO_PEERCRED)"})
			return
		}
		ctx := store.WithActor(r.Context(), string(p.Kind)+":"+p.Name)
		ctx = audit.WithRequest(ctx, audit.Actor{Kind: string(p.Kind), ID: p.ID, Name: p.Name, Auth: p.AuthMethod},
			audit.Request{Method: r.Method, Path: r.URL.Path, UA: r.UserAgent()}, func() string { return "local" })
		s.mux.ServeHTTP(w, r.WithContext(ctx))
	})
	return auth.Middleware(steps)(inner)
}

// ErrBusy is returned by Listen when another process serves the socket
// (a second kudurru on the same host with the default auth.adminSocket.path).
var ErrBusy = errors.New("adminsock: socket is in use by another process")

// Listen creates the socket at path: the directory is created 0750, a
// stale socket left by a crashed process is removed (a live one is an
// error), and the socket is chmod 0660 and, when gid >= 0, chgrp gid.
func Listen(path string, gid int) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("adminsock: %w", err)
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("adminsock: %s exists and is not a socket", path)
		}
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			_ = c.Close()
			return nil, fmt.Errorf("%w: %s", ErrBusy, path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("adminsock: remove stale socket: %w", err)
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("adminsock: %w", err)
	}
	if err := os.Chmod(path, 0o660); err != nil { //nolint:gosec // G302: group access is the configured-gid rule of §8.7
		_ = ln.Close()
		return nil, fmt.Errorf("adminsock: %w", err)
	}
	if gid >= 0 {
		if err := os.Chown(path, -1, gid); err != nil {
			_ = ln.Close()
			return nil, fmt.Errorf("adminsock: chgrp %d: %w", gid, err)
		}
	}
	return ln, nil
}

// Serve serves ln until ctx is done, then shuts down gracefully.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: s.Handler(), ConnContext: auth.PeerConnContext, ReadHeaderTimeout: 5 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// ListenAndServe is Listen then Serve; the socket file is removed on exit.
func (s *Server) ListenAndServe(ctx context.Context, path string) error {
	ln, err := Listen(path, s.opts.Policy.GID)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(path) }()
	return s.Serve(ctx, ln)
}

// ResetPasswordRequest is the body of POST /admin/v1/reset-password.
type ResetPasswordRequest struct {
	// Username defaults to "admin".
	Username string `json:"username,omitempty"`
	// Password is the new password; empty generates one.
	Password string `json:"password,omitempty"`
	// Create makes a missing user (as a builtin admin) instead of 404.
	Create bool `json:"create,omitempty"`
	// RevokeTokens also revokes every API token of the user.
	RevokeTokens bool `json:"revokeTokens,omitempty"`
}

// ResetPasswordResponse is its answer.
type ResetPasswordResponse struct {
	Username           string `json:"username"`
	Password           string `json:"password,omitempty"`
	MustChangePassword bool   `json:"mustChangePassword"`
	Created            bool   `json:"created,omitempty"`
	RevokedTokens      int    `json:"revokedTokens,omitempty"`
}

func (s *Server) resetPassword(w http.ResponseWriter, r *http.Request) {
	var req ResetPasswordRequest
	if !decode(w, r, &req) {
		return
	}
	name := users.NormalizeUsername(req.Username)
	if name == "" {
		name = users.DefaultAdminName
	}
	pw, generated := req.Password, req.Password == ""
	if generated {
		pw = users.GeneratePassword()
	}
	if err := users.CheckNewPassword(pw); err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	resp := ResetPasswordResponse{Username: name, MustChangePassword: true}
	if generated {
		resp.Password = pw
	}
	err := s.opts.Users.SetPassword(r.Context(), name, pw, true)
	if errors.Is(err, users.ErrNotFound) && req.Create {
		_, err = s.opts.Users.Create(r.Context(), users.CreateOptions{Name: name, Password: pw, MustChangePassword: true, BuiltinAdmin: true})
		resp.Created = err == nil
	}
	switch {
	case errors.Is(err, users.ErrNotFound):
		writeErr(w, r, http.StatusNotFound, "not_found", fmt.Sprintf("no local user %q", name))
		return
	case err != nil:
		s.opts.Logger.ErrorContext(r.Context(), "admin socket: reset-password failed", "username", name, "err", err)
		writeErr(w, r, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if req.RevokeTokens {
		n, err := s.opts.Tokens.RevokeOwner(r.Context(), users.PrincipalID(name))
		resp.RevokedTokens = n
		if err != nil {
			s.opts.Logger.ErrorContext(r.Context(), "admin socket: revoking tokens after a password reset failed", "username", name, "err", err)
			writeErr(w, r, http.StatusInternalServerError, "internal", fmt.Sprintf("the password was reset, but only %d API tokens were revoked: %v", n, err))
			return
		}
	}
	s.opts.Logger.WarnContext(r.Context(), "admin socket: password reset", "username", name, "peer", peer(r), "revokedTokens", resp.RevokedTokens)
	writeJSON(w, http.StatusOK, resp)
}

// CreateTokenRequest is the body of POST /admin/v1/create-token.
type CreateTokenRequest struct {
	// User makes a personal token for this local user; empty makes a token
	// of the builtin-admin service account <system ns>/break-glass.
	User        string `json:"user,omitempty"`
	Description string `json:"description,omitempty"`
	// ExpiresIn is a Go duration; empty is 24h for break-glass tokens and
	// auth.tokens.maxLifetime (or never) for personal ones.
	ExpiresIn string `json:"expiresIn,omitempty"`
}

// CreateTokenResponse is its answer; Token is shown only here.
type CreateTokenResponse struct {
	ID        string                `json:"id"`
	Token     string                `json:"token"`
	Owner     internalv1.TokenOwner `json:"owner"`
	ExpiresAt *time.Time            `json:"expiresAt"`
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	var req CreateTokenRequest
	if !decode(w, r, &req) {
		return
	}
	var life time.Duration
	if req.ExpiresIn != "" {
		d, err := time.ParseDuration(req.ExpiresIn)
		if err != nil || d < 0 {
			writeErr(w, r, http.StatusBadRequest, "invalid", "expiresIn: not a positive Go duration")
			return
		}
		life = d
	}
	opts := tokens.CreateOptions{Description: req.Description, ExpiresIn: life}
	ctx := r.Context()
	if req.User != "" {
		usr, ok := s.opts.Users.Get(req.User)
		if !ok {
			writeErr(w, r, http.StatusNotFound, "not_found", fmt.Sprintf("no local user %q", req.User))
			return
		}
		opts.Owner = internalv1.TokenOwner{Kind: internalv1.OwnerUser, Name: usr.Name, ID: users.PrincipalID(usr.Name), Provider: users.Provider}
	} else {
		ns := s.opts.Tokens.Namespace()
		if _, err := s.opts.Tokens.EnsureServiceAccount(ctx, ns, BreakGlassAccount, "Break-glass tokens from `kudurru admin create-token`", true); err != nil {
			writeErr(w, r, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		opts.Owner = internalv1.TokenOwner{Kind: internalv1.OwnerServiceAccount, Name: BreakGlassAccount, Namespace: ns}
		if opts.ExpiresIn == 0 {
			opts.ExpiresIn = DefaultTokenLifetime
			if m := s.opts.MaxTokenLifetime; m > 0 && m < opts.ExpiresIn {
				opts.ExpiresIn = m
			}
		}
		if opts.Description == "" {
			opts.Description = "break-glass token (kudurru admin create-token)"
		}
	}
	wire, tok, err := s.opts.Tokens.Create(ctx, opts)
	switch {
	case errors.Is(err, tokens.ErrLifetimeTooLong):
		writeErr(w, r, http.StatusBadRequest, "invalid", err.Error())
		return
	case err != nil:
		writeErr(w, r, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.opts.Logger.WarnContext(ctx, "admin socket: token created", "token", tok.Name, "owner", tok.Spec.Owner.ID, "peer", peer(r))
	resp := CreateTokenResponse{ID: tok.Name, Token: wire, Owner: tok.Spec.Owner}
	if tok.Spec.ExpiresAt != nil {
		t := tok.Spec.ExpiresAt.UTC()
		resp.ExpiresAt = &t
	}
	writeJSON(w, http.StatusCreated, resp)
}

// User is one entry of list-users.
type User struct {
	Name               string     `json:"name"`
	DisplayName        string     `json:"displayName,omitempty"`
	BuiltinAdmin       bool       `json:"builtinAdmin"`
	Disabled           bool       `json:"disabled"`
	MustChangePassword bool       `json:"mustChangePassword"`
	Groups             []string   `json:"groups,omitempty"`
	LastLoginAt        *time.Time `json:"lastLoginAt,omitempty"`
	PasswordChangedAt  *time.Time `json:"passwordChangedAt,omitempty"`
}

// ListUsersResponse is the answer of GET /admin/v1/list-users.
type ListUsersResponse struct {
	Users []User `json:"users"`
}

func (s *Server) listUsers(w http.ResponseWriter, _ *http.Request) {
	resp := ListUsersResponse{Users: []User{}}
	for _, u := range s.opts.Users.List() {
		resp.Users = append(resp.Users, UserOf(u))
	}
	writeJSON(w, http.StatusOK, resp)
}

// UserOf renders a LocalUser without its password hash.
func UserOf(u *internalv1.LocalUser) User {
	out := User{Name: u.Name, DisplayName: u.Spec.DisplayName, BuiltinAdmin: u.Spec.BuiltinAdmin, Disabled: u.Spec.Disabled,
		MustChangePassword: u.Spec.MustChangePassword, Groups: u.Spec.Groups}
	if t := u.Status.LastLoginAt; t != nil {
		v := t.UTC()
		out.LastLoginAt = &v
	}
	if t := u.Status.PasswordChangedAt; t != nil {
		v := t.UTC()
		out.PasswordChangedAt = &v
	}
	return out
}

func peer(r *http.Request) string {
	if c, ok := auth.PeerFrom(r.Context()); ok {
		return fmt.Sprintf("uid=%d gid=%d pid=%d", c.UID, c.GID, c.PID)
	}
	return ""
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid", "unreadable body")
		return false
	}
	if len(body) == 0 {
		return true
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, r, http.StatusBadRequest, "invalid", "body: "+err.Error())
		return false
	}
	return true
}

func writeErr(w http.ResponseWriter, r *http.Request, status int, key, msg string) {
	auth.WriteError(w, r, &auth.Error{Status: status, Key: key, Message: msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
