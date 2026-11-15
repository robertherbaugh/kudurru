package main

import (
	"encoding/json"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/cli"
)

func newClusterCmd(o *cliOpts) *cobra.Command {
	cmd := &cobra.Command{Use: "cluster", Short: "Inspect cluster membership and store status"}
	addClusterEnrollmentCommands(cmd, o)
	addClusterPromotionCommands(cmd, o)
	addClusterAnchorsCommand(cmd, o)
	addClusterJoinCommand(cmd)
	addClusterResolutionCommands(cmd)
	cmd.AddCommand(&cobra.Command{
		Use: "status", Short: "Show the connected node's cluster status", Args: cobra.NoArgs,
		Long: `Read the connected node's store driver, replication role, epoch, reported
leader, member count and read-only mode. On the SQLite driver ROLE is
standalone, primary or follower, and LEADER names the primary. An enrolled
follower forwards some commands to its primary and refuses the others with
primary_required; -o json lists both sets. Static twins each report their own
local leader and store identity. This observation does not establish quorum,
peer health or permission to promote a node. Use kudurru nodes for individual
node readiness.`,
		Example: "  kudurru cluster status\n  kudurru cluster status -o wide\n  kudurru cluster status -o json",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := o.format(); err != nil {
				return err
			}
			c, _, err := o.client()
			if err != nil {
				return err
			}
			var raw json.RawMessage
			if err := c.Get(cmd.Context(), "/cluster", nil, &raw); err != nil {
				return err
			}
			return o.render(cmd, raw, func(wide bool) (*cli.Table, error) {
				var status api.ClusterInfo
				if err := json.Unmarshal(raw, &status); err != nil {
					return nil, err
				}
				t := &cli.Table{Headers: []string{"NODE", "DRIVER", "ROLE", "LEADER", "NODES", "READ-ONLY"}}
				row := []string{status.Node, status.Driver, firstNonEmpty(status.Role, "-"), firstNonEmpty(status.Leader, "-"), strconv.Itoa(status.Nodes), strconv.FormatBool(status.ReadOnly)}
				if wide {
					t.Headers = append(t.Headers, "CLUSTER-ID", "EPOCH")
					row = append(row, firstNonEmpty(status.ClusterID, "-"), strconv.FormatUint(status.Epoch, 10))
				}
				t.Add(row...)
				return t, nil
			})
		},
	})
	return cmd
}
