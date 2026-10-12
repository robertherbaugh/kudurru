package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
	"github.com/robertherbaugh/kudurru/internal/domainlist"
)

// SourceEntry is one entry of a BlockList source.
type SourceEntry struct {
	Value   string `json:"value" doc:"inline: the line as written; url and configMap: the rule as the source's format normalises it (one item per host of a hosts line); entries: the rendered rule."`
	Line    int    `json:"line" doc:"1-based line of the body (inline: position in spec.sources[].inline; entries: position in spec.sources[].entries)."`
	Enabled bool   `json:"enabled" doc:"false when switched off (spec.sources[].exclude, or enabled: false of a structured entry)."`
}

// SourceEntries is GET /blocklists/{namespace}/{name}/sources/{source}/entries.
type SourceEntries struct {
	Items    []SourceEntry `json:"items" nullable:"false"`
	Continue string        `json:"continue,omitempty" doc:"Token for the next page; empty on the last page."`
	Total    int           `json:"total" doc:"Entries in the source."`
	Matched  int           `json:"matched" doc:"Entries matching q and enabled."`
	Excluded int           `json:"excluded" doc:"Entries switched off."`
	// StaleExcludes are exclude values that match no entry of the body.
	StaleExcludes []string   `json:"staleExcludes" nullable:"false" doc:"spec.sources[].exclude values that match no entry of the current body."`
	Kind          string     `json:"kind" enum:"inline,url,configMap,entries"`
	Digest        string     `json:"digest,omitempty" doc:"url and configMap: sha256 of the body this node holds."`
	Fetched       *time.Time `json:"fetched,omitempty" doc:"url and configMap: when this node's body was fetched."`
	Stale         bool       `json:"stale" doc:"This node's body is not the one status.sources[].digest publishes yet."`
	Took
}

type sourceEntriesInput struct {
	Namespace string `path:"namespace"`
	Name      string `path:"name"`
	Source    string `path:"source" doc:"Source name or index."`
	Q         string `query:"q" doc:"Substring filter, case-insensitive."`
	Enabled   string `query:"enabled" enum:"true,false" doc:"Only entries switched on (true) or off (false)."`
	Limit     int    `query:"limit" minimum:"0" maximum:"1000" doc:"Page size (default 100)."`
	Continue  string `query:"continue"`
}

type sourceEntriesOutput struct {
	ETag string `header:"ETag" doc:"The resourceVersion of the BlockList."`
	Body *SourceEntries
}

// SourceEntriesPatch is the body of PATCH …/entries.
type SourceEntriesPatch struct {
	Add     []string `json:"add,omitempty" maxItems:"10000" doc:"Inline sources only: lines to add."`
	Remove  []string `json:"remove,omitempty" maxItems:"10000" doc:"Inline sources only: lines to remove."`
	Enable  []string `json:"enable,omitempty" maxItems:"10000" doc:"Entries (values as GET lists them) to switch back on."`
	Disable []string `json:"disable,omitempty" maxItems:"10000" doc:"Entries to switch off: added to spec.sources[].exclude (enabled: false for structured entries)."`
}

// SourceEntriesResult is the answer of PATCH …/entries.
type SourceEntriesResult struct {
	// Added, Removed, Enabled and Disabled are the values that changed
	// the source.
	Added    []string `json:"added" nullable:"false"`
	Removed  []string `json:"removed" nullable:"false"`
	Enabled  []string `json:"enabled" nullable:"false"`
	Disabled []string `json:"disabled" nullable:"false"`
	Total    int      `json:"total" doc:"Inline lines or structured entries after the change (0 for url and configMap sources)."`
	Excluded int      `json:"excluded" doc:"Exclude values, or structured entries switched off, after the change."`
	// ResourceVersion is the BlockList's new resourceVersion (also the ETag).
	ResourceVersion string `json:"resourceVersion"`
	Took
}

type patchSourceEntriesInput struct {
	Namespace string `path:"namespace"`
	Name      string `path:"name"`
	Source    string `path:"source" doc:"Source name or index."`
	IfMatch   string `header:"If-Match" doc:"Apply only when the stored resourceVersion equals this value (412 otherwise)."`
	Body      SourceEntriesPatch
}

type patchSourceEntriesOutput struct {
	ETag string `header:"ETag"`
	Body *SourceEntriesResult
}

// maxInlineEntryBytes bounds one added entry.
const maxInlineEntryBytes = 1024

func (s *Server) registerSourceEntries() {
	entriesPath := "/blocklists/{namespace}/{name}/sources/{source}/entries"
	get := operation(http.MethodGet, entriesPath, "listSourceEntries", "Page through the entries of a BlockList source", "blocklists")
	get.Description = "Every source type: the lines of an inline source, the structured entries of an entries source, and for URL and ConfigMap sources the rules of the body this node's list cache holds " +
		"(the leader fetches it, replicas pull it by digest; 503 while this node has none). Each item says whether it is switched on. " +
		"{source} is the source's name (spec.sources[].name, else the name status.sources[] reports) or its index. q filters by substring (case-insensitive), enabled by switch. " +
		"The ETag header is the object's resourceVersion for If-Match on PATCH."
	get.Errors = []int{http.StatusServiceUnavailable}
	register(s, get, perm{resource: "blocklists", verb: "read"}, s.listSourceEntries)
	patch := operation(http.MethodPatch, entriesPath, "patchSourceEntries", "Switch entries of a BlockList source off and on, add and remove inline entries", "blocklists")
	patch.Description = "Edits the source through the same write path as PUT /blocklists/{namespace}/{name}: If-Match, the managedBy rules and validation apply " +
		"(a bootstrap-managed list such as the starter list is taken over, D84). enable and disable work for every source type: they edit spec.sources[].exclude, " +
		"each value normalised to the rule(s) the source's format reads from it, or the enabled switch of structured entries. Switching an entry off affects only this list; " +
		"to allow a name everywhere use the Allowlist. add and remove edit spec.sources[].inline and are for inline sources only; " +
		"entries already present are not added twice and removals of absent entries are ignored."
	patch.Errors = []int{http.StatusPreconditionFailed, http.StatusUnprocessableEntity}
	register(s, patch, perm{resource: "blocklists", verb: "write"}, s.patchSourceEntries)
}

// findSource returns the index of the source of bl named source: by its
// name (the status.sources[] name), else by index.
func findSource(bl *v1alpha1.BlockList, source string) (int, error) {
	for i, src := range bl.Spec.Sources {
		if blocking.SourceName(i, src) == source {
			return i, nil
		}
	}
	if n, err := strconv.Atoi(source); err == nil && n >= 0 && n < len(bl.Spec.Sources) {
		return n, nil
	}
	return 0, errNotFound(fmt.Sprintf("source %q of BlockList %s/%s", source, bl.Namespace, bl.Name))
}

// sourceKind names the kind of a source.
func sourceKind(src *v1alpha1.ListSource) string {
	switch {
	case src.URL != "":
		return "url"
	case src.ConfigMapRef != nil:
		return "configMap"
	case len(src.Entries) > 0:
		return "entries"
	}
	return "inline"
}

func (s *Server) getBlockList(ctx context.Context, ns, name string) (*v1alpha1.BlockList, error) {
	k, _ := kindByPlural("blocklists")
	obj, err := s.getObject(ctx, k, ns, name)
	if err != nil {
		return nil, err
	}
	bl, ok := obj.(*v1alpha1.BlockList)
	if !ok {
		return nil, errNotFound(ns + "/" + name)
	}
	return bl, nil
}

// entryPager collects one page of filtered entries.
type entryPager struct {
	q       string
	enabled string
	start   int
	limit   int
	out     *SourceEntries
}

func (p *entryPager) add(e SourceEntry) {
	p.out.Total++
	if !e.Enabled {
		p.out.Excluded++
	}
	if p.q != "" && !strings.Contains(strings.ToLower(e.Value), p.q) || p.enabled != "" && strconv.FormatBool(e.Enabled) != p.enabled {
		return
	}
	idx := p.out.Matched
	p.out.Matched++
	if idx < p.start {
		return
	}
	if len(p.out.Items) == p.limit {
		if p.out.Continue == "" {
			p.out.Continue = encodeCursor(strconv.Itoa(idx))
		}
		return
	}
	p.out.Items = append(p.out.Items, e)
}

// excludeSet tracks which exclude values matched.
type excludeSet map[string]bool

func newExcludeSet(values []string) excludeSet {
	ex := excludeSet{}
	for _, v := range values {
		ex[v] = false
	}
	return ex
}

// hit reports whether rule is excluded and marks it matched.
func (ex excludeSet) hit(rule string) bool {
	if _, ok := ex[rule]; ok {
		ex[rule] = true
		return true
	}
	return false
}

func (ex excludeSet) stale() []string {
	out := []string{}
	for v, hit := range ex {
		if !hit {
			out = append(out, v)
		}
	}
	slices.Sort(out)
	return out
}

func (s *Server) listSourceEntries(ctx context.Context, in *sourceEntriesInput) (*sourceEntriesOutput, error) {
	bl, err := s.getBlockList(ctx, in.Namespace, in.Name)
	if err != nil {
		return nil, err
	}
	idx, err := findSource(bl, in.Source)
	if err != nil {
		return nil, err
	}
	start := 0
	if pos, err := decodeCursor(in.Continue); err != nil {
		return nil, err
	} else if pos != "" {
		if start, err = strconv.Atoi(pos); err != nil || start < 0 {
			return nil, errInvalid("continue: not a token returned by this API")
		}
	}
	src := &bl.Spec.Sources[idx]
	out := &SourceEntries{Items: []SourceEntry{}, StaleExcludes: []string{}, Kind: sourceKind(src)}
	pg := &entryPager{q: strings.ToLower(strings.TrimSpace(in.Q)), enabled: in.Enabled, start: start, limit: clampLimit(in.Limit, 100, 1000), out: out}
	ex := newExcludeSet(src.Exclude)
	switch out.Kind {
	case "entries":
		for i, e := range src.Entries {
			pg.add(SourceEntry{Value: v1alpha1.EntryRule(e.Type, e.Value), Line: i + 1, Enabled: e.IsEnabled()})
		}
	case "inline":
		// One parse of the whole source maps every rule to its line; a
		// line is off when every rule it gives is excluded.
		rules := make([]int, len(src.Inline))
		off := make([]int, len(src.Inline))
		_, err := parser.Parse(strings.NewReader(strings.Join(src.Inline, "\n")), parser.Options{Format: parser.Format(src.Format), HostsSubdomains: src.Subdomains, KeepInvalid: -1, KeepUnsupported: -1},
			func(r parser.Rule) error {
				if i := r.Line - 1; i >= 0 && i < len(rules) {
					rules[i]++
					if ex.hit(r.String()) {
						off[i]++
					}
				}
				return nil
			})
		if err != nil {
			return nil, errInvalid("source %q: %v", in.Source, err)
		}
		for i, line := range src.Inline {
			pg.add(SourceEntry{Value: line, Line: i + 1, Enabled: rules[i] == 0 || off[i] < rules[i]})
		}
		out.StaleExcludes = ex.stale()
	default:
		if err := s.scanSourceBody(bl, src, idx, pg, ex); err != nil {
			return nil, err
		}
		out.StaleExcludes = ex.stale()
	}
	return &sourceEntriesOutput{ETag: bl.ResourceVersion, Body: out}, nil
}

// errNoBody is the 503 of a URL or ConfigMap source this node holds no
// body of.
func errNoBody(source string) *Error {
	return newError(http.StatusServiceUnavailable, KeyUnavailable,
		fmt.Sprintf("the fetched body of source %q is not on this node yet", source),
		"the leader fetches list sources and every node pulls them by digest: retry once status.sources[].digest is set, or ask the leader")
}

// scanSourceBody pages through the rules of the cached body of a URL or
// ConfigMap source.
func (s *Server) scanSourceBody(bl *v1alpha1.BlockList, src *v1alpha1.ListSource, idx int, pg *entryPager, ex excludeSet) error {
	name := blocking.SourceName(idx, *src)
	if s.opts.ListBodies == nil {
		return errNoBody(name)
	}
	rc, body, err := s.opts.ListBodies.OpenSourceBody(bl.Namespace, *src)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return errNoBody(name)
	case err != nil:
		return errUnavailable("source %q: %v", name, err)
	}
	defer func() { _ = rc.Close() }()
	pg.out.Digest = body.Digest
	if !body.Fetched.IsZero() {
		t := body.Fetched.UTC()
		pg.out.Fetched = &t
	}
	for _, st := range bl.Status.Sources {
		if st.Name == name && st.Digest != "" && st.Digest != body.Digest {
			pg.out.Stale = true
		}
	}
	_, err = parser.Parse(rc, parser.Options{Format: parser.Format(src.Format), HostsSubdomains: src.Subdomains, KeepInvalid: -1, KeepUnsupported: -1}, func(r parser.Rule) error {
		v := r.String()
		pg.add(SourceEntry{Value: v, Line: r.Line, Enabled: !ex.hit(v)})
		return nil
	})
	if err != nil && !errors.Is(err, io.EOF) {
		return errUnavailable("source %q: %v", name, err)
	}
	return nil
}

// checkInlineEntry validates one entry to add: a single non-blank line
// that parses as a rule of format (comments are kept as written).
func checkInlineEntry(e string, format parser.Format) error {
	switch {
	case strings.TrimSpace(e) == "":
		return errInvalid("add: empty entry")
	case strings.ContainsAny(e, "\r\n"):
		return errInvalid("add: %q contains a line break; send one entry per item", e)
	case len(e) > maxInlineEntryBytes:
		return errInvalid("add: an entry is longer than %d bytes", maxInlineEntryBytes)
	}
	if format == "" {
		format = parser.FormatAuto
	}
	res, err := parser.Parse(strings.NewReader(e), parser.Options{Format: format, KeepInvalid: 1, KeepUnsupported: -1}, nil)
	if err != nil {
		return errInvalid("add: %q: %v", e, err)
	}
	if res.Invalid > 0 {
		reason := "not a valid rule"
		if len(res.InvalidSamples) > 0 {
			reason = res.InvalidSamples[0].Reason
		}
		return errInvalid("add: %q is not a valid %s entry: %s", e, format, reason)
	}
	return nil
}

// excludeRules normalises a value to switch on or off to the rules the
// source's format reads from it ("0.0.0.0 a b" gives "a" and "b").
func excludeRules(src *v1alpha1.ListSource, value string) ([]string, error) {
	if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\r\n") || len(value) > maxInlineEntryBytes {
		return nil, errInvalid("%q is not one entry", value)
	}
	var out []string
	_, err := parser.Parse(strings.NewReader(value), parser.Options{Format: parser.Format(src.Format), HostsSubdomains: src.Subdomains, KeepInvalid: -1, KeepUnsupported: -1}, func(r parser.Rule) error {
		out = append(out, r.String())
		return nil
	})
	if err != nil || len(out) == 0 {
		return nil, errInvalid("%q is not an entry of this source's format (as GET …/entries lists them)", value)
	}
	return out, nil
}

// patchAttempts bounds the read-modify-write retries of a PATCH without
// If-Match that loses a race with another writer.
const patchAttempts = 3

func (s *Server) patchSourceEntries(ctx context.Context, in *patchSourceEntriesInput) (*patchSourceEntriesOutput, error) {
	b := &in.Body
	if len(b.Add) == 0 && len(b.Remove) == 0 && len(b.Enable) == 0 && len(b.Disable) == 0 {
		return nil, errInvalid("body: add, remove, enable or disable at least one entry")
	}
	k, _ := kindByPlural("blocklists")
	for attempt := 1; ; attempt++ {
		bl, err := s.getBlockList(ctx, in.Namespace, in.Name)
		if err != nil {
			return nil, err
		}
		idx, err := findSource(bl, in.Source)
		if err != nil {
			return nil, err
		}
		if in.IfMatch != "" && in.IfMatch != bl.ResourceVersion {
			return nil, errPrecondition(in.IfMatch, bl.ResourceVersion)
		}
		src := &bl.Spec.Sources[idx]
		res := &SourceEntriesResult{Added: []string{}, Removed: []string{}, Enabled: []string{}, Disabled: []string{}}
		if err := applySourcePatch(src, b, res, s.opts.Now); err != nil {
			return nil, err
		}
		if len(res.Added)+len(res.Removed)+len(res.Enabled)+len(res.Disabled) == 0 {
			res.ResourceVersion = bl.ResourceVersion
			return &patchSourceEntriesOutput{ETag: bl.ResourceVersion, Body: res}, nil
		}
		body, err := typedObject(bl)
		if err != nil {
			return nil, storeError(err)
		}
		wr, err := s.writeObject(ctx, writeRequest{kind: k, body: body, namespace: in.Namespace, name: in.Name, ifMatch: bl.ResourceVersion})
		var apiErr *Error
		lostRace := errors.As(err, &apiErr) && (apiErr.Status == http.StatusPreconditionFailed || apiErr.Body.Key == KeyConflict)
		if lostRace && in.IfMatch == "" && attempt < patchAttempts {
			continue // another writer won the race: re-read and re-apply
		}
		if err != nil {
			return nil, err
		}
		res.ResourceVersion = wr.obj.GetResourceVersion()
		for _, v := range append(slices.Clone(res.Enabled), res.Disabled...) {
			if t, name, ok := domainlist.FromRule(v, parser.Format(src.Format)); ok {
				s.flushName(domainlist.MatchName(t, name))
			}
		}
		return &patchSourceEntriesOutput{ETag: res.ResourceVersion, Body: res}, nil
	}
}

// applySourcePatch edits src in memory and records what changed in res.
func applySourcePatch(src *v1alpha1.ListSource, b *SourceEntriesPatch, res *SourceEntriesResult, now func() time.Time) error {
	kind := sourceKind(src)
	if (len(b.Add) > 0 || len(b.Remove) > 0) && kind != "inline" {
		hint := "edit the list at its origin, or switch single entries off with disable"
		if kind == "entries" {
			hint = "structured entries are edited through /allowlist and /denylist"
		}
		return newError(http.StatusUnprocessableEntity, KeyUnprocessable, fmt.Sprintf("add and remove are for inline sources; this source is %s", kind), hint)
	}
	if kind == "entries" {
		t := now().UTC().Truncate(time.Second)
		// Each entry's rule key once: up to 10000 values against up to
		// 10000 entries, each key a parse, would otherwise take minutes.
		byKey := make(map[string]int, len(src.Entries))
		for i, e := range src.Entries {
			if k := domainlist.RuleKey(v1alpha1.EntryRule(e.Type, e.Value)); k != "" {
				if _, dup := byKey[k]; !dup {
					byKey[k] = i
				}
			}
		}
		for _, list := range []struct {
			values []string
			on     bool
			dst    *[]string
		}{{b.Enable, true, &res.Enabled}, {b.Disable, false, &res.Disabled}} {
			for _, v := range list.values {
				i, ok := byKey[domainlist.RuleKey(v)]
				if !ok {
					return errInvalid("%q is not an entry of this source", v)
				}
				e := &src.Entries[i]
				if e.IsEnabled() == list.on {
					continue
				}
				e.Enabled = nil
				if !list.on {
					off := false
					e.Enabled = &off
				}
				mt := metav1.NewTime(t)
				e.Modified = &mt
				*list.dst = append(*list.dst, v)
			}
		}
		res.Total = len(src.Entries)
		for _, e := range src.Entries {
			if !e.IsEnabled() {
				res.Excluded++
			}
		}
		return nil
	}
	// enable / disable through spec.sources[].exclude, kept as a set
	// while the values are applied (linear in values and excludes).
	excluded := make(map[string]bool, len(src.Exclude))
	for _, e := range src.Exclude {
		excluded[e] = true
	}
	for _, v := range b.Disable {
		rules, err := excludeRules(src, v)
		if err != nil {
			return err
		}
		changed := false
		for _, r := range rules {
			if !excluded[r] {
				excluded[r] = true
				src.Exclude = append(src.Exclude, r)
				changed = true
			}
		}
		if changed {
			res.Disabled = append(res.Disabled, v)
		}
	}
	reenable := map[string]bool{}
	for _, v := range b.Enable {
		keys := []string{v}
		if rules, err := excludeRules(src, v); err == nil {
			keys = append(keys, rules...)
		}
		changed := false
		for _, k := range keys {
			if excluded[k] {
				delete(excluded, k)
				reenable[k], changed = true, true
			}
		}
		if changed {
			res.Enabled = append(res.Enabled, v)
		}
	}
	if len(reenable) > 0 {
		src.Exclude = slices.DeleteFunc(src.Exclude, func(e string) bool { return reenable[e] })
	}
	if len(src.Exclude) == 0 {
		src.Exclude = nil
	}
	res.Excluded = len(src.Exclude)
	if kind != "inline" {
		return nil
	}
	drop := map[string]bool{}
	for _, e := range b.Remove {
		drop[e] = true
	}
	kept := make([]string, 0, len(src.Inline)+len(b.Add))
	have := make(map[string]bool, len(src.Inline))
	for _, e := range src.Inline {
		if drop[e] {
			res.Removed = append(res.Removed, e)
			continue
		}
		kept = append(kept, e)
		have[e] = true
	}
	for _, e := range b.Add {
		if err := checkInlineEntry(e, parser.Format(src.Format)); err != nil {
			return err
		}
		if have[e] || drop[e] {
			continue
		}
		have[e] = true
		kept = append(kept, e)
		res.Added = append(res.Added, e)
	}
	res.Total = len(kept)
	if len(res.Added)+len(res.Removed) > 0 {
		if len(kept) == 0 {
			return newError(http.StatusUnprocessableEntity, KeyUnprocessable,
				"an inline source needs at least one entry", "switch the source off (spec.sources[].enabled: false) or delete it")
		}
		src.Inline = kept
	}
	return nil
}
