package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/robertherbaugh/kudurru/internal/apiclient"
	"github.com/robertherbaugh/kudurru/internal/cli"
	"github.com/robertherbaugh/kudurru/internal/config"
)

// settingsPath is the API path holding keys of a scope.
func settingsPath(s config.Scope) string {
	if s == config.ScopeNode {
		return "/node/config"
	}
	return "/settings"
}

// envName is the environment variable that sets a key on the host.
func envName(path string) string {
	return config.EnvPrefix + strings.ToUpper(strings.ReplaceAll(path, ".", "__"))
}

// hostOnlyError explains why a key cannot be set through the API.
func hostOnlyError(k config.Key) error {
	msg := fmt.Sprintf("%s is hostOnly: it is read only from kudurru.yaml or the environment (%s) on the host, never through the API", k.Path, envName(k.Path))
	if k.RestartRequired {
		msg += "; changing it needs a restart"
	}
	return errors.New(msg)
}

// lookupPath reads a dotted path from nested JSON objects.
func lookupPath(m map[string]any, path string) (any, bool) {
	var cur any = m
	for _, seg := range strings.Split(path, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = obj[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// setPath writes v at a dotted path, creating objects on the way.
func setPath(m map[string]any, path string, v any) {
	segs := strings.Split(path, ".")
	cur := m
	for _, seg := range segs[:len(segs)-1] {
		next, ok := cur[seg].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[seg] = next
		}
		cur = next
	}
	cur[segs[len(segs)-1]] = v
}

func valueString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return "null"
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func keyFlags(k config.Key) string {
	f := ""
	if k.HostOnly {
		f += "H"
	}
	if k.RestartRequired {
		f += "R"
	}
	if k.WriteOnly {
		f += "W"
	}
	return firstNonEmpty(f, "-")
}

// coerceSetting turns the text of key=value into the JSON value for the
// key's type.
func coerceSetting(k config.Key, s string) (any, error) {
	switch k.Type {
	case config.TypeInt:
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil, fmt.Errorf("%s wants an integer, not %q", k.Path, s)
		}
		return n, nil
	case config.TypeBool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return nil, fmt.Errorf("%s wants true or false, not %q", k.Path, s)
		}
		return b, nil
	case config.TypeDuration:
		if _, err := time.ParseDuration(s); err != nil {
			return nil, fmt.Errorf("%s wants a duration such as 10m, not %q", k.Path, s)
		}
		return s, nil
	case config.TypeStringList:
		if strings.HasPrefix(strings.TrimSpace(s), "[") {
			var out []string
			if err := json.Unmarshal([]byte(s), &out); err != nil {
				return nil, fmt.Errorf("%s: %w", k.Path, err)
			}
			return out, nil
		}
		if s == "" {
			return []string{}, nil
		}
		parts := strings.Split(s, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		return parts, nil
	case config.TypeList, config.TypeMap:
		j, err := yaml.YAMLToJSON([]byte(s))
		if err != nil {
			return nil, fmt.Errorf("%s wants JSON or YAML: %w", k.Path, err)
		}
		var v any
		if err := json.Unmarshal(j, &v); err != nil {
			return nil, fmt.Errorf("%s: %w", k.Path, err)
		}
		return v, nil
	}
	return s, nil
}

func newSettingsCmd(o *cliOpts) *cobra.Command {
	cmd := &cobra.Command{Use: "settings", Short: "Read and change runtime settings (cluster keys and this node's keys)"}
	var scope string
	get := &cobra.Command{
		Use:   "get [key...]",
		Short: "Show settings (redacted) with their scope",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := o.client()
			if err != nil {
				return err
			}
			scopes := []config.Scope{config.ScopeCluster, config.ScopeNode}
			switch scope {
			case "":
			case "cluster", "node":
				scopes = []config.Scope{config.Scope(scope)}
			default:
				return fmt.Errorf("--scope must be cluster or node")
			}
			values := map[config.Scope]map[string]any{}
			for _, s := range scopes {
				var m map[string]any
				if err := c.Get(cmd.Context(), settingsPath(s), nil, &m); err != nil {
					return fmt.Errorf("GET %s: %w", settingsPath(s), err)
				}
				// {scope, node, settings:{...}}; a bare tree is accepted too.
				if inner, ok := m["settings"].(map[string]any); ok {
					m = inner
				}
				delete(m, "took_ms")
				values[s] = m
			}
			want := map[string]bool{}
			for _, a := range args {
				if _, ok := config.Lookup(a); !ok {
					return fmt.Errorf("unknown settings key %q (see `kudurru config schema`)", a)
				}
				want[a] = true
			}
			type entry struct {
				Key   string `json:"key"`
				Value any    `json:"value"`
				Scope string `json:"scope"`
				Flags string `json:"flags"`
			}
			var entries []entry
			for _, k := range config.Schema {
				if len(want) > 0 && !want[k.Path] {
					continue
				}
				m, ok := values[k.Scope]
				if !ok {
					continue
				}
				v, ok := lookupPath(m, k.Path)
				if !ok {
					continue
				}
				entries = append(entries, entry{Key: k.Path, Value: v, Scope: string(k.Scope), Flags: keyFlags(k)})
			}
			return o.render(cmd, entries, func(bool) (*cli.Table, error) {
				t := &cli.Table{Headers: []string{"KEY", "VALUE", "SCOPE", "FLAGS"}}
				for _, e := range entries {
					t.Add(e.Key, valueString(e.Value), e.Scope, e.Flags)
				}
				return t, nil
			})
		},
	}
	get.Flags().StringVar(&scope, "scope", "", "only cluster or node keys")

	var revision string
	set := &cobra.Command{
		Use:   "set key=value... [key-]",
		Short: "Change settings (JSON merge patch); key- resets a key to its default",
		Long: `Change settings with a JSON merge patch. Cluster-scope keys go to
PATCH /api/v1/settings and apply to every node; node-scope keys go to
PATCH /api/v1/node/config on the node that serves the request. hostOnly keys
(listener addresses, dataDir, ...) can never be set through the API: put them
in kudurru.yaml or the KUDURRU_<SECTION>__<KEY> environment variable on the
host. "key-" removes the override so the default applies again.

For safe concurrent upstream edits, run kudurru settings ownership to review
the revision and pass it with --revision. Omitting --revision preserves legacy behavior:
your upstream edit can overwrite values changed since you reviewed them.`,
		Example: `  kudurru settings set cache.maxEntries=200000
  kudurru settings set blocking.refreshInterval=12h log.level=debug
  kudurru settings set upstreams.bootstrap=9.9.9.9,1.1.1.1
  kudurru settings set log.level-`,
		Args: cobra.MinimumNArgs(1),
		ValidArgsFunction: func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			var out []string
			for _, k := range config.Schema {
				if !k.HostOnly {
					out = append(out, k.Path+"=")
				}
			}
			return out, cobra.ShellCompDirectiveNoSpace | cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			patches := map[config.Scope]map[string]any{}
			for _, a := range args {
				path, val, hasVal := strings.Cut(a, "=")
				unset := false
				if !hasVal {
					if !strings.HasSuffix(a, "-") {
						return fmt.Errorf("%q is not key=value or key-", a)
					}
					path, unset = strings.TrimSuffix(a, "-"), true
				}
				k, ok := config.Lookup(path)
				if !ok {
					return fmt.Errorf("unknown settings key %q (see `kudurru config schema`)", path)
				}
				if k.HostOnly {
					return hostOnlyError(k)
				}
				var v any
				if !unset {
					var err error
					if v, err = coerceSetting(k, val); err != nil {
						return err
					}
				}
				if patches[k.Scope] == nil {
					patches[k.Scope] = map[string]any{}
				}
				setPath(patches[k.Scope], k.Path, v)
			}
			c, _, err := o.client()
			if err != nil {
				return err
			}
			for _, s := range []config.Scope{config.ScopeCluster, config.ScopeNode} {
				p, ok := patches[s]
				if !ok {
					continue
				}
				res, err := patchSettings(cmd, c, settingsPath(s), p, revision)
				if err != nil {
					if apiclient.IsKey(err, "host_only") {
						return fmt.Errorf("%w: hostOnly keys are set in kudurru.yaml or KUDURRU_* environment variables on the host", err)
					}
					return err
				}
				msg := string(s) + " settings updated"
				if len(res.Changed) > 0 {
					msg += ": " + strings.Join(res.Changed, ", ")
				}
				if res.Persisted != nil && !*res.Persisted {
					msg += " (not persisted: lost on restart)"
				}
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), msg)
				restart := res.RestartRequired
				if res.Changed == nil && restart == nil {
					// The server did not say; fall back to the schema.
					for _, a := range args {
						path, _, _ := strings.Cut(strings.TrimSuffix(a, "-"), "=")
						if k, _ := config.Lookup(path); k.RestartRequired && k.Scope == s {
							restart = append(restart, path)
						}
					}
				}
				for _, path := range restart {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "note: %s takes effect after a restart\n", path)
				}
			}
			return nil
		},
	}
	set.Flags().StringVar(&revision, "revision", "", "reviewed upstreams revision from settings ownership (If-Match)")
	cmd.AddCommand(get, set)
	addSettingsOwnershipCommands(cmd, o)
	return cmd
}

// patchResult is the PATCH /settings and /node/config answer.
type patchResult struct {
	Changed         []string `json:"changed"`
	RestartRequired []string `json:"restartRequired"`
	Persisted       *bool    `json:"persisted"`
}

func patchSettings(cmd *cobra.Command, c *apiclient.Client, path string, patch map[string]any, revision string) (patchResult, error) {
	var res patchResult
	b, err := json.Marshal(patch)
	if err != nil {
		return res, err
	}
	header := http.Header{}
	if _, upstreams := patch["upstreams"]; path == "/settings" && upstreams && revision != "" {
		header.Set("If-Match", revision)
	}
	resp, err := c.Raw(cmd.Context(), apiclient.Request{Method: http.MethodPatch, Path: path, Body: bytes.NewReader(b), ContentType: "application/merge-patch+json", Header: header})
	if err != nil {
		return res, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := apiclient.CheckResponse(resp); err != nil {
		return res, err
	}
	_ = json.NewDecoder(resp.Body).Decode(&res)
	return res, nil
}
