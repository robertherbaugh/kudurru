package main

import (
	"errors"
	"io"
	"net/http"
	"os"

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

func roleBindingCLIPath(args []string) (string, error) {
	if len(args) != 2 || len(validation.IsDNS1123Label(args[0])) != 0 || len(validation.IsDNS1123Subdomain(args[1])) != 0 {
		return "", errors.New("exact NAMESPACE and NAME are required")
	}
	return "/iam/rolebindings/" + args[0] + "/" + args[1], nil
}

func readRoleBindingDesired(cmd *cobra.Command, file string) (api.RoleBindingDesired, error) {
	var in api.RoleBindingDesired
	if file == "" {
		return in, errors.New("--file is required (one RoleBinding manifest without server metadata)")
	}
	reader := cmd.InOrStdin()
	if file != "-" {
		f, err := os.Open(file) //nolint:gosec // the operator names their desired manifest
		if err != nil {
			return in, err
		}
		defer func() { _ = f.Close() }()
		reader = f
	}
	raw, err := io.ReadAll(io.LimitReader(reader, (64<<10)+1))
	if err != nil || len(raw) > 64<<10 {
		return in, errors.New("RoleBinding manifest exceeds 64 KiB or cannot be read")
	}
	raw, err = yaml.YAMLToJSONStrict(raw)
	if err != nil {
		return in, errors.New("invalid RoleBinding YAML or JSON")
	}
	if err := decodeRoleBindingJSON(raw, &in); err != nil {
		return in, err
	}
	if _, err := roleBindingDesired(in); err != nil {
		return in, err
	}
	return in, nil
}

func newRoleBindingsCmd(o *cliOpts) *cobra.Command {
	root := &cobra.Command{Use: "rolebindings", Short: "Review and manage public RoleBindings", Long: `Review and manage public RoleBindings through the complete SQLite API.
Create requires an absent target. Replace and delete require the exact UID and
resourceVersion from a separately reviewed get. No command refreshes or retries
a refused mutation. Config/file-owned bindings cannot be adopted. Desired
manifests contain apiVersion, kind, metadata (name, namespace, labels,
annotations), and spec; server identity and lifecycle fields are not accepted.`}
	root.AddCommand(&cobra.Command{Use: "list", Short: "List bindings, including config sources", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, _, err := o.client()
		if err != nil {
			return err
		}
		var page api.AuthList[api.RoleBindingInfo]
		if err := c.Get(cmd.Context(), "/iam/rolebindings", nil, &page); err != nil {
			return err
		}
		return o.render(cmd, page, nil)
	}})
	root.AddCommand(&cobra.Command{Use: "get NAMESPACE NAME", Short: "Read an exact binding for operator review", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		path, err := roleBindingCLIPath(args)
		if err != nil {
			return err
		}
		c, _, err := o.client()
		if err != nil {
			return err
		}
		var out publicv1.RoleBinding
		if err := c.Get(cmd.Context(), path, nil, &out); err != nil {
			return err
		}
		return o.render(cmd, out, nil)
	}})
	for _, action := range []string{"create", "replace", "delete"} {
		var file, uid, rv string
		use := action + " NAMESPACE NAME"
		args := cobra.ExactArgs(2)
		if action == "create" {
			use = "create"
			args = cobra.NoArgs
		}
		sub := &cobra.Command{Use: use, Short: map[string]string{"create": "Create one absent RoleBinding", "replace": "Replace one explicitly reviewed API-owned RoleBinding", "delete": "Delete one explicitly reviewed API-owned RoleBinding"}[action], Args: args, RunE: func(cmd *cobra.Command, args []string) error {
			path := "/iam/rolebindings"
			review := api.RoleBindingReview{UID: uid, ResourceVersion: rv}
			if action != "create" {
				var err error
				path, err = roleBindingCLIPath(args)
				if err != nil {
					return err
				}
				if !validRoleBindingReview(review) {
					return errors.New("exact --uid and --resource-version from the reviewed binding are required")
				}
			}
			var body any = review
			method := http.MethodDelete
			if action != "delete" {
				desired, err := readRoleBindingDesired(cmd, file)
				if err != nil {
					return err
				}
				body = desired
				method = http.MethodPost
				if action == "replace" {
					if desired.Metadata.Namespace != args[0] || desired.Metadata.Name != args[1] {
						return errors.New("RoleBinding manifest and explicit target must match")
					}
					body = api.ReplaceRoleBindingCommand{RoleBindingReview: review, Binding: desired}
					method = http.MethodPut
				}
			}
			c, _, err := o.client()
			if err != nil {
				return err
			}
			if action == "delete" {
				if err := c.RoleBindingCommand(cmd.Context(), method, path, body, nil); err != nil {
					return err
				}
				return o.render(cmd, map[string]string{"namespace": args[0], "name": args[1], "status": "deleted"}, nil)
			}
			var out publicv1.RoleBinding
			if err := c.RoleBindingCommand(cmd.Context(), method, path, body, &out); err != nil {
				return err
			}
			return o.render(cmd, out, nil)
		}}
		if action != "delete" {
			sub.Flags().StringVarP(&file, "file", "f", "", "required: one desired RoleBinding YAML/JSON file or - for stdin")
		}
		if action != "create" {
			sub.Flags().StringVar(&uid, "uid", "", "required: exact reviewed target UID")
			sub.Flags().StringVar(&rv, "resource-version", "", "required: exact reviewed target resourceVersion")
		}
		root.AddCommand(sub)
	}
	return root
}
