package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/robertherbaugh/kudurru/internal/apiclient"
	"github.com/robertherbaugh/kudurru/internal/cli"
)

// newGetCmd is `kudurru get <kind> [name...]`.
func newGetCmd(o *cliOpts) *cobra.Command {
	var (
		all      bool
		selector string
	)
	cmd := &cobra.Command{
		Use:   "get <kind> [name...]",
		Short: "List or show objects (dnszones, dnsrecordsets, blocklists, clientgroups, policies)",
		Long: `List or show objects. Kinds and their aliases:

  dnszones       zone, zones, dnszone        (cluster-scoped)
  dnsrecordsets  rrset, rrsets, dnsrecordset
  blocklists     list, lists, blocklist, bl
  clientgroups   cg, clientgroup
  policies       pol, policy`,
		Example: `  kudurru get zones
  kudurru get rrset -A -o wide
  kudurru get blocklist ads -n default -o yaml
  kudurru get pol -l team=platform`,
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: o.completeObjects,
		RunE: func(cmd *cobra.Command, args []string) error {
			k, err := cli.LookupKind(args[0])
			if err != nil {
				return err
			}
			if all && len(args) > 1 && k.Namespaced {
				return fmt.Errorf("a %s cannot be fetched by name across all namespaces; use -n", k.Kind)
			}
			c, r, err := o.client()
			if err != nil {
				return err
			}
			ns := ""
			if k.Namespaced && !all {
				ns = o.nsFor(r)
			}
			var items []json.RawMessage
			single := len(args) == 2
			if len(args) > 1 {
				for _, name := range args[1:] {
					obj, err := c.GetObject(cmd.Context(), k.Resource, ns, name)
					if err != nil {
						return fmt.Errorf("%s %q: %w", k.Resource, name, err)
					}
					items = append(items, obj)
				}
			} else {
				items, err = c.ListObjects(cmd.Context(), k.Resource, apiclient.ListOptions{Namespace: ns, LabelSelector: selector})
				if err != nil {
					return err
				}
			}
			f, err := o.format()
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			switch {
			case f.Structured() && single:
				return cli.WriteStructured(w, f, items[0])
			case f.Structured():
				if items == nil {
					items = []json.RawMessage{}
				}
				return cli.WriteStructured(w, f, map[string]any{"apiVersion": "v1", "kind": "List", "items": items})
			case f == cli.FormatName:
				for _, it := range items {
					m, err := cli.DecodeMeta(it)
					if err != nil {
						return err
					}
					_, _ = fmt.Fprintf(w, "%s/%s\n", k.Resource, m.Metadata.Name)
				}
				return nil
			}
			t, err := cli.ObjectTable(k, items, f == cli.FormatWide, all, o.now())
			if err != nil {
				return err
			}
			if len(t.Rows) == 0 {
				where := ""
				if k.Namespaced && !all {
					where = " in " + ns + " namespace"
				}
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "No resources found%s.\n", where)
				return nil
			}
			return t.Write(w)
		},
	}
	cmd.Flags().BoolVarP(&all, "all-namespaces", "A", false, "list across all namespaces")
	cmd.Flags().StringVarP(&selector, "selector", "l", "", "label selector, e.g. app=web,tier!=db")
	return cmd
}

// completionTimeout bounds API calls made while the shell waits for
// completions.
const completionTimeout = 3 * time.Second

// completeObjects completes the kind, then object names from the API.
func (o *cliOpts) completeObjects(cmd *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) == 0 {
		return cli.KindCompletions(), cobra.ShellCompDirectiveNoFileComp
	}
	k, err := cli.LookupKind(args[0])
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	c, r, err := o.client()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	ns := ""
	if k.Namespaced {
		ns = o.nsFor(r)
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), completionTimeout)
	defer cancel()
	items, err := c.ListObjects(ctx, k.Resource, apiclient.ListOptions{Namespace: ns})
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var names []string
	for _, it := range items {
		if m, err := cli.DecodeMeta(it); err == nil {
			names = append(names, m.Metadata.Name)
		}
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

// newApplyCmd is `kudurru apply -f file|dir|-`.
func newApplyCmd(o *cliOpts) *cobra.Command {
	var (
		files     []string
		recursive bool
	)
	cmd := &cobra.Command{
		Use:   "apply -f file|dir|-",
		Short: "Create or update objects from YAML or JSON manifests",
		Long: `Create or update objects from manifests. Documents may be multi-document YAML,
JSON objects, JSON arrays or a List. Every document is a whole object applied
with manager "api" (POST /api/v1/apply); objects owned by a file or an
integration are refused with "managed_by" and the owner.`,
		Example: `  kudurru apply -f zones.yaml
  kudurru apply -f objects.d/ -R
  cat lists.yaml | kudurru apply -f -`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ms, err := cli.ReadManifests(files, o.stdin, recursive)
			if err != nil {
				return err
			}
			return o.applyManifests(cmd, ms)
		},
	}
	cmd.Flags().StringArrayVarP(&files, "filename", "f", nil, "file, directory or - for stdin (repeatable)")
	cmd.Flags().BoolVarP(&recursive, "recursive", "R", false, "descend into subdirectories of -f directories")
	_ = cmd.MarkFlagRequired("filename")
	return cmd
}

// applyManifests posts manifests to /apply and prints one line per
// document. Namespaced documents without a namespace get the default one.
func (o *cliOpts) applyManifests(cmd *cobra.Command, ms []cli.Manifest) error {
	if len(ms) == 0 {
		return errors.New("no objects found in the given manifests")
	}
	c, r, err := o.client()
	if err != nil {
		return err
	}
	docs := make([]json.RawMessage, 0, len(ms))
	for i := range ms {
		m := &ms[i]
		k, err := cli.LookupKind(m.Meta.Kind)
		if err != nil {
			return fmt.Errorf("%s (document %d): %w", m.Source, m.Index, err)
		}
		if k.Namespaced && m.Meta.Metadata.Namespace == "" {
			var obj map[string]any
			if err := json.Unmarshal(m.Object, &obj); err != nil {
				return err
			}
			meta, _ := obj["metadata"].(map[string]any)
			meta["namespace"] = o.nsFor(r)
			b, err := json.Marshal(obj)
			if err != nil {
				return err
			}
			m.Object, m.Meta.Metadata.Namespace = b, o.nsFor(r)
		}
		docs = append(docs, m.Object)
	}
	results, err := c.Apply(cmd.Context(), docs)
	if err != nil {
		return err
	}
	f, err := o.format()
	if err != nil {
		return err
	}
	if f.Structured() {
		if err := cli.WriteStructured(cmd.OutOrStdout(), f, results); err != nil {
			return err
		}
	}
	failed := 0
	for i, res := range results {
		ref := cli.ObjectRef(firstNonEmpty(res.Kind, kindOf(ms, i)), res.Namespace, res.Name)
		if res.Result == "error" || res.Error != "" {
			failed++
			src := ""
			if i < len(ms) {
				src = fmt.Sprintf(" (%s, document %d)", ms[i].Source, ms[i].Index)
			}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s: error: %s%s\n", ref, res.Message(), src)
			continue
		}
		if !f.Structured() {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", ref, res.Result)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d documents failed", failed, len(results))
	}
	return nil
}

func kindOf(ms []cli.Manifest, i int) string {
	if i < len(ms) {
		return ms[i].Meta.Kind
	}
	return "object"
}

// newDeleteCmd is `kudurru delete <kind> <name...>` or `delete -f`.
func newDeleteCmd(o *cliOpts) *cobra.Command {
	var (
		files          []string
		recursive      bool
		ignoreNotFound bool
	)
	cmd := &cobra.Command{
		Use:   "delete (<kind> <name...> | -f file|dir|-)",
		Short: "Delete objects by name or by manifest",
		Example: `  kudurru delete zone lab-example-com
  kudurru delete rrset web -n apps
  kudurru delete -f zones.yaml`,
		ValidArgsFunction: o.completeObjects,
		RunE: func(cmd *cobra.Command, args []string) error {
			type target struct {
				kind     cli.Kind
				ns, name string
			}
			var targets []target
			c, r, err := o.client()
			if err != nil {
				return err
			}
			switch {
			case len(files) > 0 && len(args) > 0:
				return errors.New("use either <kind> <name...> or -f, not both")
			case len(files) > 0:
				ms, err := cli.ReadManifests(files, o.stdin, recursive)
				if err != nil {
					return err
				}
				for _, m := range ms {
					k, err := cli.LookupKind(m.Meta.Kind)
					if err != nil {
						return err
					}
					ns := ""
					if k.Namespaced {
						ns = firstNonEmpty(m.Meta.Metadata.Namespace, o.nsFor(r))
					}
					targets = append(targets, target{k, ns, m.Meta.Metadata.Name})
				}
			case len(args) >= 2:
				k, err := cli.LookupKind(args[0])
				if err != nil {
					return err
				}
				ns := ""
				if k.Namespaced {
					ns = o.nsFor(r)
				}
				for _, n := range args[1:] {
					targets = append(targets, target{k, ns, n})
				}
			default:
				return errors.New("give a kind and at least one name, or -f")
			}
			var failed int
			for _, t := range targets {
				ref := cli.ObjectRef(t.kind.Kind, t.ns, t.name)
				err := c.DeleteObject(cmd.Context(), t.kind.Resource, t.ns, t.name)
				switch {
				case err == nil:
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s deleted\n", ref)
				case ignoreNotFound && apiclient.IsKey(err, "not_found"):
				default:
					failed++
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", ref, err)
				}
			}
			if failed > 0 {
				return fmt.Errorf("%d of %d deletes failed", failed, len(targets))
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVarP(&files, "filename", "f", nil, "file, directory or - naming the objects to delete")
	cmd.Flags().BoolVarP(&recursive, "recursive", "R", false, "descend into subdirectories of -f directories")
	cmd.Flags().BoolVar(&ignoreNotFound, "ignore-not-found", false, "treat a missing object as deleted")
	return cmd
}

// diagnosis is one entry of GET /info/messages: {severity, source,
// reason, object, message, hint, node}. object is the store key
// ("DNSZone/home", "Policy/dns/kids"); an object form and top-level
// kind/namespace/name are accepted too.
type diagnosis struct {
	Severity  string          `json:"severity"`
	Source    string          `json:"source"`
	Kind      string          `json:"kind"`
	Namespace string          `json:"namespace"`
	Name      string          `json:"name"`
	Reason    string          `json:"reason"`
	Message   string          `json:"message"`
	Hint      string          `json:"hint"`
	Node      string          `json:"node"`
	Object    json.RawMessage `json:"object"`
}

func (d diagnosis) ref() (kind, ns, name string) {
	var key string
	if json.Unmarshal(d.Object, &key) == nil && key != "" {
		parts := strings.Split(key, "/")
		switch len(parts) {
		case 2:
			return parts[0], "", parts[1]
		case 3:
			return parts[0], parts[1], parts[2]
		}
		return "", "", key
	}
	var obj struct{ Kind, Namespace, Name string }
	if json.Unmarshal(d.Object, &obj) == nil && obj.Name != "" {
		return obj.Kind, obj.Namespace, obj.Name
	}
	return d.Kind, d.Namespace, d.Name
}

// newDescribeCmd is `kudurru describe <kind> <name>`.
func newDescribeCmd(o *cliOpts) *cobra.Command {
	return &cobra.Command{
		Use:               "describe <kind> <name>",
		Short:             "Show an object's spec, status conditions and diagnoses",
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: o.completeObjects,
		RunE: func(cmd *cobra.Command, args []string) error {
			k, err := cli.LookupKind(args[0])
			if err != nil {
				return err
			}
			c, r, err := o.client()
			if err != nil {
				return err
			}
			ns := ""
			if k.Namespaced {
				ns = o.nsFor(r)
			}
			raw, err := c.GetObject(cmd.Context(), k.Resource, ns, args[1])
			if err != nil {
				return err
			}
			var diags []diagnosis
			if msgs, err := diagnoses(cmd.Context(), c); err == nil {
				for _, d := range msgs {
					dk, dns, dn := d.ref()
					if dn == args[1] && (dk == "" || strings.EqualFold(dk, k.Kind) || strings.EqualFold(dk, k.Resource)) && (dns == "" || dns == ns) {
						diags = append(diags, d)
					}
				}
			}
			return describe(cmd.OutOrStdout(), k, raw, diags, o)
		},
	}
}

// diagnoses fetches GET /info/messages (list envelope or bare array).
func diagnoses(ctx context.Context, c *apiclient.Client) ([]diagnosis, error) {
	var raw json.RawMessage
	if err := c.Get(ctx, "/info/messages", nil, &raw); err != nil {
		return nil, err
	}
	items, err := rawItems(raw)
	if err != nil || items == nil {
		var env struct {
			Messages []json.RawMessage `json:"messages"`
		}
		if json.Unmarshal(raw, &env) != nil {
			return nil, err
		}
		items = env.Messages
	}
	out := make([]diagnosis, 0, len(items))
	for _, it := range items {
		var d diagnosis
		if json.Unmarshal(it, &d) == nil {
			out = append(out, d)
		}
	}
	return out, nil
}

type describeObject struct {
	cli.Meta
	Spec   json.RawMessage `json:"spec"`
	Status json.RawMessage `json:"status"`
}

func describe(w io.Writer, k cli.Kind, raw json.RawMessage, diags []diagnosis, o *cliOpts) error {
	var obj describeObject
	if err := json.Unmarshal(raw, &obj); err != nil {
		return err
	}
	m := obj.Metadata
	pairs := [][2]string{{"Name", m.Name}}
	if k.Namespaced {
		pairs = append(pairs, [2]string{"Namespace", m.Namespace})
	}
	pairs = append(pairs,
		[2]string{"Kind", firstNonEmpty(obj.Kind, k.Kind)},
		[2]string{"API Version", obj.APIVersion},
		[2]string{"Labels", mapString(m.Labels)},
		[2]string{"Annotations", mapString(m.Annotations)},
		[2]string{"Managed By", cli.ManagedBy(m)},
		[2]string{"Created", createdString(m.CreationTimestamp, o)},
	)
	if m.Generation != 0 {
		pairs = append(pairs, [2]string{"Generation", fmt.Sprint(m.Generation)})
	}
	if m.ResourceVersion != "" {
		pairs = append(pairs, [2]string{"Resource Version", m.ResourceVersion})
	}
	if err := cli.KeyValues(w, "", pairs); err != nil {
		return err
	}
	if err := yamlSection(w, "Spec", obj.Spec); err != nil {
		return err
	}
	var status map[string]json.RawMessage
	_ = json.Unmarshal(obj.Status, &status)
	var conds []metav1.Condition
	if c, ok := status["conditions"]; ok {
		_ = json.Unmarshal(c, &conds)
		delete(status, "conditions")
	}
	if len(status) > 0 {
		b, _ := json.Marshal(status)
		if err := yamlSection(w, "Status", b); err != nil {
			return err
		}
	}
	_, _ = fmt.Fprintln(w, "Conditions:")
	if len(conds) == 0 {
		_, _ = fmt.Fprintln(w, "  <none>")
	} else {
		t := &cli.Table{Headers: []string{"  TYPE", "STATUS", "REASON", "AGE", "MESSAGE"}}
		for _, c := range conds {
			t.Add("  "+c.Type, string(c.Status), c.Reason, cli.Age(c.LastTransitionTime, o.now()), c.Message)
		}
		if err := t.Write(w); err != nil {
			return err
		}
	}
	_, _ = fmt.Fprintln(w, "Diagnoses:")
	if len(diags) == 0 {
		_, _ = fmt.Fprintln(w, "  <none>")
		return nil
	}
	sort.SliceStable(diags, func(i, j int) bool { return severityRank(diags[i].Severity) < severityRank(diags[j].Severity) })
	t := &cli.Table{Headers: []string{"  SEVERITY", "REASON", "MESSAGE", "HINT"}}
	for _, d := range diags {
		t.Add("  "+firstNonEmpty(d.Severity, "info"), firstNonEmpty(d.Reason, "-"), d.Message, firstNonEmpty(d.Hint, "-"))
	}
	return t.Write(w)
}

func severityRank(s string) int {
	switch strings.ToLower(s) {
	case "error", "critical":
		return 0
	case "warning", "warn":
		return 1
	}
	return 2
}

func createdString(t metav1.Time, o *cliOpts) string {
	if t.IsZero() {
		return "<unknown>"
	}
	return t.UTC().Format("2006-01-02T15:04:05Z") + " (" + cli.Age(t, o.now()) + " ago)"
}

func mapString(m map[string]string) string {
	if len(m) == 0 {
		return "<none>"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ",")
}

// yamlSection prints "Title:" and the JSON value as indented YAML.
func yamlSection(w io.Writer, title string, raw json.RawMessage) error {
	_, _ = fmt.Fprintf(w, "%s:\n", title)
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "{}" {
		_, err := fmt.Fprintln(w, "  <none>")
		return err
	}
	y, err := yaml.JSONToYAML(raw)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimRight(string(y), "\n"), "\n") {
		if _, err := fmt.Fprintf(w, "  %s\n", line); err != nil {
			return err
		}
	}
	return nil
}
