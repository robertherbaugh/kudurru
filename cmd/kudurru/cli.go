package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/robertherbaugh/kudurru/internal/apiclient"
	"github.com/robertherbaugh/kudurru/internal/cli"
	"github.com/robertherbaugh/kudurru/internal/version"
)

// cliOpts holds the global client flags and the injectable environment of
// the API verbs. One instance is shared by every command of a root.
type cliOpts struct {
	cliConfig string
	context   string
	server    string
	token     string
	namespace string
	output    string
	insecure  bool
	caFile    string
	timeout   time.Duration

	// getenv, stdin and now are replaced by tests.
	getenv func(string) string
	stdin  io.Reader
	now    func() time.Time
	// creds overrides the token store (tests); nil picks it from the
	// environment.
	creds *cli.Credentials
	// httpClient overrides the transport (tests).
	httpClient *http.Client
}

func newCLIOpts() *cliOpts {
	return &cliOpts{getenv: os.Getenv, stdin: os.Stdin, now: time.Now}
}

// addFlags registers the global client flags on the root.
func (o *cliOpts) addFlags(cmd *cobra.Command) {
	f := cmd.PersistentFlags()
	f.StringVar(&o.cliConfig, "cli-config", "", "CLI contexts file (default $KUDURRU_CLI_CONFIG or ~/.config/kudurru/config.yaml)")
	f.StringVar(&o.context, "context", "", "CLI context to use (default: the current context, or $KUDURRU_CONTEXT)")
	f.StringVar(&o.server, "server", "", "API server URL, e.g. https://dns.example (overrides the context; env KUDURRU_SERVER)")
	f.StringVar(&o.token, "token", "", "API token kdr_… (overrides the context; env KUDURRU_TOKEN)")
	f.StringVarP(&o.namespace, "namespace", "n", "", "namespace for namespaced kinds (default: the context's, else \"default\")")
	f.StringVarP(&o.output, "output", "o", "", "output format: table, wide, json, yaml or name")
	f.BoolVar(&o.insecure, "insecure-skip-tls-verify", false, "do not verify the server's TLS certificate (lab use only)")
	f.StringVar(&o.caFile, "ca-file", "", "PEM bundle to trust for the API server")
	f.DurationVar(&o.timeout, "request-timeout", 30*time.Second, "timeout of each API request")
	_ = cmd.RegisterFlagCompletionFunc("output", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return cli.Formats, cobra.ShellCompDirectiveNoFileComp
	})
	_ = cmd.RegisterFlagCompletionFunc("context", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		cfg, err := o.loadConfig()
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return cfg.Names(), cobra.ShellCompDirectiveNoFileComp
	})
}

func (o *cliOpts) configPath() (string, error) {
	if o.cliConfig != "" {
		return o.cliConfig, nil
	}
	return cli.DefaultConfigPath(o.getenv)
}

func (o *cliOpts) loadConfig() (*cli.Config, error) {
	p, err := o.configPath()
	if err != nil {
		return nil, err
	}
	return cli.LoadConfig(p)
}

func (o *cliOpts) credentials() *cli.Credentials {
	if o.creds != nil {
		return o.creds
	}
	o.creds = cli.NewCredentials(o.getenv)
	return o.creds
}

// resolve picks the endpoint from flags, environment and context.
func (o *cliOpts) resolve() (cli.Resolved, error) {
	cfg, err := o.loadConfig()
	if err != nil {
		return cli.Resolved{}, err
	}
	return cli.Resolve(cfg, o.credentials(), cli.Overrides{
		Context: o.context, Server: o.server, Token: o.token, Namespace: o.namespace,
		InsecureSkipVerify: o.insecure, CAFile: o.caFile,
	}, o.getenv)
}

// newClient builds an API client for a resolved endpoint.
func (o *cliOpts) newClient(r cli.Resolved) (*apiclient.Client, error) {
	return apiclient.New(r.Server, apiclient.Options{
		Token: r.Token, InsecureSkipVerify: r.InsecureSkipVerify, CAFile: r.CAFile,
		Timeout: o.timeout, UserAgent: "kudurru-cli/" + version.Get().Version, HTTPClient: o.httpClient,
	})
}

// client resolves the endpoint and builds its client.
func (o *cliOpts) client() (*apiclient.Client, cli.Resolved, error) {
	r, err := o.resolve()
	if err != nil {
		return nil, r, err
	}
	c, err := o.newClient(r)
	return c, r, err
}

// format parses -o.
func (o *cliOpts) format() (cli.Format, error) { return cli.ParseFormat(o.output) }

// nsFor returns the namespace for a namespaced kind: -n, then the
// context's namespace, then "default".
func (o *cliOpts) nsFor(r cli.Resolved) string {
	if r.Namespace != "" {
		return r.Namespace
	}
	return "default"
}

// render writes v as JSON/YAML when -o asks for it, otherwise the table
// built by table (which may be nil for structured-only output).
func (o *cliOpts) render(cmd *cobra.Command, v any, table func(wide bool) (*cli.Table, error)) error {
	f, err := o.format()
	if err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	if f.Structured() || table == nil {
		if !f.Structured() {
			f = cli.FormatYAML
		}
		return cli.WriteStructured(w, f, v)
	}
	t, err := table(f == cli.FormatWide)
	if err != nil {
		return err
	}
	if len(t.Rows) == 0 {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "No resources found.")
		return nil
	}
	return t.Write(w)
}

// rawItems decodes a list envelope or a bare array into raw items.
func rawItems(raw json.RawMessage) ([]json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if raw[0] == '[' {
		var out []json.RawMessage
		return out, json.Unmarshal(raw, &out)
	}
	var env struct {
		Items json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	if len(env.Items) == 0 || string(env.Items) == "null" {
		return nil, nil
	}
	var out []json.RawMessage
	return out, json.Unmarshal(env.Items, &out)
}

// listItems GETs a list endpoint following "continue" and decodes every
// item into T.
func listItems[T any](ctx context.Context, c *apiclient.Client, path string, q url.Values) ([]T, []json.RawMessage, error) {
	raws, err := c.ListAll(ctx, path, q, 0)
	if err != nil {
		return nil, nil, err
	}
	out := make([]T, 0, len(raws))
	for _, r := range raws {
		var v T
		if err := json.Unmarshal(r, &v); err != nil {
			return nil, nil, fmt.Errorf("decode %s: %w", path, err)
		}
		out = append(out, v)
	}
	return out, raws, nil
}

// parseSeconds accepts a Go duration ("5m", "90s") or plain seconds.
func parseSeconds(s string) (int, error) {
	if n, err := strconv.Atoi(s); err == nil && n >= 0 {
		return n, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid duration %q (use 30s, 5m, 1h or seconds)", s)
	}
	return int((d + time.Second - 1) / time.Second), nil
}

// parseSince accepts an RFC 3339 time or a duration back from now.
func parseSince(s string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return time.Time{}, fmt.Errorf("invalid time %q (use RFC 3339 or a duration such as 1h)", s)
	}
	return now.Add(-d), nil
}

// sortedKeys returns the keys of m in order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

// newAPICmd is `kudurru api <method> <path>`: a raw authenticated call.
func newAPICmd(o *cliOpts) *cobra.Command {
	var (
		data    string
		headers []string
		ctype   string
	)
	cmd := &cobra.Command{
		Use:   "api <method> <path> | api <path>",
		Short: "Send a raw request to the API (path relative to /api/v1)",
		Example: `  kudurru api /version
  kudurru api POST /cache/flush
  kudurru api PATCH /settings --data '{"cache":{"maxEntries":200000}}'
  kudurru api POST /apply --data @zones.json`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			method, path := http.MethodGet, args[0]
			if len(args) == 2 {
				method, path = strings.ToUpper(args[0]), args[1]
			}
			c, _, err := o.client()
			if err != nil {
				return err
			}
			req := apiclient.Request{Method: method, Path: path, ContentType: ctype}
			if data != "" {
				body, err := readData(data, o.stdin)
				if err != nil {
					return err
				}
				req.Body = bytes.NewReader(body)
				if req.Method == http.MethodGet && len(args) == 1 {
					req.Method = http.MethodPost
				}
			}
			req.Header = http.Header{}
			for _, h := range headers {
				k, v, ok := strings.Cut(h, ":")
				if !ok {
					return fmt.Errorf("header %q is not Name: value", h)
				}
				req.Header.Add(strings.TrimSpace(k), strings.TrimSpace(v))
			}
			req.Accept = "*/*"
			resp, err := c.Raw(cmd.Context(), req)
			if err != nil {
				return err
			}
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if resp.StatusCode >= 300 {
				w = cmd.ErrOrStderr()
			}
			if strings.Contains(resp.Header.Get("Content-Type"), "json") {
				var pretty bytes.Buffer
				if json.Indent(&pretty, body, "", "  ") == nil {
					body = append(pretty.Bytes(), '\n')
				}
			}
			_, _ = w.Write(body)
			if resp.StatusCode >= 300 {
				return fmt.Errorf("%s %s: %s", method, path, resp.Status)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&data, "data", "d", "", "request body: inline text, @file, or @- for stdin")
	cmd.Flags().StringArrayVarP(&headers, "header", "H", nil, "extra header Name: value (repeatable)")
	cmd.Flags().StringVar(&ctype, "content-type", "", "Content-Type of the body (default application/json)")
	return cmd
}

// readData resolves inline text, @file or @- (stdin).
func readData(s string, stdin io.Reader) ([]byte, error) {
	switch {
	case s == "@-":
		return io.ReadAll(stdin)
	case strings.HasPrefix(s, "@"):
		return os.ReadFile(s[1:])
	}
	return []byte(s), nil
}

// readFileOrStdin reads a path, or stdin for "-".
func readFileOrStdin(path string, stdin io.Reader) ([]byte, error) {
	if path == "" {
		return nil, errors.New("no file given (use -f file or -f -)")
	}
	if path == "-" {
		return io.ReadAll(stdin)
	}
	return os.ReadFile(path) //nolint:gosec // the user names the file
}

// newCompletionCmd generates shell completion scripts.
func newCompletionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "completion bash|zsh|fish|powershell",
		Short: "Generate a shell completion script",
		Long: `Generate a shell completion script.

  bash:        source <(kudurru completion bash)
  zsh:         kudurru completion zsh > "${fpath[1]}/_kudurru"
  fish:        kudurru completion fish > ~/.config/fish/completions/kudurru.fish
  powershell:  kudurru completion powershell | Out-String | Invoke-Expression`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		RunE: func(cmd *cobra.Command, args []string) error {
			root, w := cmd.Root(), cmd.OutOrStdout()
			switch args[0] {
			case "bash":
				return root.GenBashCompletionV2(w, true)
			case "zsh":
				return root.GenZshCompletion(w)
			case "fish":
				return root.GenFishCompletion(w, true)
			case "powershell":
				return root.GenPowerShellCompletionWithDesc(w)
			}
			return fmt.Errorf("unsupported shell %q (bash, zsh, fish, powershell)", args[0])
		},
	}
}

// completeKinds offers kind names for the first argument.
func completeKinds(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) == 0 {
		return cli.KindCompletions(), cobra.ShellCompDirectiveNoFileComp
	}
	return nil, cobra.ShellCompDirectiveNoFileComp
}
