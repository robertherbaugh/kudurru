package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/cli"
	"github.com/spf13/cobra"
)

func addSettingsOwnershipCommands(parent *cobra.Command, o *cliOpts) {
	ownership := &cobra.Command{Use: "ownership", Short: "Show section ownership and revisions", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, _, err := o.client()
		if err != nil {
			return err
		}
		var body api.SettingsBody
		if err := c.Get(cmd.Context(), "/settings", nil, &body); err != nil {
			return err
		}
		return o.render(cmd, body.Sections, func(bool) (*cli.Table, error) {
			t := &cli.Table{Headers: []string{"SECTION", "OWNER", "REVISION"}}
			for _, section := range slices.Sorted(maps.Keys(body.Sections)) {
				value := body.Sections[section]
				t.Add(section, value.Owner, value.Revision)
			}
			return t, nil
		})
	}}
	parent.AddCommand(ownership)
	for _, action := range []string{"adopt", "release"} {
		var revision string
		command := &cobra.Command{Use: action + " upstreams", Short: action + " upstream settings ownership using a reviewed revision", Long: action + " upstream settings ownership. Read the revision with `kudurru settings ownership`. Before release, export with `kudurru settings yaml upstreams`, commit and apply those Helm values, then read the updated revision. Release refuses a handoff if the file settings differ.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != "upstreams" {
				return fmt.Errorf("only upstreams supports ownership changes")
			}
			c, _, err := o.client()
			if err != nil {
				return err
			}
			var body api.SettingsBody
			if err := c.Post(cmd.Context(), "/settings/sections/upstreams/ownership/"+action, nil, map[string]string{"revision": revision}, &body); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "upstreams are now %s-managed\n", body.Sections["upstreams"].Owner)
			return err
		}}
		command.Flags().StringVar(&revision, "revision", "", "reviewed revision from settings ownership")
		_ = command.MarkFlagRequired("revision")
		parent.AddCommand(command)
	}
	var proposedFile string
	export := &cobra.Command{Use: "yaml upstreams", Short: "Generate Helm values without saving (optionally from a proposed JSON section)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if args[0] != "upstreams" {
			return fmt.Errorf("only upstreams supports YAML generation")
		}
		c, _, err := o.client()
		if err != nil {
			return err
		}
		input := map[string]any{}
		if proposedFile != "" {
			raw, err := os.ReadFile(proposedFile) //nolint:gosec // operator-selected proposed settings file
			if err != nil {
				return err
			}
			var proposed map[string]any
			if err := json.Unmarshal(raw, &proposed); err != nil {
				return err
			}
			input["proposed"] = proposed
		}
		var body struct {
			YAML string `json:"yaml"`
		}
		if err := c.Post(cmd.Context(), "/settings/sections/upstreams/yaml", nil, input, &body); err != nil {
			return err
		}
		_, err = fmt.Fprint(cmd.OutOrStdout(), body.YAML)
		return err
	}}
	export.Flags().StringVar(&proposedFile, "proposed-file", "", "complete proposed upstreams section as JSON")
	parent.AddCommand(export)
}
