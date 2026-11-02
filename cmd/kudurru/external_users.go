package main

import (
	"errors"
	"net/url"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/spf13/cobra"
)

func newExternalUsersCmd(o *cliOpts) *cobra.Command {
	root := &cobra.Command{Use: "external-users", Short: "Inspect and administer persisted external accounts", Long: `Inspect persisted external identities and revoke their Kudurru credentials.
Requires the complete SQLite runtime and users.read or users.write permission.
Read the exact name, UID and revision before choosing a mutation. A conflict
requires a fresh review; commands never refresh or replay a mutation for you.
Log out everywhere is local to Kudurru and does not end identity-provider sessions.`}
	var limit int
	var cursor string
	list := &cobra.Command{Use: "list", Short: "Read one bounded account page", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if limit < 1 || limit > 100 {
			return errors.New("--limit must be between 1 and 100")
		}
		c, _, err := o.client()
		if err != nil {
			return err
		}
		q := url.Values{"limit": {cmd.Flag("limit").Value.String()}}
		if cursor != "" {
			q.Set("continue", cursor)
		}
		var page api.AuthList[api.ExternalUser]
		if err := c.Get(cmd.Context(), "/iam/external-users", q, &page); err != nil {
			return err
		}
		return o.render(cmd, page, nil)
	}}
	list.Flags().IntVar(&limit, "limit", 50, "maximum accounts on this page (1–100)")
	list.Flags().StringVar(&cursor, "continue", "", "exact continuation from the previous page")
	root.AddCommand(list)
	root.AddCommand(&cobra.Command{Use: "get NAME", Short: "Read an account and its current UID/revision", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !externalHex(args[0], 64) {
			return errors.New("NAME must be the exact account digest from external-users list")
		}
		c, _, err := o.client()
		if err != nil {
			return err
		}
		var account api.ExternalUser
		if err := c.Get(cmd.Context(), "/iam/external-users/"+args[0], nil, &account); err != nil {
			return err
		}
		return o.render(cmd, account, nil)
	}})
	for _, action := range []string{"disable", "enable", "logout"} {
		var uid, revision string
		sub := &cobra.Command{Use: action + " NAME", Short: map[string]string{"disable": "Disable an account and revoke its local credentials", "enable": "Enable an account without restoring revoked credentials", "logout": "Revoke all local sessions and personal tokens"}[action], Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			if !externalHex(args[0], 64) || !externalHex(uid, 32) || !externalHex(revision, 64) {
				return errors.New("exact NAME, --uid and --revision from the reviewed account are required")
			}
			c, _, err := o.client()
			if err != nil {
				return err
			}
			if err := c.Post(cmd.Context(), "/iam/external-users/"+args[0]+"/"+action, nil, api.ExternalUserCommand{UID: uid, Revision: revision}, nil); err != nil {
				return err
			}
			return o.render(cmd, map[string]string{"name": args[0], "action": action, "status": "committed"}, nil)
		}}
		sub.Flags().StringVar(&uid, "uid", "", "required: exact independent account UID")
		sub.Flags().StringVar(&revision, "revision", "", "required: revision from the reviewed account")
		root.AddCommand(sub)
	}
	return root
}
