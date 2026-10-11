package users

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// Environment variables of the first-boot admin (§8.7).
const (
	EnvAdminPassword     = "KUDURRU_ADMIN_PASSWORD"
	EnvAdminPasswordFile = "KUDURRU_ADMIN_PASSWORD_FILE"
)

// DefaultAdminName is the first-boot admin's username.
const DefaultAdminName = "admin"

// BootstrapOptions configures Bootstrap.
type BootstrapOptions struct {
	// Getenv reads the environment; default os.Getenv.
	Getenv func(string) string
	// PasswordFile is auth.bootstrap.adminPasswordFile; the environment
	// variables take precedence.
	PasswordFile string
	// Out receives the generated password, printed once; default os.Stderr.
	Out io.Writer
	// Identity names this node in the bootstrap manager
	// ("bootstrap:<identity>"), so that of two replicas racing to create
	// the admin exactly one wins and the other sees store.ErrManagedBy.
	Identity string
	// Username defaults to DefaultAdminName.
	Username string
}

// BootstrapResult says what Bootstrap did.
type BootstrapResult struct {
	// Created is true when this call created the admin.
	Created bool
	// Generated is true when the password was generated (and printed).
	Generated bool
	// Username is the admin's name.
	Username string
	// Password is the generated password (empty otherwise); returned for
	// tests and the admin socket, never logged.
	Password string
}

// Bootstrap creates the first-boot admin when the store holds no local
// user (§8.7): the password comes from KUDURRU_ADMIN_PASSWORD,
// KUDURRU_ADMIN_PASSWORD_FILE or auth.bootstrap.adminPasswordFile, in that
// order, else a random one is generated, printed once to Out and the
// account must change it at first login (which is what GET /auth/status
// reports as hasDefaultCredentials). The admin is a builtin admin: it holds
// the admin role without a binding.
func (u *Users) Bootstrap(ctx context.Context, opts BootstrapOptions) (BootstrapResult, error) {
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	if opts.Out == nil {
		opts.Out = os.Stderr
	}
	name := opts.Username
	if name == "" {
		name = DefaultAdminName
	}
	res := BootstrapResult{Username: name}
	page, err := u.st.List(ctx, internalv1.KindLocalUser, store.ListOptions{Limit: 1})
	if err != nil {
		return res, fmt.Errorf("users: bootstrap: %w", err)
	}
	if len(page.Items) > 0 {
		return res, nil
	}
	password, source, err := bootstrapPassword(opts)
	if err != nil {
		return res, err
	}
	generated := password == ""
	if generated {
		password = GeneratePassword()
	} else if err := CheckNewPassword(password); err != nil {
		return res, fmt.Errorf("users: bootstrap admin password from %s: %w", source, err)
	}
	identity := opts.Identity
	if identity == "" {
		identity, _ = os.Hostname()
	}
	_, err = u.Create(ctx, CreateOptions{
		Name: name, DisplayName: "Administrator", Password: password,
		MustChangePassword: generated, BuiltinAdmin: true,
		Manager: store.ManagerBootstrap + ":" + identity,
	})
	if store.IsManagedBy(err) || errors.Is(err, store.ErrConflict) {
		// Another replica created it first; its password stands.
		u.log.InfoContext(ctx, "first-boot admin was created by another replica", "username", name)
		return res, nil
	}
	if err != nil {
		return res, fmt.Errorf("users: bootstrap admin: %w", err)
	}
	res.Created, res.Generated = true, generated
	if generated {
		res.Password = password
		_, _ = fmt.Fprintf(opts.Out, "\nkudurru: created the first-boot admin user %q with the generated password:\n\n    %s\n\n"+
			"It is shown only once and must be changed at the first login.\n"+
			"Set %s or %s to choose it instead; `kudurru admin reset-password` resets it.\n\n",
			name, password, EnvAdminPassword, EnvAdminPasswordFile)
		u.log.WarnContext(ctx, "created the first-boot admin with a generated password (printed once to stderr)", "username", name)
	} else {
		u.log.InfoContext(ctx, "created the first-boot admin", "username", name, "passwordFrom", source)
	}
	return res, nil
}

func bootstrapPassword(opts BootstrapOptions) (password, source string, err error) {
	if v := opts.Getenv(EnvAdminPassword); v != "" {
		return v, EnvAdminPassword, nil
	}
	read := func(path, source string) (string, string, error) {
		b, err := os.ReadFile(path) //nolint:gosec // G304: the operator names the file
		if err != nil {
			return "", "", fmt.Errorf("users: %s: %w", source, err)
		}
		return strings.TrimRight(string(b), "\r\n"), source, nil
	}
	if f := opts.Getenv(EnvAdminPasswordFile); f != "" {
		return read(f, EnvAdminPasswordFile)
	}
	if opts.PasswordFile != "" {
		return read(opts.PasswordFile, "auth.bootstrap.adminPasswordFile")
	}
	return "", "", nil
}
