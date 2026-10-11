package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"
	"github.com/spf13/cobra"

	"github.com/robertherbaugh/kudurru/internal/apiclient"
	"github.com/robertherbaugh/kudurru/internal/cli"
)

// newListsCmd is `kudurru lists list|refresh|search|allow|deny`.
func newListsCmd(o *cliOpts) *cobra.Command {
	cmd := &cobra.Command{Use: "lists", Short: "Inspect and refresh block and allow lists"}
	var all bool
	list := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List block and allow lists with entry counts and fetch state",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, r, err := o.client()
			if err != nil {
				return err
			}
			q := url.Values{}
			if !all {
				q.Set("namespace", o.nsFor(r))
			}
			items, err := c.ListAll(cmd.Context(), "/blocklists", q, 0)
			if err != nil {
				return err
			}
			k, _ := cli.LookupKind("blocklists")
			return o.render(cmd, items, func(wide bool) (*cli.Table, error) {
				return cli.ObjectTable(k, items, wide, all, o.now())
			})
		},
	}
	list.Flags().BoolVarP(&all, "all-namespaces", "A", false, "list across all namespaces")

	refresh := &cobra.Command{
		Use:   "refresh <name...>",
		Short: "Fetch lists now instead of waiting for their refresh interval",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, r, err := o.client()
			if err != nil {
				return err
			}
			ns := o.nsFor(r)
			for _, name := range args {
				if err := c.Post(cmd.Context(), "/blocklists/"+apiclient.Seg(ns)+"/"+apiclient.Seg(name)+"/refresh", nil, nil, nil); err != nil {
					return fmt.Errorf("refresh %s/%s: %w", ns, name, err)
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "blocklist/%s/%s refresh requested\n", ns, name)
			}
			return nil
		},
	}

	search := &cobra.Command{
		Use:   "search <domain>",
		Short: "Show which lists contain a domain",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := o.client()
			if err != nil {
				return err
			}
			var raw json.RawMessage
			if err := c.Get(cmd.Context(), "/blocklists/_search", url.Values{"domain": {args[0]}}, &raw); err != nil {
				return err
			}
			hits, err := searchHits(raw)
			if err != nil {
				return err
			}
			return o.render(cmd, raw, func(bool) (*cli.Table, error) {
				t := &cli.Table{Headers: []string{"NAMESPACE", "NAME", "MODE", "RULE", "SOURCE"}}
				for _, h := range hits {
					t.Add(firstNonEmpty(h.Namespace, "-"), firstNonEmpty(h.Name, h.List), firstNonEmpty(h.Mode, "-"), firstNonEmpty(h.Rule, "-"), firstNonEmpty(h.Source, "-"))
				}
				return t, nil
			})
		},
	}
	cmd.AddCommand(list, refresh, search, newDomainsCmd(o, "allow"), newDomainsCmd(o, "deny"))
	return cmd
}

// searchHit is one list containing the searched domain.
type searchHit struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	List      string `json:"list"`
	Mode      string `json:"mode"`
	Rule      string `json:"rule"`
	Source    string `json:"source"`
}

func searchHits(raw json.RawMessage) ([]searchHit, error) {
	items, err := rawItems(raw)
	if err != nil || items == nil {
		var env struct {
			Lists   []json.RawMessage `json:"lists"`
			Matches []json.RawMessage `json:"matches"`
		}
		if json.Unmarshal(raw, &env) == nil {
			items = append(env.Lists, env.Matches...)
			err = nil
		}
	}
	if err != nil {
		return nil, err
	}
	out := make([]searchHit, 0, len(items))
	for _, it := range items {
		var h searchHit
		if json.Unmarshal(it, &h) != nil {
			// A bare list name.
			var s string
			if json.Unmarshal(it, &s) != nil {
				continue
			}
			h.Name = s
		}
		out = append(out, h)
	}
	return out, nil
}

// newDomainsCmd adds inline allow/deny entries (the "ui-allow"/"ui-deny"
// lists, created on demand).
func newDomainsCmd(o *cliOpts, verb string) *cobra.Command {
	var (
		regex bool
		list  string
	)
	cmd := &cobra.Command{
		Use:   verb + " <domain...>",
		Short: fmt.Sprintf("Add domains to the %q list", "ui-"+verb),
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, r, err := o.client()
			if err != nil {
				return err
			}
			kind := "exact"
			if regex {
				kind = "regex"
			}
			body := map[string]any{"domains": args, "namespace": o.nsFor(r)}
			if list != "" {
				body["list"] = list
			}
			var res struct {
				Processed struct {
					Success []any `json:"success"`
					Errors  []any `json:"errors"`
				} `json:"processed"`
			}
			if err := c.Post(cmd.Context(), "/domains/"+verb+"/"+kind, nil, body, &res); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%d added to %s\n", len(res.Processed.Success), firstNonEmpty(list, "ui-"+verb))
			for _, e := range res.Processed.Errors {
				b, _ := json.Marshal(e)
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "error: %s\n", b)
			}
			if len(res.Processed.Errors) > 0 {
				return fmt.Errorf("%d domains were rejected", len(res.Processed.Errors))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&regex, "regex", false, "entries are regular expressions")
	cmd.Flags().StringVar(&list, "list", "", "target list (default ui-"+verb+")")
	return cmd
}

// blockingState is GET|POST /dns/blocking.
type blockingState struct {
	Blocking bool       `json:"blocking"`
	Timer    *float64   `json:"timer,omitempty"`
	Until    *time.Time `json:"until,omitempty"`
}

// newBlockingCmd is `kudurru blocking status|enable|disable [5m]`.
func newBlockingCmd(o *cliOpts) *cobra.Command {
	cmd := &cobra.Command{Use: "blocking", Short: "Show or toggle blocking"}
	show := func(cmd *cobra.Command, st blockingState) error {
		f, err := o.format()
		if err != nil {
			return err
		}
		if f.Structured() {
			return cli.WriteStructured(cmd.OutOrStdout(), f, st)
		}
		w := cmd.OutOrStdout()
		if st.Blocking {
			_, err = fmt.Fprintln(w, "blocking: enabled")
			return err
		}
		switch {
		case st.Until != nil && !st.Until.IsZero():
			left := st.Until.Sub(o.now()).Round(time.Second)
			_, err = fmt.Fprintf(w, "blocking: disabled until %s (%s left)\n", st.Until.Format(time.RFC3339), max(left, 0))
		case st.Timer != nil && *st.Timer > 0:
			_, err = fmt.Fprintf(w, "blocking: disabled (%s left)\n", time.Duration(*st.Timer*float64(time.Second)).Round(time.Second))
		default:
			_, err = fmt.Fprintln(w, "blocking: disabled")
		}
		return err
	}
	status := &cobra.Command{
		Use:   "status",
		Short: "Show whether blocking is on",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := o.client()
			if err != nil {
				return err
			}
			var st blockingState
			if err := c.Get(cmd.Context(), "/dns/blocking", nil, &st); err != nil {
				return err
			}
			return show(cmd, st)
		},
	}
	enable := &cobra.Command{
		Use:   "enable",
		Short: "Turn blocking on",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := o.client()
			if err != nil {
				return err
			}
			var st blockingState
			if err := c.Post(cmd.Context(), "/dns/blocking", nil, map[string]any{"blocking": true}, &st); err != nil {
				return err
			}
			return show(cmd, st)
		},
	}
	var until string
	disable := &cobra.Command{
		Use:   "disable [duration]",
		Short: "Turn blocking off, indefinitely or for a duration (30s, 5m, 1h)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]any{"blocking": false}
			if len(args) == 1 {
				secs, err := parseSeconds(args[0])
				if err != nil {
					return err
				}
				body["timer"] = secs
			}
			if until != "" {
				t, err := time.Parse(time.RFC3339, until)
				if err != nil {
					return fmt.Errorf("--until: %w", err)
				}
				body["until"] = t.Format(time.RFC3339)
			}
			c, _, err := o.client()
			if err != nil {
				return err
			}
			var st blockingState
			if err := c.Post(cmd.Context(), "/dns/blocking", nil, body, &st); err != nil {
				return err
			}
			return show(cmd, st)
		},
	}
	disable.Flags().StringVar(&until, "until", "", "re-enable at this RFC 3339 time")
	cmd.AddCommand(status, enable, disable)
	return cmd
}

// newCacheCmd is `kudurru cache flush [name]`.
func newCacheCmd(o *cliOpts) *cobra.Command {
	cmd := &cobra.Command{Use: "cache", Short: "Manage the response cache"}
	cmd.AddCommand(&cobra.Command{
		Use:   "flush [name]",
		Short: "Flush the whole cache, or the entries of one name",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := o.client()
			if err != nil {
				return err
			}
			q := url.Values{}
			what := "cache flushed"
			if len(args) == 1 {
				q.Set("name", args[0])
				what = "cache entries for " + args[0] + " flushed"
			}
			var res map[string]any
			if err := c.Post(cmd.Context(), "/cache/flush", q, nil, &res); err != nil {
				return err
			}
			if n, ok := res["flushed"].(float64); ok {
				what += fmt.Sprintf(" (%d entries)", int(n))
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), what)
			return err
		},
	})
	return cmd
}

// queryResult is POST /dns/query: the answer sections plus kudurru's
// verdict. Records may be presentation strings or {name,ttl,class,type,
// data} objects.
type queryResult struct {
	Name       string            `json:"name"`
	Type       string            `json:"type"`
	Client     string            `json:"client,omitempty"`
	Status     string            `json:"status"`
	Rcode      json.RawMessage   `json:"rcode,omitempty"`
	Upstream   string            `json:"upstream,omitempty"`
	List       string            `json:"list,omitempty"`
	Rule       string            `json:"rule,omitempty"`
	Zone       string            `json:"zone,omitempty"`
	Node       string            `json:"node,omitempty"`
	Flags      json.RawMessage   `json:"flags,omitempty"`
	Answer     []json.RawMessage `json:"answer,omitempty"`
	Authority  []json.RawMessage `json:"authority,omitempty"`
	Additional []json.RawMessage `json:"additional,omitempty"`
	LatencyMS  *float64          `json:"latency_ms,omitempty"`
	TookMS     float64           `json:"took_ms,omitempty"`
}

// newQueryCmd is `kudurru query <name> [type] [--client ip]`: the
// built-in dig running the server's pipeline in-process.
func newQueryCmd(o *cliOpts) *cobra.Command {
	var client string
	cmd := &cobra.Command{
		Use:   "query <name> [type]",
		Short: "Resolve a name through the server's pipeline (dig-like)",
		Long: `Resolve a name through the server's own pipeline (POST /api/v1/dns/query) and
print the answer the way dig does, plus kudurru's verdict: status, upstream,
and the list and rule when a query was blocked. --client evaluates the policy
of another client address.`,
		Example: `  kudurru query example.com
  kudurru query ads.example.net AAAA --client 192.168.1.23`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			qtype := "A"
			if len(args) == 2 {
				qtype = strings.ToUpper(args[1])
			}
			if _, ok := dns.StringToType[qtype]; !ok && !strings.HasPrefix(qtype, "TYPE") {
				return fmt.Errorf("unknown record type %q", qtype)
			}
			c, r, err := o.client()
			if err != nil {
				return err
			}
			body := map[string]string{"name": args[0], "type": qtype}
			if client != "" {
				body["client"] = client
			}
			var raw json.RawMessage
			if err := c.Post(cmd.Context(), "/dns/query", nil, body, &raw); err != nil {
				return err
			}
			f, err := o.format()
			if err != nil {
				return err
			}
			if f.Structured() {
				return cli.WriteStructured(cmd.OutOrStdout(), f, raw)
			}
			var res queryResult
			if err := json.Unmarshal(raw, &res); err != nil {
				return err
			}
			return printDig(cmd.OutOrStdout(), args[0], qtype, res, r.Server)
		},
	}
	cmd.Flags().StringVar(&client, "client", "", "evaluate policy as this client IP")
	return cmd
}

// rcodeString renders an rcode given as a name or a number.
func rcodeString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n int
	if json.Unmarshal(raw, &n) == nil {
		if name, ok := dns.RcodeToString[n]; ok {
			return name
		}
		return strconv.Itoa(n)
	}
	return string(raw)
}

func printDig(w io.Writer, name, qtype string, res queryResult, server string) error {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	rcode := firstNonEmpty(rcodeString(res.Rcode), "NOERROR")
	p(";; ->>HEADER<<- status: %s\n", rcode)
	verdict := []string{"status: " + firstNonEmpty(res.Status, "-")}
	if res.Upstream != "" {
		verdict = append(verdict, "upstream: "+res.Upstream)
	}
	if res.Zone != "" {
		verdict = append(verdict, "zone: "+res.Zone)
	}
	if res.List != "" {
		verdict = append(verdict, "list: "+res.List)
	}
	if res.Rule != "" {
		verdict = append(verdict, "rule: "+res.Rule)
	}
	if res.Client != "" {
		verdict = append(verdict, "client: "+res.Client)
	}
	p(";; kudurru %s\n", strings.Join(verdict, ", "))
	p("\n;; QUESTION SECTION:\n;%s\tIN\t%s\n", dns.Fqdn(firstNonEmpty(res.Name, name)), firstNonEmpty(res.Type, qtype))
	for _, sec := range []struct {
		title string
		rrs   []json.RawMessage
	}{{"ANSWER", res.Answer}, {"AUTHORITY", res.Authority}, {"ADDITIONAL", res.Additional}} {
		if len(sec.rrs) == 0 {
			continue
		}
		p("\n;; %s SECTION:\n", sec.title)
		for _, rr := range sec.rrs {
			p("%s\n", rrString(rr))
		}
	}
	p("\n")
	switch {
	case res.LatencyMS != nil:
		p(";; Query time: %s msec\n", strconv.FormatFloat(*res.LatencyMS, 'f', -1, 64))
	case res.TookMS > 0:
		p(";; Query time: %s msec\n", strconv.FormatFloat(res.TookMS, 'f', -1, 64))
	}
	p(";; SERVER: %s", server)
	if res.Node != "" {
		p(" (node %s)", res.Node)
	}
	p("\n")
	return nil
}

// rrString renders a record from a presentation string or an object.
func rrString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if rr, err := dns.NewRR(s); err == nil && rr != nil {
			return rr.String()
		}
		return s
	}
	var obj struct {
		Name  string `json:"name"`
		TTL   uint32 `json:"ttl"`
		Class string `json:"class"`
		Type  string `json:"type"`
		Data  string `json:"data"`
		RData string `json:"rdata"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil || obj.Name == "" {
		return string(raw)
	}
	return fmt.Sprintf("%s\t%d\t%s\t%s\t%s", dns.Fqdn(obj.Name), obj.TTL, firstNonEmpty(obj.Class, "IN"), obj.Type, firstNonEmpty(obj.Data, obj.RData))
}
