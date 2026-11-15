package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

// The IAM writer commands (D396). Every mutation is one request: the CLI never
// reads a fresh review, follows a redirect or retries a refused or uncertain
// command. Passwords are read from a terminal prompt or stdin, never from a
// flag value that would stay in the shell history.

// readIAMManifest reads one desired YAML or JSON manifest of at most 64 KiB and
// decodes it strictly (no duplicate or unknown keys).
func readIAMManifest(cmd *cobra.Command, file, kind string, out any) error {
	if file == "" {
		return fmt.Errorf("--file is required (one %s manifest without server metadata)", kind)
	}
	reader := cmd.InOrStdin()
	if file != "-" {
		f, err := os.Open(file) //nolint:gosec // the operator names their desired manifest
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		reader = f
	}
	raw, err := io.ReadAll(io.LimitReader(reader, (64<<10)+1))
	if err != nil || len(raw) > 64<<10 {
		return fmt.Errorf("%s manifest exceeds 64 KiB or cannot be read", kind)
	}
	if raw, err = yaml.YAMLToJSONStrict(raw); err != nil {
		return fmt.Errorf("invalid %s YAML or JSON", kind)
	}
	return decodeReviewedCommandJSON(raw, out, "invalid "+kind+" manifest")
}

func reviewFlags(sub *cobra.Command, uid, rv *string) {
	sub.Flags().StringVar(uid, "uid", "", "required: exact reviewed target UID")
	sub.Flags().StringVar(rv, "resource-version", "", "required: exact reviewed target resourceVersion")
}

func exactReview(uid, rv string) (api.ObjectReview, error) {
	review := api.ObjectReview{UID: uid, ResourceVersion: rv}
	if !validObjectReview(review) {
		return review, errors.New("exact --uid and --resource-version from the reviewed read are required")
	}
	return review, nil
}

func newIAMUsersCmd(o *cliOpts) *cobra.Command {
	root := &cobra.Command{Use: "users", Short: "Review and manage local accounts", Long: `Review and manage local accounts through the complete SQLite API.
Create makes an ordinary account that must change its password at the first
login. Reset, update and delete require the exact UID and resourceVersion from
"kudurru iam users list". The built-in administrator and accounts of other
managers (configuration, bootstrap) cannot be changed here; use
"kudurru admin reset-password" on the node for them. On the Kubernetes driver
these commands answer 503 user_management_unavailable.`}
	root.AddCommand(&cobra.Command{Use: "list", Short: "List local accounts with their UID and resourceVersion", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, _, err := o.client()
		if err != nil {
			return err
		}
		var page api.AuthList[api.IAMUser]
		if err := c.Get(cmd.Context(), "/iam/users", nil, &page); err != nil {
			return err
		}
		return o.render(cmd, page, nil)
	}})
	var displayName string
	var stdin bool
	create := &cobra.Command{Use: "create NAME", Short: "Create one absent local account", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !servingLocalUserName(args[0]) {
			return errors.New("NAME must be a lower-case DNS subdomain")
		}
		pw, err := o.readPassword(cmd, "", stdin, args[0])
		if err != nil {
			return err
		}
		c, _, err := o.client()
		if err != nil {
			return err
		}
		var out api.IAMUser
		if err := c.IAMCommand(cmd.Context(), http.MethodPost, "/iam/users", api.CreateLocalUserRequest{Name: args[0], DisplayName: displayName, Password: pw}, &out); err != nil {
			return err
		}
		return o.render(cmd, out, nil)
	}}
	create.Flags().StringVar(&displayName, "display-name", "", "display name of the account")
	create.Flags().BoolVar(&stdin, "password-stdin", false, "read the initial password from stdin instead of a terminal prompt")
	root.AddCommand(create)

	var resetUID, resetRV string
	var resetStdin bool
	reset := &cobra.Command{Use: "reset-password NAME", Short: "Reset an exact account's password and end its sessions and tokens", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !servingLocalUserName(args[0]) {
			return errors.New("NAME must be a lower-case DNS subdomain")
		}
		review, err := exactReview(resetUID, resetRV)
		if err != nil {
			return err
		}
		pw, err := o.readPassword(cmd, "", resetStdin, args[0])
		if err != nil {
			return err
		}
		c, _, err := o.client()
		if err != nil {
			return err
		}
		if err := c.IAMCommand(cmd.Context(), http.MethodPost, "/iam/users/"+args[0]+"/password", api.ResetLocalUserPasswordRequest{UID: review.UID, ResourceVersion: review.ResourceVersion, NewPassword: pw}, nil); err != nil {
			return err
		}
		return o.render(cmd, map[string]string{"name": args[0], "status": "password reset"}, nil)
	}}
	reviewFlags(reset, &resetUID, &resetRV)
	reset.Flags().BoolVar(&resetStdin, "password-stdin", false, "read the new password from stdin instead of a terminal prompt")
	root.AddCommand(reset)

	var updUID, updRV, updDisplay string
	var updGroups []string
	var updDisabled bool
	update := &cobra.Command{Use: "update NAME", Short: "Change an exact account's display name, groups or disabled state", Long: `Change only the fields whose flags are given. --group replaces the complete
list of local groups (repeat it, or pass --group= to remove every group).
Changing groups or disabling the account ends its sessions and deletes its API
tokens in the same commit; enabling it again restores none of them.`, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !servingLocalUserName(args[0]) {
			return errors.New("NAME must be a lower-case DNS subdomain")
		}
		review, err := exactReview(updUID, updRV)
		if err != nil {
			return err
		}
		req := api.UpdateLocalUserRequest{UID: review.UID, ResourceVersion: review.ResourceVersion}
		if cmd.Flags().Changed("display-name") {
			req.DisplayName = &updDisplay
		}
		if cmd.Flags().Changed("group") {
			groups := []string{}
			for _, g := range updGroups {
				if g != "" {
					groups = append(groups, g)
				}
			}
			req.Groups = &groups
		}
		if cmd.Flags().Changed("disabled") {
			req.Disabled = &updDisabled
		}
		if req.DisplayName == nil && req.Groups == nil && req.Disabled == nil {
			return errors.New("give at least one of --display-name, --group or --disabled")
		}
		c, _, err := o.client()
		if err != nil {
			return err
		}
		var out api.IAMUser
		if err := c.IAMCommand(cmd.Context(), http.MethodPatch, "/iam/users/"+args[0], req, &out); err != nil {
			return err
		}
		return o.render(cmd, out, nil)
	}}
	reviewFlags(update, &updUID, &updRV)
	update.Flags().StringVar(&updDisplay, "display-name", "", "new display name (empty clears it)")
	update.Flags().StringArrayVar(&updGroups, "group", nil, "a local group; repeat for each group, --group= for none")
	update.Flags().BoolVar(&updDisabled, "disabled", false, "--disabled disables the account, --disabled=false enables it")
	root.AddCommand(update)

	var delUID, delRV string
	del := &cobra.Command{Use: "delete NAME", Short: "Delete an exact account and its sessions and tokens", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !servingLocalUserName(args[0]) {
			return errors.New("NAME must be a lower-case DNS subdomain")
		}
		review, err := exactReview(delUID, delRV)
		if err != nil {
			return err
		}
		c, _, err := o.client()
		if err != nil {
			return err
		}
		if err := c.IAMCommand(cmd.Context(), http.MethodDelete, "/iam/users/"+args[0], review, nil); err != nil {
			return err
		}
		return o.render(cmd, map[string]string{"name": args[0], "status": "deleted"}, nil)
	}}
	reviewFlags(del, &delUID, &delRV)
	root.AddCommand(del)
	return root
}

func serviceAccountCLIPath(args []string) (string, error) {
	if len(args) != 2 || len(validation.IsDNS1123Label(args[0])) != 0 || len(validation.IsDNS1123Subdomain(args[1])) != 0 {
		return "", errors.New("exact NAMESPACE and NAME are required")
	}
	return "/iam/serviceaccounts/" + args[0] + "/" + args[1], nil
}

func newServiceAccountsCmd(o *cliOpts) *cobra.Command {
	root := &cobra.Command{Use: "serviceaccounts", Aliases: []string{"sa"}, Short: "Review and manage kudurru ServiceAccounts and their tokens", Long: `Review and manage kudurru ServiceAccounts through the complete SQLite API.
Create requires an absent target. Replace and delete require the exact UID and
resourceVersion from a separately reviewed get. Desired manifests contain
apiVersion, kind, metadata (name, namespace, labels, annotations) and
spec.description/spec.disabled. Disabling or deleting an account ends its
tokens in the same commit. create-token prints the new token once; it cannot be
shown again. On the Kubernetes driver the commands answer 503
iam_management_unavailable: change the objects with kubectl.`}
	root.AddCommand(&cobra.Command{Use: "list", Short: "List service accounts", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, _, err := o.client()
		if err != nil {
			return err
		}
		var page api.AuthList[api.ServiceAccountInfo]
		if err := c.Get(cmd.Context(), "/iam/serviceaccounts", nil, &page); err != nil {
			return err
		}
		return o.render(cmd, page, nil)
	}})
	root.AddCommand(&cobra.Command{Use: "get NAMESPACE NAME", Short: "Read an exact service account for review", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		path, err := serviceAccountCLIPath(args)
		if err != nil {
			return err
		}
		c, _, err := o.client()
		if err != nil {
			return err
		}
		var out publicv1.ServiceAccount
		if err := c.Get(cmd.Context(), path, nil, &out); err != nil {
			return err
		}
		return o.render(cmd, out, nil)
	}})
	for _, action := range []string{"create", "replace", "delete"} {
		var file, uid, rv string
		use, args := action+" NAMESPACE NAME", cobra.ExactArgs(2)
		if action == "create" {
			use, args = "create", cobra.NoArgs
		}
		sub := &cobra.Command{Use: use, Short: map[string]string{"create": "Create one absent ServiceAccount", "replace": "Replace one reviewed API-owned ServiceAccount", "delete": "Delete one reviewed API-owned ServiceAccount and its tokens"}[action], Args: args, RunE: func(cmd *cobra.Command, args []string) error {
			path, method := "/iam/serviceaccounts", http.MethodPost
			var body any
			if action != "create" {
				var err error
				if path, err = serviceAccountCLIPath(args); err != nil {
					return err
				}
			}
			var review api.ObjectReview
			if action != "create" {
				var err error
				if review, err = exactReview(uid, rv); err != nil {
					return err
				}
				body, method = review, http.MethodDelete
			}
			if action != "delete" {
				var desired api.ServiceAccountDesired
				if err := readIAMManifest(cmd, file, "ServiceAccount", &desired); err != nil {
					return err
				}
				if _, err := serviceAccountDesired(desired); err != nil {
					return err
				}
				body = desired
				if action == "replace" {
					if desired.Metadata.Namespace != args[0] || desired.Metadata.Name != args[1] {
						return errors.New("ServiceAccount manifest and explicit target must match")
					}
					body, method = api.ReplaceServiceAccountCommand{ObjectReview: review, ServiceAccount: desired}, http.MethodPut
				}
			}
			c, _, err := o.client()
			if err != nil {
				return err
			}
			if action == "delete" {
				if err := c.IAMCommand(cmd.Context(), method, path, body, nil); err != nil {
					return err
				}
				return o.render(cmd, map[string]string{"namespace": args[0], "name": args[1], "status": "deleted"}, nil)
			}
			var out publicv1.ServiceAccount
			if err := c.IAMCommand(cmd.Context(), method, path, body, &out); err != nil {
				return err
			}
			return o.render(cmd, out, nil)
		}}
		if action != "delete" {
			sub.Flags().StringVarP(&file, "file", "f", "", "required: one desired ServiceAccount YAML/JSON file or - for stdin")
		}
		if action != "create" {
			reviewFlags(sub, &uid, &rv)
		}
		root.AddCommand(sub)
	}
	var tokUID, tokDesc, tokExpires string
	var tokScopes []string
	token := &cobra.Command{Use: "create-token NAMESPACE NAME", Short: "Issue an API token for an exact service account and print it once", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		path, err := serviceAccountCLIPath(args)
		if err != nil {
			return err
		}
		if tokUID == "" || strings.TrimSpace(tokUID) != tokUID {
			return errors.New("exact --uid of the reviewed service account is required")
		}
		c, _, err := o.client()
		if err != nil {
			return err
		}
		var out api.CreatedToken
		if err := c.IAMCommand(cmd.Context(), http.MethodPost, path+"/tokens", api.CreateServiceAccountTokenRequest{UID: tokUID, Description: tokDesc, ExpiresIn: tokExpires, Scopes: tokScopes}, &out); err != nil {
			return err
		}
		return o.render(cmd, out, nil)
	}}
	token.Flags().StringVar(&tokUID, "uid", "", "required: exact UID of the reviewed service account")
	token.Flags().StringVar(&tokDesc, "description", "", "free text shown in the token list")
	token.Flags().StringVar(&tokExpires, "expires-in", "", "Go duration (720h); default auth.tokens.maxLifetime, or never")
	token.Flags().StringArrayVar(&tokScopes, "scope", nil, "narrow the token to a grant (viewer, zone-admin@zone:NAME); repeat for more")
	root.AddCommand(token)
	return root
}

func identityProviderCLIPath(args []string) (string, error) {
	if len(args) != 1 || len(validation.IsDNS1123Label(args[0])) != 0 {
		return "", errors.New("exact provider NAME (a DNS label) is required")
	}
	return "/iam/identityproviders/" + args[0], nil
}

func newIdentityProvidersCmd(o *cliOpts) *cobra.Command {
	root := &cobra.Command{Use: "identityproviders", Aliases: []string{"idp"}, Short: "Review and manage IdentityProvider objects", Long: `Review and manage IdentityProvider objects through the complete SQLite API.
Create requires an absent target whose id the node configuration does not
define. Replace and delete require the exact UID and resourceVersion from a
separately reviewed get. spec.clientSecretRef names a credential reference (a
Secret key, an allowlisted file or environment variable); a secret value is
never accepted. Changing or deleting a provider ends every session and token
that came from it. On the Kubernetes driver the commands answer 503
iam_management_unavailable: change the objects with kubectl.`}
	root.AddCommand(&cobra.Command{Use: "list", Short: "List configured providers, including configuration-file entries", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, _, err := o.client()
		if err != nil {
			return err
		}
		var page api.AuthList[api.IdentityProviderInfo]
		if err := c.Get(cmd.Context(), "/iam/identityproviders", nil, &page); err != nil {
			return err
		}
		return o.render(cmd, page, nil)
	}})
	root.AddCommand(&cobra.Command{Use: "get NAME", Short: "Read an exact IdentityProvider object for review", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		path, err := identityProviderCLIPath(args)
		if err != nil {
			return err
		}
		c, _, err := o.client()
		if err != nil {
			return err
		}
		var out publicv1.IdentityProvider
		if err := c.Get(cmd.Context(), path, nil, &out); err != nil {
			return err
		}
		return o.render(cmd, out, nil)
	}})
	for _, action := range []string{"create", "replace", "delete"} {
		var file, uid, rv string
		use, args := action+" NAME", cobra.ExactArgs(1)
		if action == "create" {
			use, args = "create", cobra.NoArgs
		}
		sub := &cobra.Command{Use: use, Short: map[string]string{"create": "Create one absent IdentityProvider", "replace": "Replace one reviewed API-owned IdentityProvider", "delete": "Delete one reviewed API-owned IdentityProvider"}[action], Args: args, RunE: func(cmd *cobra.Command, args []string) error {
			path, method := "/iam/identityproviders", http.MethodPost
			var body any
			var review api.ObjectReview
			if action != "create" {
				var err error
				if path, err = identityProviderCLIPath(args); err != nil {
					return err
				}
				if review, err = exactReview(uid, rv); err != nil {
					return err
				}
				body, method = review, http.MethodDelete
			}
			if action != "delete" {
				var desired api.IdentityProviderDesired
				if err := readIAMManifest(cmd, file, "IdentityProvider", &desired); err != nil {
					return err
				}
				if _, err := identityProviderDesired(desired); err != nil {
					return err
				}
				body = desired
				if action == "replace" {
					if desired.Metadata.Name != args[0] {
						return errors.New("IdentityProvider manifest and explicit target must match")
					}
					body, method = api.ReplaceIdentityProviderCommand{ObjectReview: review, IdentityProvider: desired}, http.MethodPut
				}
			}
			c, _, err := o.client()
			if err != nil {
				return err
			}
			if action == "delete" {
				if err := c.IAMCommand(cmd.Context(), method, path, body, nil); err != nil {
					return err
				}
				return o.render(cmd, map[string]string{"name": args[0], "status": "deleted"}, nil)
			}
			var out publicv1.IdentityProvider
			if err := c.IAMCommand(cmd.Context(), method, path, body, &out); err != nil {
				return err
			}
			return o.render(cmd, out, nil)
		}}
		if action != "delete" {
			sub.Flags().StringVarP(&file, "file", "f", "", "required: one desired IdentityProvider YAML/JSON file or - for stdin")
		}
		if action != "create" {
			reviewFlags(sub, &uid, &rv)
		}
		root.AddCommand(sub)
	}
	return root
}
