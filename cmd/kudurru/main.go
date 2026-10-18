// Command kudurru is the single binary: server (`kudurru serve`), CLI and,
// later, the dnsdist/keepalived supervisors (`kudurru edge run`,
// `kudurru vip run`). Seven letters.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/robertherbaugh/kudurru/internal/apiclient"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/version"
)

// defaultConfigPath is a var so tests can point it at a missing file.
var defaultConfigPath = "/etc/kudurru/kudurru.yaml"

func main() {
	if err := newRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		if hint := errorHint(err); hint != "" {
			fmt.Fprintln(os.Stderr, "hint:", hint)
		}
		os.Exit(1)
	}
}

// errorHint suggests the next step for common API failures.
func errorHint(err error) string {
	switch {
	case apiclient.IsStatus(err, http.StatusUnauthorized):
		return "the server rejected the credentials; run `kudurru login` (or check KUDURRU_TOKEN / --token)"
	case apiclient.IsStatus(err, http.StatusForbidden):
		return "the token lacks the permission named above; ask an admin for a RoleBinding"
	case apiclient.IsKey(err, "read_only"):
		return "this deployment is read-only through the API (ha-static); edit objects.d on every node"
	}
	return ""
}

func newRoot() *cobra.Command {
	return newRootWith(newCLIOpts())
}

// newRootWith builds the command tree around o, the client options the
// API verbs share (tests inject the environment through it).
func newRootWith(o *cliOpts) *cobra.Command {
	var configPath string
	root := &cobra.Command{
		Use:           "kudurru",
		Short:         "kudurru (koo-DOO-roo): a Kubernetes-native DNS server that also runs as one container",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&configPath, "config", envOr("KUDURRU_CONFIG", defaultConfigPath), "path to the server's kudurru.yaml")
	o.addFlags(root)
	root.AddGroup(
		&cobra.Group{ID: "server", Title: "Server:"},
		&cobra.Group{ID: "objects", Title: "Objects:"},
		&cobra.Group{ID: "dns", Title: "DNS and lists:"},
		&cobra.Group{ID: "observe", Title: "Observe:"},
		&cobra.Group{ID: "admin", Title: "Setup and administration:"},
	)
	add := func(group string, cmds ...*cobra.Command) {
		for _, c := range cmds {
			c.GroupID = group
			root.AddCommand(c)
		}
	}
	add("server", newServeCmd(&configPath), newHealthCmd(), newLameduckCmd(), newDoctorCmd(&configPath, o), newVersionCmd(), newEdgeCmd(), newVIPCmd())
	add("objects", newGetCmd(o), newApplyCmd(o), newDeleteCmd(o), newDescribeCmd(o), newExportCmd(o), newBackupCmd(o), newRestoreCmd(o), newImportCmd(o))
	add("dns", newZonesCmd(o), newRecordsCmd(o), newListsCmd(o), newBlockingCmd(o), newCacheCmd(o), newQueryCmd(o))
	add("observe", newStatsCmd(o), newLogsCmd(o), newNodesCmd(o), newAuditCmd(o))
	add("admin", newLoginCmd(o), newLogoutCmd(o), newConfigCmd(&configPath, o), newSettingsCmd(o), newAdminCmd(o), newTokenCmd(), newLicenseCmd(&configPath), newAPICmd(o), newCompletionCmd())
	root.SetHelpCommandGroupID("admin")
	root.SetCompletionCommandGroupID("admin")
	return root
}

// loadConfig reads the config file. When the path is the default and the
// file does not exist, built-in defaults are used so `docker run` with no
// mounted file still starts.
func loadConfig(cmd *cobra.Command, path string) (*config.Config, error) {
	return loadConfigWithSections(cmd, path, nil)
}

func loadConfigWithSections(cmd *cobra.Command, path string, owned config.SectionLoader) (*config.Config, error) {
	explicit := cmd.Flags().Changed("config") || os.Getenv("KUDURRU_CONFIG") != ""
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) && !explicit {
			return config.LoadWithSections("", os.Environ(), owned)
		}
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return config.LoadWithSections(path, os.Environ(), owned)
}

func newVersionCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print build information",
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := version.Get()
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(info)
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "kudurru %s (%s, %s, %s)\n", info.Version, info.Commit, info.GoVersion, info.Platform)
			return err
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	return cmd
}

func newConfigCmd(configPath *string, o *cliOpts) *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Inspect the server configuration and manage CLI contexts"}
	cmd.AddCommand(contextCmds(o)...)
	cmd.AddCommand(
		&cobra.Command{
			Use:   "show",
			Short: "Print the effective configuration",
			RunE: func(c *cobra.Command, _ []string) error {
				cfg, err := loadConfig(c, *configPath)
				if err != nil {
					return err
				}
				b, err := yaml.Marshal(cfg)
				if err != nil {
					return err
				}
				_, err = c.OutOrStdout().Write(b)
				return err
			},
		},
		&cobra.Command{
			Use:   "schema",
			Short: "List every settings key with its scope and flags",
			RunE: func(c *cobra.Command, _ []string) error {
				w := c.OutOrStdout()
				_, _ = fmt.Fprintf(w, "%-32s %-10s %-8s %-9s %s\n", "KEY", "TYPE", "SCOPE", "FLAGS", "HELP")
				for _, k := range config.Schema {
					flags := ""
					if k.HostOnly {
						flags += "H"
					}
					if k.RestartRequired {
						flags += "R"
					}
					if k.WriteOnly {
						flags += "W"
					}
					help := k.Help
					if d := k.Shape().Describe(); d != "" {
						help += " [" + d + "]"
					}
					_, _ = fmt.Fprintf(w, "%-32s %-10s %-8s %-9s %s\n", k.Path, k.Type, k.Scope, flags, help)
				}
				_, _ = fmt.Fprintln(w, "\nFlags: H=hostOnly (file/env only, never the API)  R=restartRequired  W=writeOnly")
				_, _ = fmt.Fprintln(w, "[...] lists the allowed values, ranges and formats; other values are rejected.")
				return nil
			},
		},
	)
	return cmd
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
