package main

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/util/duration"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/apiclient"
	"github.com/robertherbaugh/kudurru/internal/cli"
)

// newListsRPZCmd is `kudurru lists rpz [name]`: the RPZ feeds of the node
// that answers (D391). Feed state is per node.
func newListsRPZCmd(o *cliOpts) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "rpz [name]",
		Short: "Show this node's RPZ feeds: state, serial, expiry, rule counts and ignored rules",
		Long: "Show the RPZ feeds of the node that answers. Every node transfers its own copy, so " +
			"run it against each node to compare them. With a name, only that BlockList's feeds.\n\n" +
			"STATE is pending, serving, expired or unavailable; queries whose policy selects a feed " +
			"that is not serving answer SERVFAIL. IGNORED counts policy rules the compiler skipped " +
			"(unsupported or malformed); -o wide lists them per reason.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, r, err := o.client()
			if err != nil {
				return err
			}
			path, q := "/blocklists/_rpz", url.Values{}
			switch {
			case len(args) == 1:
				path = "/blocklists/" + apiclient.Seg(o.nsFor(r)) + "/" + apiclient.Seg(args[0]) + "/rpz"
			case !all:
				q.Set("namespace", o.nsFor(r))
			}
			feeds, raws, err := listItems[api.RPZFeed](cmd.Context(), c, path, q)
			if err != nil {
				return err
			}
			return o.render(cmd, raws, func(wide bool) (*cli.Table, error) {
				return rpzFeedTable(feeds, wide, all && len(args) == 0, o.now()), nil
			})
		},
	}
	cmd.Flags().BoolVarP(&all, "all-namespaces", "A", false, "show feeds of lists in every namespace")
	return cmd
}

func rpzFeedTable(feeds []api.RPZFeed, wide, namespaces bool, now time.Time) *cli.Table {
	t := &cli.Table{Headers: []string{"NAME", "SOURCE", "ORIGIN", "STATE", "SERIAL", "RULES", "IGNORED", "EXPIRES", "NODE"}}
	if namespaces {
		t.Headers = append([]string{"NAMESPACE"}, t.Headers...)
	}
	if wide {
		t.Headers = append(t.Headers, "PRIMARY", "CONFIRMED", "NEXT-ATTEMPT", "IGNORED-REASONS", "MESSAGE")
	}
	for _, f := range feeds {
		serial, rules, ignored, expires := "-", "-", "-", "-"
		if f.Serial != nil {
			serial = strconv.FormatUint(uint64(*f.Serial), 10)
			if f.State != "serving" {
				serial += " (last)"
			}
		}
		if f.Counts != nil {
			rules = strconv.Itoa(f.Counts.Rules)
			ignored = strconv.Itoa(f.Counts.Invalid + f.Counts.Unsupported)
		}
		if f.ExpiresAt != nil {
			expires = relativeTime(*f.ExpiresAt, now)
		}
		row := []string{f.List, f.Source, f.Origin, f.State, serial, rules, ignored, expires, f.Node}
		if namespaces {
			row = append([]string{f.Namespace}, row...)
		}
		if wide {
			reasons := make([]string, 0, len(f.Ignored))
			for _, ig := range f.Ignored {
				reasons = append(reasons, ig.Reason+"="+strconv.Itoa(ig.Count))
			}
			row = append(row, firstNonEmpty(f.Primary, "-"), timeCell(f.LastConfirmedAt, now), timeCell(f.NextAttemptAt, now),
				firstNonEmpty(strings.Join(reasons, ","), "-"), firstNonEmpty(f.Message, "-"))
		}
		t.Add(row...)
	}
	return t
}

// relativeTime renders "in 9m" for a future instant and "3m ago" otherwise.
func relativeTime(at, now time.Time) string {
	if at.After(now) {
		return "in " + duration.HumanDuration(at.Sub(now))
	}
	return fmt.Sprintf("%s ago", duration.HumanDuration(now.Sub(at)))
}

func timeCell(at *time.Time, now time.Time) string {
	if at == nil || at.IsZero() {
		return "-"
	}
	return relativeTime(*at, now)
}
