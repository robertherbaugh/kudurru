package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/robertherbaugh/kudurru/internal/license"
)

// supervisorDataDir is where the edge and VIP supervisors look for
// license.key; the images have no kudurru.yaml, so the key normally arrives
// through KUDURRU_LICENSE_KEY or KUDURRU_LICENSE_KEY_FILE.
const supervisorDataDir = "/var/lib/kudurru"

// newLicenseManager builds the node's license manager over the keys embedded
// in the binary; tests replace it to trust a key of their own.
var newLicenseManager = func() (*license.Manager, error) { return license.New(license.Options{}) }

// requireFeature is the license gate of the supervisors under ee/edge
// (`kudurru edge run`, `kudurru vip run`). Those processes sit in the data
// path, so the gate must never turn a license lapse into a DNS outage: it
// refuses only a start with no verifiable key that ever covered the edge. A
// valid key, a key in its grace period and a key that has expired since
// (license.Manager.RequireStart) all start, the last two with a warning. It
// checks once, at start: a running supervisor is never stopped by an expiry.
func requireFeature(logger *slog.Logger, f license.Feature) error {
	m, err := newLicenseManager()
	if err != nil {
		return err
	}
	if err := m.Load(license.DefaultSources("", supervisorDataDir, os.Getenv)); err != nil {
		logger.Warn("license key", "error", err)
	}
	warning, err := m.RequireStart(f)
	if err != nil {
		return fmt.Errorf("%w (set KUDURRU_LICENSE_KEY or KUDURRU_LICENSE_KEY_FILE)", err)
	}
	if warning != "" {
		logger.Warn(warning)
	}
	return nil
}

// newLicenseCmd is `kudurru license`: offline helpers for license keys.
func newLicenseCmd(configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "license",
		Short: "Show or check the license key (offline; keys are verified without a license server)",
		Long: "kudurru is source-available under the Kudurru Community License (LICENSE). Running it for\n" +
			"eligible personal or nonprofit use is free; business and government production needs an agreement.\n" +
			"Enterprise materials have their own notice; commercial features depend on the Order Form. A key unlocks more\n" +
			"than one node, more than one policy and the features under ee/ (OIDC single sign-on, the dnsdist and\n" +
			"keepalived edge, the integrations). A free personal key covers 3 nodes. See https://kudurru.io/license.\n\n" +
			"The key is read from --license-key, KUDURRU_LICENSE_KEY, the file KUDURRU_LICENSE_KEY_FILE names, or\n" +
			"<node.dataDir>/license.key, in that order. Licensing never affects DNS answering: without a key the\n" +
			"node serves DNS as usual and only the node limit, the policy limit and the licensed features apply.",
	}

	var (
		statusKey  string
		statusJSON bool
	)
	status := &cobra.Command{
		Use:   "status",
		Short: "Show the license this node would run with",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			dataDir := supervisorDataDir
			if cfg, err := loadConfig(c, *configPath); err == nil {
				dataDir = cfg.Node.DataDir
			}
			m, err := newLicenseManager()
			if err != nil {
				return err
			}
			loadErr := m.Load(license.DefaultSources(statusKey, dataDir, os.Getenv))
			s := m.Status()
			if statusJSON {
				enc := json.NewEncoder(c.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(s)
			}
			printStatus(c.OutOrStdout(), s)
			return loadErr
		},
	}
	status.Flags().StringVar(&statusKey, "license-key", "", "check this key instead of the configured sources")
	status.Flags().BoolVar(&statusJSON, "json", false, "print as JSON")

	verify := &cobra.Command{
		Use:   "verify KEY|-",
		Short: "Verify a key and print what it grants (- reads the key from stdin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			raw := args[0]
			if raw == "-" {
				b, err := io.ReadAll(c.InOrStdin())
				if err != nil {
					return err
				}
				raw = string(b)
			}
			m, err := newLicenseManager()
			if err != nil {
				return err
			}
			if err := m.Set(raw, "argument"); err != nil {
				return err
			}
			printStatus(c.OutOrStdout(), m.Status())
			return nil
		},
	}
	cmd.AddCommand(status, verify)
	return cmd
}

func printStatus(w io.Writer, s license.Status) {
	p := func(k string, v any) { _, _ = fmt.Fprintf(w, "%-10s %v\n", k+":", v) }
	p("state", s.State)
	if s.Plan != "" {
		p("plan", s.Plan)
		p("licensee", s.Licensee)
		p("id", s.ID)
	}
	if s.Source != "" {
		p("source", s.Source)
	}
	p("nodes", s.Nodes)
	feats := "none"
	if len(s.Features) > 0 {
		parts := make([]string, len(s.Features))
		for i, f := range s.Features {
			parts[i] = string(f)
		}
		feats = strings.Join(parts, ", ")
	}
	p("features", feats)
	if s.ExpiresAt != nil {
		p("expires", s.ExpiresAt.Format(time.DateOnly))
	}
	if s.State == license.StateGrace {
		p("grace ends", s.GraceEndsAt.Format(time.DateOnly))
	}
	if s.Error != "" {
		p("error", s.Error)
	}
}
