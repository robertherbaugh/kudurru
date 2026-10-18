package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/apiclient"
	"github.com/robertherbaugh/kudurru/internal/cli"
)

// exportOptions control which objects an export holds and how they are
// cleaned.
type exportOptions struct {
	kinds          []cli.Kind
	safe           bool
	excludeManaged bool
}

// redacted replaces secrets removed by --safe.
const redacted = "REDACTED"

// secretParams are URL query parameters whose values --safe redacts.
var secretParams = map[string]bool{
	"token": true, "key": true, "apikey": true, "api_key": true, "secret": true,
	"password": true, "pass": true, "auth": true, "access_token": true, "sig": true,
}

// exportObjects lists every object of the selected kinds across all
// namespaces and returns them cleaned for re-apply: status and server-set
// metadata dropped, the managed-by annotation removed (apply sets its own
// manager).
func exportObjects(cmd *cobra.Command, c *apiclient.Client, opts exportOptions) ([]map[string]any, map[string]int, error) {
	kinds := opts.kinds
	if len(kinds) == 0 {
		kinds = cli.Kinds
	}
	var out []map[string]any
	counts := map[string]int{}
	for _, k := range kinds {
		items, err := c.ListObjects(cmd.Context(), k.Resource, apiclient.ListOptions{})
		if err != nil {
			return nil, nil, fmt.Errorf("list %s: %w", k.Resource, err)
		}
		for _, raw := range items {
			var obj map[string]any
			if err := json.Unmarshal(raw, &obj); err != nil {
				return nil, nil, err
			}
			meta, _ := obj["metadata"].(map[string]any)
			if opts.excludeManaged && meta != nil {
				ann, _ := meta["annotations"].(map[string]any)
				owner, _ := ann[v1alpha1.ManagedByAnnotation].(string)
				if strings.HasPrefix(owner, "file:") || strings.HasPrefix(owner, "integration:") {
					continue
				}
			}
			cleanObject(obj, k)
			if opts.safe {
				redactURLs(obj)
			}
			out = append(out, obj)
			counts[k.Resource]++
		}
	}
	return out, counts, nil
}

// cleanObject strips what the server owns so the object re-applies cleanly.
func cleanObject(obj map[string]any, k cli.Kind) {
	delete(obj, "status")
	if obj["apiVersion"] == nil {
		obj["apiVersion"] = v1alpha1.GroupVersion.String()
	}
	if obj["kind"] == nil {
		obj["kind"] = k.Kind
	}
	meta, _ := obj["metadata"].(map[string]any)
	if meta == nil {
		return
	}
	for _, f := range []string{"uid", "resourceVersion", "generation", "creationTimestamp", "managedFields", "deletionTimestamp", "deletionGracePeriodSeconds", "selfLink", "ownerReferences", "finalizers"} {
		delete(meta, f)
	}
	if ann, ok := meta["annotations"].(map[string]any); ok {
		delete(ann, v1alpha1.ManagedByAnnotation)
		delete(ann, "kubectl.kubernetes.io/last-applied-configuration")
		if len(ann) == 0 {
			delete(meta, "annotations")
		}
	}
	if l, ok := meta["labels"].(map[string]any); ok && len(l) == 0 {
		delete(meta, "labels")
	}
}

// redactURLs walks v and replaces passwords in URL userinfo and secret
// query parameters (list sources behind a token, for example).
func redactURLs(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if s, ok := x.(string); ok {
				t[k] = redactURL(s)
				continue
			}
			redactURLs(x)
		}
	case []any:
		for i, x := range t {
			if s, ok := x.(string); ok {
				t[i] = redactURL(s)
				continue
			}
			redactURLs(x)
		}
	}
}

func redactURL(s string) string {
	if !strings.Contains(s, "://") {
		return s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return s
	}
	changed := false
	if _, has := u.User.Password(); has {
		u.User = url.UserPassword(u.User.Username(), redacted)
		changed = true
	}
	q := u.Query()
	for k := range q {
		if secretParams[strings.ToLower(k)] {
			q.Set(k, redacted)
			changed = true
		}
	}
	if !changed {
		return s
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// writeYAMLDocs writes objects as a multi-document YAML stream.
func writeYAMLDocs(w io.Writer, objs []map[string]any) error {
	for _, obj := range objs {
		b, err := yaml.Marshal(obj)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "---\n%s", b); err != nil {
			return err
		}
	}
	return nil
}

func parseKinds(args []string) ([]cli.Kind, error) {
	var ks []cli.Kind
	for _, a := range args {
		k, err := cli.LookupKind(a)
		if err != nil {
			return nil, err
		}
		ks = append(ks, k)
	}
	return ks, nil
}

func newExportCmd(o *cliOpts) *cobra.Command {
	var opts exportOptions
	cmd := &cobra.Command{
		Use:               "export [kind...]",
		Short:             "Print objects as re-appliable YAML (all kinds by default)",
		ValidArgsFunction: completeKinds,
		RunE: func(cmd *cobra.Command, args []string) error {
			ks, err := parseKinds(args)
			if err != nil {
				return err
			}
			opts.kinds = ks
			c, _, err := o.client()
			if err != nil {
				return err
			}
			objs, _, err := exportObjects(cmd, c, opts)
			if err != nil {
				return err
			}
			f, err := o.format()
			if err != nil {
				return err
			}
			if f == cli.FormatJSON {
				if objs == nil {
					objs = []map[string]any{}
				}
				return cli.WriteStructured(cmd.OutOrStdout(), f, objs)
			}
			return writeYAMLDocs(cmd.OutOrStdout(), objs)
		},
	}
	cmd.Flags().BoolVar(&opts.safe, "safe", false, "redact credentials embedded in URLs")
	cmd.Flags().BoolVar(&opts.excludeManaged, "exclude-managed", false, "skip objects owned by files or integrations")
	return cmd
}

func newBackupCmd(o *cliOpts) *cobra.Command {
	var (
		out  string
		opts exportOptions
	)
	cmd := &cobra.Command{
		Use:   "backup --out file.yaml [--safe]",
		Short: "Save every object to a YAML file that `kudurru restore` re-applies",
		Long: `Save every object (zones, record sets, lists, client groups, policies) as a
multi-document YAML file. Status and server-set metadata are dropped so the
file re-applies cleanly with ` + "`kudurru restore -f`" + `. --safe redacts credentials
embedded in URLs (user:password@ and token/key/secret query parameters) so the
file can be shared; such a backup needs the secrets filled in before restore.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, r, err := o.client()
			if err != nil {
				return err
			}
			objs, counts, err := exportObjects(cmd, c, opts)
			if err != nil {
				return err
			}
			var buf bytes.Buffer
			fmt.Fprintf(&buf, "# kudurru backup\n# server: %s\n# created: %s\n", r.Server, o.now().UTC().Format(time.RFC3339))
			var parts []string
			for _, k := range cli.Kinds {
				if n := counts[k.Resource]; n > 0 {
					parts = append(parts, fmt.Sprintf("%s %d", k.Resource, n))
				}
			}
			fmt.Fprintf(&buf, "# objects: %d (%s)\n", len(objs), strings.Join(parts, ", "))
			if opts.safe {
				buf.WriteString("# safe: credentials in URLs are REDACTED\n")
			}
			buf.WriteString("# restore: kudurru restore -f <this file>\n")
			if err := writeYAMLDocs(&buf, objs); err != nil {
				return err
			}
			if out == "-" {
				_, err := cmd.OutOrStdout().Write(buf.Bytes())
				return err
			}
			if err := os.WriteFile(out, buf.Bytes(), 0o600); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.ErrOrStderr(), "%d objects written to %s\n", len(objs), out)
			return err
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "file to write (- for stdout)")
	cmd.Flags().BoolVar(&opts.safe, "safe", false, "redact credentials embedded in URLs")
	cmd.Flags().BoolVar(&opts.excludeManaged, "exclude-managed", false, "skip objects owned by files or integrations")
	_ = cmd.MarkFlagRequired("out")
	return cmd
}

func newRestoreCmd(o *cliOpts) *cobra.Command {
	var files []string
	cmd := &cobra.Command{
		Use:   "restore -f file.yaml",
		Short: "Re-apply a backup (POST /apply; per-object results)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ms, err := cli.ReadManifests(files, o.stdin, false)
			if err != nil {
				return err
			}
			for _, m := range ms {
				if bytes.Contains(m.Object, []byte(redacted)) {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s contains REDACTED values (a --safe backup)\n", m.Ref())
				}
			}
			return o.applyManifests(cmd, ms)
		},
	}
	cmd.Flags().StringArrayVarP(&files, "filename", "f", nil, "backup file, or - for stdin")
	_ = cmd.MarkFlagRequired("filename")
	return cmd
}
