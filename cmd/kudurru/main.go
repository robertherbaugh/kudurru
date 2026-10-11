// Command kudurru is the single binary: server (`kudurru serve`), CLI and,
// later, the dnsdist/keepalived supervisors (`kudurru edge run`,
// `kudurru vip run`). Seven letters.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/version"
)

// defaultConfigPath is a var so tests can point it at a missing file.
var defaultConfigPath = "/etc/kudurru/kudurru.yaml"

func main() {
	if err := newRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newRoot() *cobra.Command {
	var configPath string
	root := &cobra.Command{
		Use:           "kudurru",
		Short:         "kudurru (koo-DOO-roo): a Kubernetes-native DNS server that also runs as one container",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&configPath, "config", envOr("KUDURRU_CONFIG", defaultConfigPath), "path to kudurru.yaml")

	root.AddCommand(
		newServeCmd(&configPath),
		newVersionCmd(),
		newHealthCmd(),
		newDoctorCmd(&configPath),
		newConfigCmd(&configPath),
	)
	root.AddCommand(newEdgeCmd(), newVIPCmd())
	return root
}

// loadConfig reads the config file. When the path is the default and the
// file does not exist, built-in defaults are used so `docker run` with no
// mounted file still starts.
func loadConfig(cmd *cobra.Command, path string) (*config.Config, error) {
	explicit := cmd.Flags().Changed("config") || os.Getenv("KUDURRU_CONFIG") != ""
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) && !explicit {
			return config.Load("", os.Environ())
		}
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return config.Load(path, os.Environ())
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

func newConfigCmd(configPath *string) *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Inspect configuration"}
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
					_, _ = fmt.Fprintf(w, "%-32s %-10s %-8s %-9s %s\n", k.Path, k.Type, k.Scope, flags, k.Help)
				}
				_, _ = fmt.Fprintln(w, "\nFlags: H=hostOnly (file/env only, never the API)  R=restartRequired  W=writeOnly")
				return nil
			},
		},
	)
	return cmd
}

func newDoctorCmd(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check the local configuration and environment",
		RunE: func(c *cobra.Command, _ []string) error {
			w := c.OutOrStdout()
			info := version.Get()
			say := func(format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
			say("kudurru %s (%s)\n", info.Version, info.Platform)
			cfg, err := loadConfig(c, *configPath)
			if err != nil {
				say("config: FAIL %v\n", err)
				return err
			}
			src := cfg.Source
			if src == "" {
				src = "built-in defaults (no file at " + *configPath + ")"
			}
			say("config: OK (%s)\n", src)
			say("node: %s  dataDir: %s\n", cfg.Node.Name, cfg.Node.DataDir)
			for _, l := range cfg.Listeners.DNS {
				say("dns listener: %s %v\n", l.Address, l.Protocols)
			}
			for name, g := range cfg.Upstreams.Groups {
				say("upstream group %s: %v (%s, %s)\n", name, g.Servers, g.Strategy, g.Timeout)
			}
			if len(cfg.EnvLocked) > 0 {
				say("env-locked keys: %v\n", cfg.EnvLocked)
			}
			if st, err := os.Stat(cfg.Node.DataDir); err != nil {
				say("dataDir: WARN %v\n", err)
			} else if !st.IsDir() {
				say("dataDir: WARN %s is not a directory\n", cfg.Node.DataDir)
			} else {
				say("dataDir: OK\n")
			}
			return nil
		},
	}
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
