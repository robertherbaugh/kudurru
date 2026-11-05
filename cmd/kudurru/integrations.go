package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/apiclient"
	"github.com/robertherbaugh/kudurru/internal/cli"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/util/validation"
)

func newIntegrationsCmd(o *cliOpts) *cobra.Command {
	group := &cobra.Command{Use: "integrations", Short: "Review and run captured integration operations"}
	list := &cobra.Command{Use: "list", Short: "List bounded integration status in one namespace", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, resolved, err := o.client()
		if err != nil {
			return err
		}
		namespace := o.nsFor(resolved)
		if len(validation.IsDNS1123Label(namespace)) != 0 {
			return errors.New("invalid integration namespace")
		}
		raw, err := c.IntegrationCall(cmd.Context(), http.MethodGet, "/integrations", url.Values{"namespace": {namespace}}, nil)
		if err != nil {
			return err
		}
		return o.render(cmd, raw, func(bool) (*cli.Table, error) {
			var body struct {
				Items []api.IntegrationSummary `json:"items"`
			}
			if err := json.Unmarshal(raw, &body); err != nil {
				return nil, err
			}
			table := &cli.Table{Headers: []string{"NAME", "TYPE", "PHASE", "UID", "REVISION"}}
			for _, item := range body.Items {
				table.Add(item.Name, item.Type, item.Phase, item.UID, item.Revision)
			}
			return table, nil
		})
	}}
	group.AddCommand(list)
	group.AddCommand(newIntegrationWebhookCmd())
	for _, command := range []string{"get", "check", "preview", "sync", "actions", "act"} {
		var review api.IntegrationReview
		var params string
		use := command + " NAME"
		nargs := 1
		if command == "act" {
			use += " ACTION"
			nargs = 2
		}
		cmd := &cobra.Command{Use: use, Short: integrationCommandHelp(command), Args: cobra.ExactArgs(nargs), RunE: func(cmd *cobra.Command, args []string) error {
			if len(validation.IsDNS1123Subdomain(args[0])) != 0 {
				return errors.New("invalid integration name")
			}
			if command != "get" && (review.UID == "" || len(review.UID) > 128 || review.Revision == "" || len(review.Revision) > 256) {
				return errors.New("--uid and --revision require the exact reviewed integration")
			}
			if command == "sync" && (review.Mode != "observe" && review.Mode != "publish" || review.Mode == "observe" && review.PreviewDigest != "" || review.Mode == "publish" && len(review.PreviewDigest) != 64) {
				return errors.New("sync requires --mode observe, or --mode publish with --preview-digest")
			}
			if command == "act" {
				if len(args[1]) > 128 || strings.ContainsAny(args[1], "/\\\r\n\x00") {
					return errors.New("invalid integration action")
				}
				if len(params) > 60<<10 || !json.Valid([]byte(params)) {
					return errors.New("--params must be bounded valid JSON")
				}
				review.Params = json.RawMessage(params)
			}
			c, resolved, err := o.client()
			if err != nil {
				return err
			}
			namespace := o.nsFor(resolved)
			if len(validation.IsDNS1123Label(namespace)) != 0 {
				return errors.New("invalid integration namespace")
			}
			path := "/integrations/" + apiclient.Seg(namespace) + "/" + apiclient.Seg(args[0])
			method := http.MethodPost
			var query url.Values
			var body any = review
			switch command {
			case "get":
				method, body = http.MethodGet, nil
			case "actions":
				method, body = http.MethodGet, nil
				path += "/actions"
				query = url.Values{"uid": {review.UID}, "revision": {review.Revision}}
			case "act":
				path += "/actions/" + apiclient.Seg(args[1])
			default:
				path += "/" + command
			}
			raw, err := c.IntegrationCall(cmd.Context(), method, path, query, body)
			if err != nil {
				return err
			}
			return o.render(cmd, raw, nil)
		}}
		if command != "get" {
			cmd.Flags().StringVar(&review.UID, "uid", "", "Exact reviewed integration UID")
			cmd.Flags().StringVar(&review.Revision, "revision", "", "Exact reviewed resource version")
		}
		if command == "sync" {
			cmd.Flags().StringVar(&review.Mode, "mode", "", "Explicit observe or publish mode")
			cmd.Flags().StringVar(&review.PreviewDigest, "preview-digest", "", "Exact aggregate digest from preview")
		}
		if command == "act" {
			cmd.Flags().StringVar(&params, "params", "{}", "Bounded JSON action parameters")
		}
		group.AddCommand(cmd)
	}
	return group
}

func integrationCommandHelp(command string) string {
	switch command {
	case "get":
		return "Read integration identity and status for review"
	case "check":
		return "Check the reviewed provider connection"
	case "preview":
		return "Preview complete changes without applying them"
	case "sync":
		return "Run one reviewed publication or observation"
	case "actions":
		return "List actions advertised by the reviewed provider"
	default:
		return "Invoke one advertised action without retry"
	}
}
