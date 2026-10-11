package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// UI list names: POST /domains/{allow|deny}/... appends to these
// BlockLists, created on demand in the request's namespace.
const (
	UIAllowList = "ui-allow"
	UIDenyList  = "ui-deny"
	// uiSource is the inline source the UI lists keep their entries in.
	uiSource = "ui"
)

type nsListInput struct {
	Namespace     string `query:"namespace"`
	LabelSelector string `query:"labelSelector"`
	Limit         int    `query:"limit" minimum:"0" maximum:"1000"`
	Continue      string `query:"continue"`
}

type nsCreateInput struct {
	Body Object
}

type nsObjectInput struct {
	Namespace string `path:"namespace"`
	Name      string `path:"name"`
}

type nsPutInput struct {
	Namespace string `path:"namespace"`
	Name      string `path:"name"`
	IfMatch   string `header:"If-Match"`
	Body      Object
}

type nsDeleteInput struct {
	Namespace string `path:"namespace"`
	Name      string `path:"name"`
	IfMatch   string `header:"If-Match"`
}

// ListSearchHit is one list that contains a domain.
type ListSearchHit struct {
	// List is "<namespace>/<name>".
	List string `json:"list"`
	Mode string `json:"mode"`
	// Rule is the matching rule as list text (||ads.example^).
	Rule string `json:"rule"`
}

type searchInput struct {
	Domain string `query:"domain" required:"true"`
}

type searchOutput struct {
	Body *List[ListSearchHit]
}

// DomainsRequest is the body of POST /domains/{action}/{kind}.
type DomainsRequest struct {
	Domains []string `json:"domains" minItems:"1" maxItems:"1000"`
	// Namespace of the list (default "default").
	Namespace string `json:"namespace,omitempty"`
	// List overrides the list name (default ui-allow / ui-deny).
	List string `json:"list,omitempty"`
}

type domainsInput struct {
	Action string `path:"action" enum:"allow,deny"`
	Kind   string `path:"kind" enum:"exact,regex"`
	Body   DomainsRequest
}

type refreshOutput struct {
	Status int
	Body   *refreshBody
}

type refreshBody struct {
	List string `json:"list"`
	Took
}

// registerLists registers /blocklists, /clientgroups and /policies (CRUD
// mirroring /objects), list refresh and search, and the multi-add
// /domains routes.
func (s *Server) registerLists() {
	for _, plural := range []string{"blocklists", "clientgroups", "policies"} {
		k, _ := kindByPlural(plural)
		tag := plural
		name := exportName(plural)
		register(s, operation(http.MethodGet, "/"+plural, "list"+name, "List "+plural, tag), perm{resource: k.resource, verb: "read"},
			func(ctx context.Context, in *nsListInput) (*objectListOutput, error) {
				return s.listObjects(ctx, &listObjectsInput{Kind: k.plural, Namespace: in.Namespace, LabelSelector: in.LabelSelector, Limit: in.Limit, Continue: in.Continue})
			})
		register(s, withStatus(operation(http.MethodPost, "/"+plural, "create"+name, "Create a "+string(k.kind), tag), http.StatusCreated), perm{resource: k.resource, verb: "write"},
			func(ctx context.Context, in *nsCreateInput) (*objectOutput, error) {
				return s.putObjectRoute(ctx, k, writeRequest{kind: k, body: in.Body, createOnly: true})
			})
		register(s, operation(http.MethodGet, "/"+plural+"/{namespace}/{name}", "get"+name, "Get a "+string(k.kind), tag), perm{resource: k.resource, verb: "read"},
			func(ctx context.Context, in *nsObjectInput) (*objectOutput, error) {
				return s.getObjectRoute(ctx, k.plural, in.Namespace, in.Name, true)
			})
		register(s, operation(http.MethodPut, "/"+plural+"/{namespace}/{name}", "put"+name, "Create or replace a "+string(k.kind), tag), perm{resource: k.resource, verb: "write"},
			func(ctx context.Context, in *nsPutInput) (*objectOutput, error) {
				return s.putObjectRoute(ctx, k, writeRequest{kind: k, body: in.Body, namespace: in.Namespace, name: in.Name, ifMatch: in.IfMatch})
			})
		register(s, withStatus(operation(http.MethodDelete, "/"+plural+"/{namespace}/{name}", "delete"+name, "Delete a "+string(k.kind), tag), http.StatusNoContent), perm{resource: k.resource, verb: "delete"},
			func(ctx context.Context, in *nsDeleteInput) (*emptyOutput, error) {
				return s.deleteObjectRoute(ctx, k.plural, in.Namespace, in.Name, in.IfMatch, true)
			})
	}
	register(s, withStatus(operation(http.MethodPost, "/blocklists/{namespace}/{name}/refresh", "refreshBlockList", "Fetch a BlockList's sources now", "blocklists"), http.StatusAccepted),
		perm{resource: "blocklists", verb: "refresh"}, s.refreshList)
	register(s, operation(http.MethodGet, "/blocklists/_search", "searchBlockLists", "Which lists contain a domain", "blocklists"),
		perm{resource: "blocklists", verb: "read"}, s.searchLists)
	register(s, operation(http.MethodPost, "/domains/{action}/{kind}", "addDomains", "Allow or deny domains through the UI lists", "blocklists"),
		perm{resource: "blocklists", verb: "write"}, s.addDomains)
	entriesPath := "/blocklists/{namespace}/{name}/sources/{source}/entries"
	getEntries := operation(http.MethodGet, entriesPath, "listInlineEntries", "Page through the entries of an inline BlockList source", "blocklists")
	getEntries.Description = "The entries of spec.sources[].inline, one per line, so a client never round-trips a long inline source. " +
		"{source} is the source's name (spec.sources[].name, else the name status.sources[] reports) or its index. q filters by substring (case-insensitive). " +
		"The ETag header is the object's resourceVersion for If-Match on PATCH."
	register(s, getEntries, perm{resource: "blocklists", verb: "read"}, s.listInlineEntries)
	patchEntries := operation(http.MethodPatch, entriesPath, "patchInlineEntries", "Add and remove entries of an inline BlockList source", "blocklists")
	patchEntries.Description = "Edits spec.sources[].inline through the same write path as PUT /blocklists/{namespace}/{name}: If-Match, the managedBy rules and validation apply. " +
		"Entries already present are not added twice; removals of absent entries are ignored. Each added entry is one rule in the source's format."
	patchEntries.Errors = []int{http.StatusPreconditionFailed, http.StatusUnprocessableEntity}
	register(s, patchEntries, perm{resource: "blocklists", verb: "write"}, s.patchInlineEntries)
}

// InlineEntry is one line of an inline source.
type InlineEntry struct {
	Value string `json:"value" doc:"The entry as written in spec.sources[].inline."`
	Line  int    `json:"line" doc:"1-based position in spec.sources[].inline."`
}

// InlineEntries is GET /blocklists/{namespace}/{name}/sources/{source}/entries.
type InlineEntries struct {
	Items    []InlineEntry `json:"items" nullable:"false"`
	Continue string        `json:"continue,omitempty" doc:"Token for the next page; empty on the last page."`
	// Total counts every entry of the source, Matched those matching q.
	Total   int `json:"total" doc:"Entries in the source."`
	Matched int `json:"matched" doc:"Entries matching q (Total without q)."`
	Took
}

type inlineEntriesInput struct {
	Namespace string `path:"namespace"`
	Name      string `path:"name"`
	Source    string `path:"source" doc:"Source name or index."`
	Q         string `query:"q" doc:"Substring filter, case-insensitive."`
	Limit     int    `query:"limit" minimum:"0" maximum:"1000" doc:"Page size (default 100)."`
	Continue  string `query:"continue"`
}

type inlineEntriesOutput struct {
	ETag string `header:"ETag" doc:"The resourceVersion of the BlockList."`
	Body *InlineEntries
}

// InlineEntriesPatch is the body of PATCH …/entries.
type InlineEntriesPatch struct {
	Add    []string `json:"add,omitempty" maxItems:"10000"`
	Remove []string `json:"remove,omitempty" maxItems:"10000"`
}

// InlineEntriesResult is the answer of PATCH …/entries.
type InlineEntriesResult struct {
	// Added and Removed are the entries that changed the source.
	Added   []string `json:"added" nullable:"false"`
	Removed []string `json:"removed" nullable:"false"`
	Total   int      `json:"total" doc:"Entries in the source after the change."`
	// ResourceVersion is the BlockList's new resourceVersion (also the ETag).
	ResourceVersion string `json:"resourceVersion"`
	Took
}

type patchInlineEntriesInput struct {
	Namespace string `path:"namespace"`
	Name      string `path:"name"`
	Source    string `path:"source" doc:"Source name or index."`
	IfMatch   string `header:"If-Match" doc:"Apply only when the stored resourceVersion equals this value (412 otherwise)."`
	Body      InlineEntriesPatch
}

type patchInlineEntriesOutput struct {
	ETag string `header:"ETag"`
	Body *InlineEntriesResult
}

// maxInlineEntryBytes bounds one added entry.
const maxInlineEntryBytes = 1024

// inlineSource finds the source of bl named source: by its name (the
// status.sources[] name), else by index. Only inline sources qualify.
func inlineSource(bl *v1alpha1.BlockList, source string) (int, error) {
	idx := -1
	for i, src := range bl.Spec.Sources {
		if blocking.SourceName(i, src) == source {
			idx = i
			break
		}
	}
	if idx < 0 {
		if n, err := strconv.Atoi(source); err == nil && n >= 0 && n < len(bl.Spec.Sources) {
			idx = n
		}
	}
	if idx < 0 {
		return 0, errNotFound(fmt.Sprintf("source %q of BlockList %s/%s", source, bl.Namespace, bl.Name))
	}
	if src := bl.Spec.Sources[idx]; src.URL != "" || src.ConfigMapRef != nil {
		return 0, newError(http.StatusUnprocessableEntity, KeyUnprocessable,
			fmt.Sprintf("source %q is fetched from a URL or ConfigMap and has no inline entries", source), "edit the list at its origin")
	}
	return idx, nil
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

func (s *Server) listInlineEntries(ctx context.Context, in *inlineEntriesInput) (*inlineEntriesOutput, error) {
	bl, err := s.getBlockList(ctx, in.Namespace, in.Name)
	if err != nil {
		return nil, err
	}
	idx, err := inlineSource(bl, in.Source)
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
	lines := bl.Spec.Sources[idx].Inline
	limit := clampLimit(in.Limit, 100, 1000)
	q := strings.ToLower(strings.TrimSpace(in.Q))
	out := &InlineEntries{Items: []InlineEntry{}, Total: len(lines)}
	for i, v := range lines {
		if q != "" && !strings.Contains(strings.ToLower(v), q) {
			continue
		}
		out.Matched++
		if i < start {
			continue
		}
		if len(out.Items) == limit {
			if out.Continue == "" {
				out.Continue = encodeCursor(strconv.Itoa(i))
			}
			continue
		}
		out.Items = append(out.Items, InlineEntry{Value: v, Line: i + 1})
	}
	return &inlineEntriesOutput{ETag: bl.ResourceVersion, Body: out}, nil
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

// patchAttempts bounds the read-modify-write retries of a PATCH without
// If-Match that loses a race with another writer.
const patchAttempts = 3

func (s *Server) patchInlineEntries(ctx context.Context, in *patchInlineEntriesInput) (*patchInlineEntriesOutput, error) {
	if len(in.Body.Add) == 0 && len(in.Body.Remove) == 0 {
		return nil, errInvalid("body: add or remove at least one entry")
	}
	k, _ := kindByPlural("blocklists")
	for attempt := 1; ; attempt++ {
		bl, err := s.getBlockList(ctx, in.Namespace, in.Name)
		if err != nil {
			return nil, err
		}
		idx, err := inlineSource(bl, in.Source)
		if err != nil {
			return nil, err
		}
		if in.IfMatch != "" && in.IfMatch != bl.ResourceVersion {
			return nil, errPrecondition(in.IfMatch, bl.ResourceVersion)
		}
		src := &bl.Spec.Sources[idx]
		res := &InlineEntriesResult{Added: []string{}, Removed: []string{}}
		drop := map[string]bool{}
		for _, e := range in.Body.Remove {
			drop[e] = true
		}
		kept := make([]string, 0, len(src.Inline)+len(in.Body.Add))
		have := make(map[string]bool, len(src.Inline))
		for _, e := range src.Inline {
			if drop[e] {
				res.Removed = append(res.Removed, e)
				continue
			}
			kept = append(kept, e)
			have[e] = true
		}
		for _, e := range in.Body.Add {
			if err := checkInlineEntry(e, parser.Format(src.Format)); err != nil {
				return nil, err
			}
			if have[e] || drop[e] {
				continue
			}
			have[e] = true
			kept = append(kept, e)
			res.Added = append(res.Added, e)
		}
		res.Total = len(kept)
		if len(res.Added) == 0 && len(res.Removed) == 0 {
			res.ResourceVersion = bl.ResourceVersion
			return &patchInlineEntriesOutput{ETag: bl.ResourceVersion, Body: res}, nil
		}
		if len(kept) == 0 {
			return nil, newError(http.StatusUnprocessableEntity, KeyUnprocessable,
				"an inline source needs at least one entry", "switch the source off (spec.sources[].enabled: false) or delete it")
		}
		src.Inline = kept
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
		return &patchInlineEntriesOutput{ETag: res.ResourceVersion, Body: res}, nil
	}
}

func (s *Server) refreshList(ctx context.Context, in *nsObjectInput) (*refreshOutput, error) {
	k, _ := kindByPlural("blocklists")
	if _, err := s.getObject(ctx, k, in.Namespace, in.Name); err != nil {
		return nil, err
	}
	if s.opts.Refresher == nil {
		return nil, errNotImplemented("an on-demand list refresh")
	}
	err := s.opts.Refresher.RefreshList(ctx, in.Namespace, in.Name)
	s.recordAction(ctx, "blocklists.refresh", audit.Resource{Kind: "BlockList", Namespace: in.Namespace, Name: in.Name}, err, nil, nil)
	var apiErr *Error
	switch {
	case errors.As(err, &apiErr):
		return nil, apiErr
	case err != nil:
		return nil, errUnavailable("refresh: %v", err)
	}
	return &refreshOutput{Status: http.StatusAccepted, Body: &refreshBody{List: in.Namespace + "/" + in.Name}}, nil
}

// ErrNotLeader is what a ListRefresher returns on a replica: the leader
// fetches lists (409 conflict naming it, or 503 while none is elected).
func ErrNotLeader(leader string) error {
	if leader == "" {
		return newError(http.StatusServiceUnavailable, KeyUnavailable, "no leader is elected; lists are fetched by the leader", "retry in a few seconds")
	}
	return newError(http.StatusConflict, KeyConflict, "lists are fetched by the leader "+leader+", not this node",
		"send the request to "+leader+" (any node's API serves it once leadership settles)")
}

// ErrListNotRefreshable is what a ListRefresher returns for a list the
// fetcher does not handle (disabled with kudurru.io/enabled: "false").
func ErrListNotRefreshable(list string) error {
	return newError(http.StatusConflict, KeyConflict, "BlockList "+list+" is disabled and not fetched", `remove the kudurru.io/enabled: "false" annotation first`)
}

// RefreshFunc adapts a function to ListRefresher.
type RefreshFunc func(ctx context.Context, namespace, name string) error

// RefreshList implements ListRefresher.
func (f RefreshFunc) RefreshList(ctx context.Context, namespace, name string) error {
	return f(ctx, namespace, name)
}

func (s *Server) searchLists(ctx context.Context, in *searchInput) (*searchOutput, error) {
	domain := normName(in.Domain)
	if domain == "" {
		return nil, errInvalid("domain: required")
	}
	var hits []ListSearchHit
	if s.opts.Cache != nil {
		if snap := s.opts.Cache.Snapshot(); snap != nil {
			if lists := snap.Objects.Lists(); lists != nil {
				for name, rule := range lists.Contains(domain) {
					hits = append(hits, ListSearchHit{List: name, Mode: string(v1alpha1ListModeBlock), Rule: rule})
				}
				for name, rule := range lists.ContainsAllow(domain) {
					hits = append(hits, ListSearchHit{List: name, Mode: string(v1alpha1ListModeAllow), Rule: rule})
				}
				sort.Slice(hits, func(i, j int) bool { return hits[i].List+hits[i].Mode < hits[j].List+hits[j].Mode })
				return &searchOutput{Body: newList(hits, "")}, nil
			}
		}
	}
	// No compiled lists on this node: search the inline entries.
	err := s.listAll(ctx, "BlockList", "", func(o store.Object) {
		bl, ok := o.(*v1alpha1.BlockList)
		if !ok {
			return
		}
		for _, src := range bl.Spec.Sources {
			for _, e := range src.Inline {
				if inlineMatches(e, domain) {
					mode := string(bl.Spec.Mode)
					if mode == "" {
						mode = string(v1alpha1ListModeBlock)
					}
					hits = append(hits, ListSearchHit{List: bl.Namespace + "/" + bl.Name, Mode: mode, Rule: e})
				}
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return &searchOutput{Body: newList(hits, "")}, nil
}

// The list modes of api/v1alpha1 (enum strings without Go constants).
const (
	v1alpha1ListModeBlock v1alpha1.ListMode = "block"
	v1alpha1ListModeAllow v1alpha1.ListMode = "allow"
)

// inlineMatches is the fallback search over inline entries: exact names,
// ||name^ rules and /regex/ entries.
func inlineMatches(entry, domain string) bool {
	e := strings.TrimSpace(entry)
	if strings.HasPrefix(e, "/") && strings.HasSuffix(e, "/") && len(e) > 2 {
		re, err := regexp.Compile(e[1 : len(e)-1])
		return err == nil && re.MatchString(domain)
	}
	e = strings.TrimPrefix(strings.TrimPrefix(e, "@@"), "||")
	e = strings.TrimSuffix(e, "^")
	e = normName(e)
	return e == domain || strings.HasSuffix(domain, "."+e) && strings.HasPrefix(strings.TrimSpace(entry), "||")
}

// dnsLabel validates a domain for an exact entry.
var dnsName = regexp.MustCompile(`^(\*\.)?([a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9_])?\.)*[a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9_])?$`)

func (s *Server) addDomains(ctx context.Context, in *domainsInput) (*processedOutput, error) {
	ns := in.Body.Namespace
	if ns == "" {
		ns = s.opts.DefaultNamespace
	}
	list := in.Body.List
	if list == "" {
		list = UIDenyList
		if in.Action == "allow" {
			list = UIAllowList
		}
	}
	out := newProcessed()
	var entries []string
	for _, d := range in.Body.Domains {
		e, err := domainEntry(in.Kind, d)
		if err != nil {
			out.fail(d, err)
			continue
		}
		entries = append(entries, e)
	}
	if len(entries) > 0 {
		added, err := s.appendListEntries(ctx, ns, list, in.Action, entries)
		if err != nil {
			for _, e := range entries {
				out.fail(e, err)
			}
		} else {
			for _, e := range entries {
				out.ok(e)
			}
			_ = added
		}
	}
	return &processedOutput{Body: out}, nil
}

// domainEntry validates one domain and renders it as a list entry.
func domainEntry(kind, d string) (string, error) {
	d = strings.TrimSpace(d)
	switch kind {
	case "regex":
		re := strings.TrimSuffix(strings.TrimPrefix(d, "/"), "/")
		if re == "" {
			return "", errInvalid("empty expression")
		}
		if _, err := regexp.Compile(re); err != nil {
			return "", errInvalid("not a valid RE2 expression: %v", err)
		}
		return "/" + re + "/", nil
	default:
		n := normName(d)
		if n == "" || len(n) > 253 || !dnsName.MatchString(n) {
			return "", errInvalid("%q is not a domain name", d)
		}
		return n, nil
	}
}

// appendListEntries adds entries to the inline "ui" source of the
// BlockList ns/name (mode block for deny, allow for allow), creating the
// list on demand and referencing it from Policy ns/default when that
// policy is API- or bootstrap-managed. It returns the entries that were
// new.
func (s *Server) appendListEntries(ctx context.Context, ns, name, action string, entries []string) ([]string, error) {
	k, _ := kindByPlural("blocklists")
	mode := v1alpha1ListModeBlock
	if action == "allow" {
		mode = v1alpha1ListModeAllow
	}
	bl := &v1alpha1.BlockList{}
	created := false
	if err := s.opts.Store.Get(ctx, store.Key{Kind: "BlockList", Namespace: ns, Name: name}, bl); errors.Is(err, store.ErrNotFound) {
		created = true
		bl = &v1alpha1.BlockList{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       v1alpha1.BlockListSpec{Mode: mode, Sources: []v1alpha1.ListSource{{Name: uiSource, Inline: []string{}}}},
		}
	} else if err != nil {
		return nil, storeError(err)
	} else if bl.Spec.Mode != "" && bl.Spec.Mode != mode {
		return nil, newError(http.StatusUnprocessableEntity, KeyUnprocessable,
			fmt.Sprintf("BlockList %s/%s has mode %s, not %s", ns, name, bl.Spec.Mode, mode), "choose another list")
	}
	idx := slices.IndexFunc(bl.Spec.Sources, func(src v1alpha1.ListSource) bool {
		return src.Name == uiSource || (src.URL == "" && src.ConfigMapRef == nil)
	})
	if idx < 0 {
		bl.Spec.Sources = append(bl.Spec.Sources, v1alpha1.ListSource{Name: uiSource})
		idx = len(bl.Spec.Sources) - 1
	}
	src := &bl.Spec.Sources[idx]
	var added []string
	for _, e := range entries {
		if !slices.Contains(src.Inline, e) {
			src.Inline = append(src.Inline, e)
			added = append(added, e)
		}
	}
	if len(added) > 0 || created {
		body, err := typedObject(bl)
		if err != nil {
			return nil, storeError(err)
		}
		req := writeRequest{kind: k, body: body, namespace: ns, name: name}
		if !created {
			req.ifMatch = bl.ResourceVersion
		}
		if _, err := s.writeObject(ctx, req); err != nil {
			return nil, err
		}
	}
	s.referenceFromDefaultPolicy(ctx, ns, name, mode)
	return added, nil
}

// referenceFromDefaultPolicy adds the list to Policy ns/default (block or
// allow refs) so a UI decision takes effect at once. File- and
// integration-managed policies are left alone; the list then stays
// unreferenced until an operator references it.
func (s *Server) referenceFromDefaultPolicy(ctx context.Context, ns, list string, mode v1alpha1.ListMode) {
	pol := &v1alpha1.Policy{}
	if err := s.opts.Store.Get(ctx, store.Key{Kind: "Policy", Namespace: ns, Name: "default"}, pol); err != nil {
		return
	}
	if owner := store.ManagedBy(pol); owner != "" && owner != store.ManagerAPI && owner != store.ManagerBootstrap {
		return
	}
	refs := &pol.Spec.BlockListRefs
	if mode == v1alpha1ListModeAllow {
		refs = &pol.Spec.AllowListRefs
	}
	if slices.Contains(*refs, list) {
		return
	}
	*refs = append(*refs, list)
	body, err := typedObject(pol)
	if err != nil {
		return
	}
	k, _ := kindByPlural("policies")
	if _, err := s.writeObject(ctx, writeRequest{kind: k, body: body, namespace: ns, name: "default", ifMatch: pol.ResourceVersion}); err != nil {
		s.log.Warn("could not reference the UI list from the default policy", "list", ns+"/"+list, "err", err)
	}
}
