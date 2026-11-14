package main

import (
	"github.com/spf13/cobra"

	"github.com/robertherbaugh/kudurru/internal/api"
)

func addClusterAnchorsCommand(root *cobra.Command, o *cliOpts) {
	root.AddCommand(&cobra.Command{Use: "anchors", Short: "Show this node's managed root trust-anchor status", Args: cobra.NoArgs,
		Long: `Show the node's root trust-anchor state (GET /cluster/anchors): the accepted
owner and its RFC 5011 key states, hold-down and schedule, quarantine and
recovery records, the keys this member denied locally, the updater or deny-only
supervisor, and on a member that pulls whether it refuses the primary's state
and why. Read-only; nothing here is a recommendation to act. A fenced or
conflicted member's API is closed: read its kudurru_root_anchor_* metrics, or
stop it and run kudurru anchors status --offline.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := o.format(); err != nil {
				return err
			}
			c, _, err := o.client()
			if err != nil {
				return err
			}
			var out api.RootAnchorStatus
			if err := c.Get(cmd.Context(), "/cluster/anchors", nil, &out); err != nil {
				return err
			}
			return o.render(cmd, out, nil)
		}})
}
