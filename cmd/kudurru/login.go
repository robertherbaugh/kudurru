package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/robertherbaugh/kudurru/internal/apiclient"
	"github.com/robertherbaugh/kudurru/internal/cli"
)

// newLoginCmd is `kudurru login`: verify credentials against a server and
// save them as a context.
func newLoginCmd(o *cliOpts) *cobra.Command {
	var (
		name          string
		username      string
		password      string
		passwordStdin bool
		tokenStdin    bool
		tokenName     string
		expiresIn     string
		namespace     string
	)
	cmd := &cobra.Command{
		Use:   "login --server https://dns.example [--token kdr_… | --username admin]",
		Short: "Log in to a kudurru API server and save it as a context",
		Long: `Log in to a kudurru API server and save the endpoint as a named context in
~/.config/kudurru/config.yaml.

With --token the API token is verified (GET /auth/me) and stored. With
--username the local password is exchanged for a session (POST /auth/login),
which then mints an API token for the CLI (POST /auth/tokens). Tokens go to
the OS keyring; when no keyring is available (headless Linux, containers) they
are kept in the config file, which is written with mode 0600. Set
KUDURRU_KEYRING=file to skip the keyring.`,
		Example: `  kudurru login --server https://dns.example --token kdr_abc123def456_…
  kudurru login --server https://dns.example --username admin
  kudurru login --server https://dns.example --token-stdin < token.txt
  echo "$PW" | kudurru login --server http://127.0.0.1:8080 --username admin --password-stdin --name local`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := o.loadConfig()
			if err != nil {
				return err
			}
			server := o.server
			if server == "" {
				server = o.getenv(cli.EnvServer)
			}
			var existing *cli.Context
			if server == "" {
				// Re-login to the selected context.
				ctxName := firstNonEmpty(o.context, name, o.getenv(cli.EnvContext), cfg.CurrentContext)
				if c, ok := cfg.Context(ctxName); ok {
					existing, server = c, c.Server
				}
			}
			if server == "" {
				return errors.New("--server is required")
			}
			if !strings.Contains(server, "://") {
				server = "https://" + server
			}
			server = strings.TrimSuffix(server, "/")
			if name == "" {
				switch {
				case o.context != "":
					name = o.context
				case existing != nil:
					name = existing.Name
				default:
					name = cli.ContextNameFor(server)
				}
			}
			ctx := cli.Context{Name: name, Server: server, Namespace: namespace, InsecureSkipVerify: o.insecure, CAFile: o.caFile}
			if prev, ok := cfg.Context(name); ok {
				if namespace == "" {
					ctx.Namespace = prev.Namespace
				}
				if !cmd.Flags().Changed("insecure-skip-tls-verify") {
					ctx.InsecureSkipVerify = prev.InsecureSkipVerify
				}
				if o.caFile == "" {
					ctx.CAFile = prev.CAFile
				}
			}
			r := cli.Resolved{Server: server, InsecureSkipVerify: ctx.InsecureSkipVerify, CAFile: ctx.CAFile}
			token := o.token
			if tokenStdin {
				if token != "" || passwordStdin {
					return errors.New("--token-stdin cannot be combined with --token or --password-stdin")
				}
				line, err := bufio.NewReader(o.stdin).ReadString('\n')
				if err != nil && !errors.Is(err, io.EOF) {
					return err
				}
				if token = strings.TrimSpace(line); token == "" {
					return errors.New("empty token on stdin")
				}
			}
			if token == "" && username == "" {
				token = o.getenv(cli.EnvToken)
			}
			errOut := cmd.ErrOrStderr()

			var me *apiclient.Me
			switch {
			case token != "" && username != "":
				return errors.New("use either --token or --username, not both")
			case token != "":
				r.Token = token
				c, err := o.newClient(r)
				if err != nil {
					return err
				}
				if me, err = c.Me(cmd.Context()); err != nil {
					return fmt.Errorf("token rejected by %s: %w", server, err)
				}
			case username != "":
				pw, err := o.readPassword(cmd, password, passwordStdin, username)
				if err != nil {
					return err
				}
				c, err := o.newClient(r)
				if err != nil {
					return err
				}
				if _, err := c.Login(cmd.Context(), username, pw); err != nil {
					return fmt.Errorf("login to %s: %w", server, err)
				}
				if tokenName == "" {
					host, _ := os.Hostname()
					tokenName = "kudurru-cli@" + firstNonEmpty(host, "unknown")
				}
				tok, err := c.CreateToken(cmd.Context(), apiclient.CreateTokenRequest{Name: tokenName, ExpiresIn: expiresIn})
				if err != nil {
					return fmt.Errorf("mint a CLI token: %w", err)
				}
				_ = c.Logout(cmd.Context()) // the session is no longer needed
				token = tok.Secret
				c.SetToken(token)
				c.SetCSRF("")
				if me, err = c.Me(cmd.Context()); err != nil {
					return fmt.Errorf("verify the new token: %w", err)
				}
			default:
				return errors.New("pass --token kdr_… or --username (and --password, --password-stdin, or a prompt)")
			}

			if prev, ok := cfg.Context(name); ok {
				o.credentials().Forget(prev)
			}
			ctx.User = me.Principal.Name
			where, warn := o.credentials().Store(&ctx, token)
			if warn != nil {
				_, _ = fmt.Fprintln(errOut, "warning:", warn)
			}
			cfg.Upsert(ctx)
			cfg.CurrentContext = name
			if err := cfg.Save(); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Logged in to %s as %s (%s); context %q saved to %s (token in %s).\n",
				server, firstNonEmpty(me.Principal.Name, me.Principal.ID), strings.Join(me.Roles, ","), name, cfg.Path(), where)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&name, "name", "", "context name (default: the server host)")
	f.StringVarP(&username, "username", "u", "", "local account to log in with")
	f.StringVarP(&password, "password", "p", "", "password (prefer the prompt or --password-stdin)")
	f.BoolVar(&passwordStdin, "password-stdin", false, "read the password from stdin")
	f.BoolVar(&tokenStdin, "token-stdin", false, "read the API token from stdin (keeps it out of the process list)")
	f.StringVar(&tokenName, "token-name", "", "name of the token minted for the CLI (default kudurru-cli@<hostname>)")
	f.StringVar(&expiresIn, "token-expires-in", "", "lifetime of the minted token, e.g. 720h (default: the server's)")
	f.StringVar(&namespace, "default-namespace", "", "default namespace stored in the context")
	return cmd
}

func (o *cliOpts) readPassword(cmd *cobra.Command, password string, fromStdin bool, user string) (string, error) {
	switch {
	case password != "":
		return password, nil
	case fromStdin:
		line, err := bufio.NewReader(o.stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		pw := strings.TrimRight(line, "\r\n")
		if pw == "" {
			return "", errors.New("empty password on stdin")
		}
		return pw, nil
	}
	f, ok := o.stdin.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) { //nolint:gosec // file descriptors fit in int
		return "", errors.New("no password: pass --password-stdin or run in a terminal")
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Password for %s: ", user)
	b, err := term.ReadPassword(int(f.Fd())) //nolint:gosec // file descriptors fit in int
	_, _ = fmt.Fprintln(cmd.ErrOrStderr())
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// newLogoutCmd forgets the token of a context (and keeps the endpoint).
func newLogoutCmd(o *cliOpts) *cobra.Command {
	var remove bool
	cmd := &cobra.Command{
		Use:   "logout [context]",
		Short: "Forget the stored token of a context",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := o.loadConfig()
			if err != nil {
				return err
			}
			name := firstNonEmpty(o.context, cfg.CurrentContext)
			if len(args) == 1 {
				name = args[0]
			}
			c, ok := cfg.Context(name)
			if !ok {
				return fmt.Errorf("no context %q", name)
			}
			o.credentials().Forget(c)
			if remove {
				cfg.Remove(name)
			}
			if err := cfg.Save(); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Logged out of context %q.\n", name)
			return nil
		},
	}
	cmd.Flags().BoolVar(&remove, "remove", false, "also delete the context")
	return cmd
}

// contextCmds are the `kudurru config` subcommands for CLI contexts.
func contextCmds(o *cliOpts) []*cobra.Command {
	completeContexts := func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		cfg, err := o.loadConfig()
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return cfg.Names(), cobra.ShellCompDirectiveNoFileComp
	}
	getContexts := &cobra.Command{
		Use:   "get-contexts",
		Short: "List the CLI contexts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := o.loadConfig()
			if err != nil {
				return err
			}
			type row struct {
				Name      string `json:"name"`
				Current   bool   `json:"current"`
				Server    string `json:"server"`
				User      string `json:"user,omitempty"`
				Namespace string `json:"namespace,omitempty"`
				Token     string `json:"tokenStore,omitempty"`
			}
			var rows []row
			for _, c := range cfg.Contexts {
				rows = append(rows, row{Name: c.Name, Current: c.Name == cfg.CurrentContext, Server: c.Server, User: c.User, Namespace: c.Namespace, Token: c.TokenStore})
			}
			return o.render(cmd, rows, func(bool) (*cli.Table, error) {
				t := &cli.Table{Headers: []string{"CURRENT", "NAME", "SERVER", "USER", "NAMESPACE", "TOKEN"}}
				for _, r := range rows {
					cur := ""
					if r.Current {
						cur = "*"
					}
					t.Add(cur, r.Name, r.Server, r.User, r.Namespace, r.Token)
				}
				return t, nil
			})
		},
	}
	useContext := &cobra.Command{
		Use:               "use-context <name>",
		Short:             "Switch the current CLI context",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeContexts,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := o.loadConfig()
			if err != nil {
				return err
			}
			if err := cfg.UseContext(args[0]); err != nil {
				return err
			}
			if err := cfg.Save(); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Switched to context %q.\n", args[0])
			return nil
		},
	}
	currentContext := &cobra.Command{
		Use:   "current-context",
		Short: "Print the current CLI context",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := o.loadConfig()
			if err != nil {
				return err
			}
			if cfg.CurrentContext == "" {
				return errors.New("no current context (run `kudurru login`)")
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), cfg.CurrentContext)
			return err
		},
	}
	deleteContext := &cobra.Command{
		Use:               "delete-context <name>",
		Short:             "Delete a CLI context and its stored token",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeContexts,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := o.loadConfig()
			if err != nil {
				return err
			}
			c, ok := cfg.Context(args[0])
			if !ok {
				return fmt.Errorf("no context %q", args[0])
			}
			o.credentials().Forget(c)
			cfg.Remove(args[0])
			if err := cfg.Save(); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Deleted context %q.\n", args[0])
			return nil
		},
	}
	setContext := &cobra.Command{
		Use:   "set-context <name> [--server URL] [--default-namespace ns]",
		Short: "Create or change a CLI context without logging in",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := o.loadConfig()
			if err != nil {
				return err
			}
			c := cli.Context{Name: args[0]}
			if prev, ok := cfg.Context(args[0]); ok {
				c = *prev
			}
			if o.server != "" {
				c.Server = strings.TrimSuffix(o.server, "/")
			}
			if ns, _ := cmd.Flags().GetString("default-namespace"); ns != "" {
				c.Namespace = ns
			}
			if cmd.Flags().Changed("insecure-skip-tls-verify") {
				c.InsecureSkipVerify = o.insecure
			}
			if o.caFile != "" {
				c.CAFile = o.caFile
			}
			if o.token != "" {
				if _, warn := o.credentials().Store(&c, o.token); warn != nil {
					_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "warning:", warn)
				}
			}
			if c.Server == "" {
				return errors.New("--server is required for a new context")
			}
			cfg.Upsert(c)
			if cfg.CurrentContext == "" {
				cfg.CurrentContext = c.Name
			}
			if err := cfg.Save(); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Context %q saved.\n", c.Name)
			return nil
		},
	}
	setContext.Flags().String("default-namespace", "", "default namespace of the context")
	return []*cobra.Command{getContexts, useContext, currentContext, deleteContext, setContext}
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
