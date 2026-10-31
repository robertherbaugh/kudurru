package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/domainlist"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// DomainEntryList is one list a domain entry is stored in.
type DomainEntryList struct {
	List      string `json:"list" doc:"The BlockList, ns/name (ui-allow, ui-deny, ui-allow-<group>, ui-deny-<group>)."`
	Source    string `json:"source" doc:"The source holding the entry (ui)."`
	Group     string `json:"group,omitempty" doc:"The ClientGroup of a group list, ns/name."`
	ManagedBy string `json:"managedBy,omitempty" doc:"The list's owner when not the API."`
}

// DomainOverride is a list the entry wins over (allow) or loses to (deny).
type DomainOverride struct {
	List string `json:"list" doc:"The compiled list, ns/name."`
	Rule string `json:"rule" doc:"The rule of that list that matches the name."`
}

// DomainEntry is one Allowlist or Denylist entry (every copy of namespace,
// type and value across the action's lists).
type DomainEntry struct {
	ID string `json:"id" doc:"Opaque, URL-safe identifier for /allowlist/{id} and /denylist/{id}."`
	// Kind is entry (an Allowlist or Denylist entry) or exception (an entry
	// of another list switched off there: on the Allowlist for block
	// lists, on the Denylist for allow lists).
	Kind      string            `json:"kind" enum:"entry,exception" doc:"entry: an Allowlist or Denylist entry. exception: an entry of the list in lists[0] switched off there (spec.sources[].exclude); it only affects that list, its switch is the exclusion (off switches the list's entry back on) and nothing else of it can be edited."`
	Action    string            `json:"action" enum:"allow,deny"`
	Namespace string            `json:"namespace"`
	Value     string            `json:"value" doc:"exact: ads.example.com; wildcard: *.example.com; regex: the RE2 expression without slashes."`
	Type      string            `json:"type" enum:"exact,wildcard,regex"`
	Rule      string            `json:"rule" doc:"The rule as compiled and shown in the query log: name, *.name or /expression/."`
	Enabled   bool              `json:"enabled"`
	Comment   string            `json:"comment,omitempty"`
	Everyone  bool              `json:"everyone" doc:"Applies to every client (stored in ui-allow / ui-deny)."`
	Groups    []string          `json:"groups" nullable:"false" doc:"ClientGroups (ns/name) with a group-scoped copy."`
	Lists     []DomainEntryList `json:"lists" nullable:"false"`
	Policies  []string          `json:"policies" nullable:"false" doc:"Policies that reference the entry's lists, ns/name."`
	Created   *time.Time        `json:"created,omitempty"`
	CreatedBy string            `json:"createdBy,omitempty" doc:"The actor that added the entry (user:alice)."`
	Modified  *time.Time        `json:"modified,omitempty"`
	Origin    string            `json:"origin" enum:"manual,querylog,unblock,api,migrated,list" doc:"list for an exception."`
	Request   string            `json:"request,omitempty" doc:"The UnblockRequest (ns/name) whose approval added the entry."`
	// Overrides are, for an allow entry, the block lists that contain the
	// name (allow wins over them); for a deny entry, the allow lists that
	// win over it. Empty for regex entries.
	Overrides []DomainOverride `json:"overrides" nullable:"false"`
	// StillMatchedBy, for an exception, are the other lists that still
	// match the name (block lists for an Allowlist exception, allow lists
	// for a Denylist one): the exception does not change them.
	StillMatchedBy []DomainOverride `json:"stillMatchedBy" nullable:"false" doc:"exception only: other lists that still match the name; the exception does not affect them."`
	ReadOnly       bool             `json:"readOnly"`
	ReadOnlyReason string           `json:"readOnlyReason,omitempty"`
}

// DomainListInfo describes one list of the Allowlist or Denylist.
type DomainListInfo struct {
	List      string   `json:"list"`
	Action    string   `json:"action" enum:"allow,deny"`
	Group     string   `json:"group,omitempty"`
	Enabled   bool     `json:"enabled" doc:"The list's kudurru.io/enabled switch."`
	Entries   int      `json:"entries"`
	ManagedBy string   `json:"managedBy,omitempty"`
	Policies  []string `json:"policies" nullable:"false"`
}

// DomainUnattached is a policy that should reference a list but is managed
// elsewhere.
type DomainUnattached struct {
	List      string `json:"list"`
	Policy    string `json:"policy"`
	ManagedBy string `json:"managedBy"`
}

// DomainEntries is GET /allowlist, /denylist and /domains.
type DomainEntries struct {
	Items      []DomainEntry      `json:"items" nullable:"false"`
	Continue   string             `json:"continue,omitempty" doc:"Token for the next page; empty on the last page."`
	Total      int                `json:"total" doc:"Entries of the action(s) and namespace(s), before filters."`
	Matched    int                `json:"matched" doc:"Entries matching the filters."`
	Lists      []DomainListInfo   `json:"lists" nullable:"false"`
	Unattached []DomainUnattached `json:"unattached" nullable:"false" doc:"Policies that should reference a list but are managed by a file or an integration; add the list there."`
	Took
}

// DomainEntriesCreate is the body of POST /allowlist and /denylist.
type DomainEntriesCreate struct {
	Values    []string `json:"values" minItems:"1" maxItems:"1000" doc:"One entry per value, all of one type."`
	Type      string   `json:"type,omitempty" enum:"exact,wildcard,regex" doc:"Default exact."`
	Namespace string   `json:"namespace,omitempty" doc:"Default the server's default namespace."`
	Groups    []string `json:"groups,omitempty" maxItems:"100" doc:"ClientGroups (ns/name or a name in namespace); empty applies to everyone."`
	Comment   string   `json:"comment,omitempty" maxLength:"500"`
	Enabled   *bool    `json:"enabled,omitempty" doc:"Default true."`
	Origin    string   `json:"origin,omitempty" enum:"manual,querylog,api" doc:"Where the entry was added from (default manual)."`
	// List and Source make list exceptions instead: the entries of that
	// list (ns/name or a name in namespace) whose rule names the value are
	// switched off there (spec.sources[].exclude); only that list is
	// affected. A block list's for /allowlist, an allow list's for
	// /denylist.
	List   string `json:"list,omitempty" maxLength:"300" doc:"Switch the value off in this list only (a list exception) instead of adding an entry: ns/name or a name in namespace."`
	Source string `json:"source,omitempty" maxLength:"300" doc:"With list: only this source (default every source that has the value)."`
}

// DomainEntryPatch is the body of PATCH /allowlist/{id} and /denylist/{id}.
type DomainEntryPatch struct {
	Enabled *bool    `json:"enabled,omitempty"`
	Comment *string  `json:"comment,omitempty" maxLength:"500"`
	Value   *string  `json:"value,omitempty" maxLength:"1024"`
	Type    *string  `json:"type,omitempty" enum:"exact,wildcard,regex"`
	Scope   *string  `json:"scope,omitempty" enum:"everyone,groups" doc:"everyone, or groups (with groups)."`
	Groups  []string `json:"groups,omitempty" maxItems:"100"`
}

// DomainEntriesBatch is the body of POST /allowlist:batch and /denylist:batch.
type DomainEntriesBatch struct {
	IDs []string `json:"ids" minItems:"1" maxItems:"1000"`
	Op  string   `json:"op" enum:"enable,disable,delete"`
}

// DomainWriteResult is the answer of the Allowlist and Denylist writes.
type DomainWriteResult struct {
	Items     []DomainEntry  `json:"items" nullable:"false" doc:"The entries written, as stored after the write."`
	Processed ProcessedItems `json:"processed"`
	Attached  []string       `json:"attached" nullable:"false" doc:"Policies that reference the written lists."`
	Skipped   []string       `json:"skipped" nullable:"false" doc:"Policies that should reference a list but are managed elsewhere."`
	Flushed   int            `json:"flushed" doc:"Response-cache entries of the names removed on this node."`
	Took
}

type domainListInput struct {
	Kind      string `query:"kind" enum:"entry,exception" doc:"Only entries, or only list exceptions."`
	Namespace string `query:"namespace" doc:"Only this namespace (default: every namespace)."`
	Type      string `query:"type" enum:"exact,wildcard,regex"`
	Enabled   string `query:"enabled" enum:"true,false"`
	Scope     string `query:"scope" doc:"everyone, a ClientGroup (ns/name), or list:<ns/name> for the exceptions of one list."`
	Q         string `query:"q" doc:"Substring of the value, comment or list, case-insensitive."`
	Limit     int    `query:"limit" minimum:"0" maximum:"1000" doc:"Page size (default 100)."`
	Continue  string `query:"continue"`
}

type domainsListInput struct {
	Action    string `query:"action" enum:"allow,deny" doc:"Only Allowlist (allow) or Denylist (deny) entries."`
	Kind      string `query:"kind" enum:"entry,exception" doc:"Only entries, or only list exceptions."`
	Namespace string `query:"namespace" doc:"Only this namespace (default: every namespace)."`
	Type      string `query:"type" enum:"exact,wildcard,regex"`
	Enabled   string `query:"enabled" enum:"true,false"`
	Scope     string `query:"scope" doc:"everyone, a ClientGroup (ns/name), or list:<ns/name> for the exceptions of one list."`
	Q         string `query:"q" doc:"Substring of the value, comment or list, case-insensitive."`
	Limit     int    `query:"limit" minimum:"0" maximum:"1000" doc:"Page size (default 100)."`
	Continue  string `query:"continue"`
}

type domainListOutput struct {
	Body *DomainEntries
}

type domainIDInput struct {
	ID string `path:"id"`
}

type domainCreateInput struct {
	Body DomainEntriesCreate
}

type domainPatchInput struct {
	ID   string `path:"id"`
	Body DomainEntryPatch
}

type domainBatchInput struct {
	Body DomainEntriesBatch
}

type domainEntryOutput struct {
	Body *DomainEntry
}

type domainWriteOutput struct {
	Body *DomainWriteResult
}

// registerDomainLists registers the Allowlist and Denylist
// (internal/domainlist) and the combined GET /domains.
func (s *Server) registerDomainLists() {
	list := operation(http.MethodGet, "/domains", "listDomainEntries", "List Allowlist and Denylist entries together", "domains")
	list.Description = "The entries of both the Allowlist and the Denylist, newest first, with the same filters as GET /allowlist."
	register(s, list, perm{resource: "blocklists", verb: "read"}, func(ctx context.Context, in *domainsListInput) (*domainListOutput, error) {
		var actions []domainlist.Action
		switch in.Action {
		case "":
			actions = []domainlist.Action{domainlist.Allow, domainlist.Deny}
		default:
			actions = []domainlist.Action{domainlist.Action(in.Action)}
		}
		return s.listDomainEntries(ctx, actions, &domainListInput{
			Kind: in.Kind, Namespace: in.Namespace, Type: in.Type, Enabled: in.Enabled, Scope: in.Scope, Q: in.Q, Limit: in.Limit, Continue: in.Continue,
		})
	})
	for _, a := range []domainlist.Action{domainlist.Allow, domainlist.Deny} {
		path := "/" + string(a) + "list"
		title := "Allowlist"
		what := "Allowed names are never blocked: an allow entry wins over every block list of the policies it applies through."
		if a == domainlist.Deny {
			title = "Denylist"
			what = "Denied names are blocked for the clients the entry applies to, unless an allow list of their policy allows them."
		}
		tag := "domains"
		get := operation(http.MethodGet, path, "list"+title, "List "+title+" entries", tag)
		get.Description = what + " Entries are stored on the BlockLists " + domainlist.ListName(a, "") + " (everyone) and " +
			domainlist.ListName(a, "<group>") + " (one ClientGroup), referenced by the policies concerned. Newest first."
		register(s, get, perm{resource: "blocklists", verb: "read"}, func(ctx context.Context, in *domainListInput) (*domainListOutput, error) {
			return s.listDomainEntries(ctx, []domainlist.Action{a}, in)
		})
		create := operation(http.MethodPost, path, "create"+title+"Entries", "Add "+title+" entries", tag)
		create.Description = "Adds one entry per value, of one type and scope. A value that exists already gets the new comment and switch (when given) and its scope widened. " +
			"The lists are created on demand and referenced from the policies of the scope (every policy of the namespace for everyone, the policies naming the group otherwise); policies managed by a file or an integration are reported in skipped. " +
			"Refused values are listed in processed.errors; a scope error (unknown group, no policy covers it) answers 422."
		create.Errors = []int{http.StatusConflict, http.StatusUnprocessableEntity}
		register(s, create, perm{resource: "blocklists", verb: "write"}, func(ctx context.Context, in *domainCreateInput) (*domainWriteOutput, error) {
			return s.createDomainEntries(ctx, a, &in.Body)
		})
		register(s, operation(http.MethodGet, path+"/{id}", "get"+title+"Entry", "Get a "+title+" entry", tag), perm{resource: "blocklists", verb: "read"},
			func(ctx context.Context, in *domainIDInput) (*domainEntryOutput, error) {
				if s.capturedConvenienceReads() {
					return s.getCapturedDomainEntry(ctx, a, in.ID)
				}
				e, set, err := s.findDomainEntry(ctx, a, in.ID)
				if err != nil {
					return nil, err
				}
				v := s.domainView(e, set)
				return &domainEntryOutput{Body: &v}, nil
			})
		patch := operation(http.MethodPatch, path+"/{id}", "patch"+title+"Entry", "Change a "+title+" entry", tag)
		patch.Description = "Switch it on or off, change its comment, value, type or scope. A new scope moves the entry between the everyone list and the group lists."
		patch.Errors = []int{http.StatusConflict, http.StatusUnprocessableEntity}
		register(s, patch, perm{resource: "blocklists", verb: "write"}, func(ctx context.Context, in *domainPatchInput) (*domainWriteOutput, error) {
			return s.patchDomainEntry(ctx, a, in.ID, &in.Body)
		})
		del := operation(http.MethodDelete, path+"/{id}", "delete"+title+"Entry", "Delete a "+title+" entry", tag)
		del.Description = "Removes every copy; a list left empty is deleted and its policy references removed. For a list exception: removes the exclusion, so the list's entry applies again."
		register(s, withStatus(del, http.StatusNoContent), perm{resource: "blocklists", verb: "write"}, func(ctx context.Context, in *domainIDInput) (*emptyOutput, error) {
			if s.useDomainCommand() {
				return s.commandDeleteDomainEntry(ctx, a, in.ID)
			}
			if _, _, _, _, ok := domainlist.ParseExceptionID(in.ID); ok {
				if _, err := s.setException(ctx, a, in.ID, false); err != nil {
					return nil, err
				}
				return &emptyOutput{}, nil
			}
			ns, t, v, err := domainlist.ParseID(in.ID)
			if err != nil {
				return nil, domainError(err)
			}
			if err := s.readOnly(); err != nil {
				return nil, err
			}
			res, err := s.domainLists().Delete(ctx, a, ns, t, v)
			s.recordDomainAction(ctx, a, "delete", ns, res, err, map[string]any{"type": t, "value": v})
			if err != nil {
				return nil, domainError(err)
			}
			s.flushNames(res.Names)
			return &emptyOutput{}, nil
		})
		batch := operation(http.MethodPost, path+":batch", "batch"+title+"Entries", "Switch on, off or delete several "+title+" entries", tag)
		register(s, batch, perm{resource: "blocklists", verb: "write"}, func(ctx context.Context, in *domainBatchInput) (*domainWriteOutput, error) {
			return s.batchDomainEntries(ctx, a, &in.Body)
		})
	}
}

// domainLists is the store's Allowlist and Denylist.
func (s *Server) domainLists() *domainlist.Service {
	s.domainOnce.Do(func() {
		s.domainSvc = domainlist.New(domainlist.Options{Store: s.opts.Store, Now: s.opts.Now, Logger: s.log})
	})
	return s.domainSvc
}

// domainError maps domainlist errors onto the envelope.
func domainError(err error) error {
	switch {
	case errors.Is(err, domainlist.ErrNotFound):
		return errNotFound(strings.TrimPrefix(err.Error(), "domainlist: no such entry: "))
	case errors.Is(err, domainlist.ErrNoPolicy):
		return newError(http.StatusUnprocessableEntity, KeyUnprocessable, err.Error(), "create a policy for the group, or use scope everyone")
	case errors.Is(err, domainlist.ErrInvalid):
		msg := strings.TrimPrefix(err.Error(), "domainlist: invalid: ")
		if strings.Contains(msg, "client group") {
			return newError(http.StatusUnprocessableEntity, KeyUnprocessable, msg, "")
		}
		return errInvalid("%s", msg)
	}
	return storeError(err)
}

func itemError(err error) string {
	return strings.TrimPrefix(err.Error(), "domainlist: invalid: ")
}

func (s *Server) recordDomainAction(ctx context.Context, a domainlist.Action, verb, ns string, res *domainlist.Result, err error, detail map[string]any) {
	name := domainlist.ListName(a, "")
	if res != nil && len(res.Lists) > 0 {
		_, name, _ = strings.Cut(res.Lists[0], "/")
	}
	if res != nil {
		detail["lists"] = res.Lists
		detail["attached"] = res.Attached
	}
	s.recordAction(ctx, string(a)+"list."+verb, audit.Resource{Kind: "BlockList", Namespace: ns, Name: name}, err, nil, detail)
}

// flushNames removes names from this node's response cache.
func (s *Server) flushNames(names []string) int {
	n := 0
	for _, name := range names {
		n += s.flushName(name)
	}
	return n
}

func (s *Server) namespaceOr(ns string) string {
	if ns == "" {
		return s.opts.DefaultNamespace
	}
	return ns
}

func (s *Server) listDomainEntries(ctx context.Context, actions []domainlist.Action, in *domainListInput) (*domainListOutput, error) {
	if s.capturedConvenienceReads() {
		return s.listCapturedDomainEntries(ctx, actions, in)
	}
	set, err := s.domainLists().Load(ctx, in.Namespace)
	if err != nil {
		return nil, storeError(err)
	}
	start := 0
	if pos, err := decodeCursor(in.Continue); err != nil {
		return nil, err
	} else if pos != "" {
		if start, err = strconv.Atoi(pos); err != nil || start < 0 {
			return nil, errInvalid("continue: not a token returned by this API")
		}
	}
	return s.domainEntriesPage(ctx, actions, in, set, start, s.legacyDomainLists(), func(offset int) string { return encodeCursor(strconv.Itoa(offset)) })
}

func (s *Server) domainEntriesPage(ctx context.Context, actions []domainlist.Action, in *domainListInput, set *domainlist.Set, start int, lists *blocking.Snapshot, next func(int) string) (*domainListOutput, error) {
	scope := strings.TrimSpace(in.Scope)
	listScope, byList := strings.CutPrefix(scope, "list:")
	if byList && !strings.Contains(listScope, "/") {
		listScope = s.namespaceOr(in.Namespace) + "/" + listScope
	}
	if !byList && scope != "" && scope != "everyone" && !strings.Contains(scope, "/") {
		scope = s.namespaceOr(in.Namespace) + "/" + scope
	}
	q := strings.ToLower(strings.TrimSpace(in.Q))
	limit := clampLimit(in.Limit, 100, 1000)
	out := &DomainEntries{Items: []DomainEntry{}, Lists: []DomainListInfo{}, Unattached: []DomainUnattached{}}
	var all []*domainlist.Entry
	for _, a := range actions {
		all = append(all, set.Entries(a)...)
	}
	if len(actions) > 1 {
		sort.SliceStable(all, func(i, j int) bool {
			ci, cj := all[i].Primary().Created, all[j].Primary().Created
			switch {
			case ci == nil:
				return false
			case cj == nil:
				return true
			}
			return ci.After(cj.Time)
		})
	}
	out.Total = len(all)
	n := 0
	for _, e := range all {
		if ctx.Err() != nil {
			return nil, objectReadUnavailable()
		}
		if in.Kind != "" && e.Kind != in.Kind ||
			in.Type != "" && string(e.Type) != in.Type ||
			in.Enabled != "" && strconv.FormatBool(e.Enabled()) != in.Enabled ||
			byList && (e.Kind != domainlist.KindException || e.Copies[0].List != listScope) ||
			!byList && scope == "everyone" && !e.Everyone ||
			!byList && scope != "" && scope != "everyone" && !slices.Contains(e.Groups, scope) ||
			q != "" && !domainMatches(e, q) {
			continue
		}
		out.Matched++
		n++
		if n-1 < start {
			continue
		}
		if len(out.Items) == limit {
			if out.Continue == "" {
				out.Continue = next(n - 1)
			}
			continue
		}
		out.Items = append(out.Items, s.domainViewWithLists(e, set, lists))
	}
	for _, l := range set.Lists {
		if !slices.Contains(actions, l.Action) {
			continue
		}
		out.Lists = append(out.Lists, DomainListInfo{List: l.List, Action: string(l.Action), Group: l.Group, Enabled: l.Enabled, Entries: l.Entries, ManagedBy: l.ManagedBy, Policies: nonNil(l.Policies)})
	}
	for _, u := range set.Unattached {
		if l := slices.IndexFunc(set.Lists, func(x domainlist.ListInfo) bool { return x.List == u.List }); l >= 0 && slices.Contains(actions, set.Lists[l].Action) {
			out.Unattached = append(out.Unattached, DomainUnattached{List: u.List, Policy: u.Policy, ManagedBy: u.ManagedBy})
		}
	}
	return &domainListOutput{Body: out}, nil
}

// domainMatches is the q search: the value, the comment or a list name.
func domainMatches(e *domainlist.Entry, q string) bool {
	if strings.Contains(strings.ToLower(e.Value), q) || strings.Contains(strings.ToLower(e.Primary().Comment), q) {
		return true
	}
	return slices.ContainsFunc(e.Copies, func(c domainlist.Copy) bool { return strings.Contains(strings.ToLower(c.List), q) })
}

// legacyDomainLists samples the optional legacy display cache.
func (s *Server) legacyDomainLists() *blocking.Snapshot {
	if s.opts.Cache != nil {
		if snap := s.opts.Cache.Snapshot(); snap != nil {
			return snap.Objects.Lists()
		}
	}
	return nil
}

// domainView preserves the legacy and postcommit projection entry point.
func (s *Server) domainView(e *domainlist.Entry, set *domainlist.Set) DomainEntry {
	return s.domainViewWithLists(e, set, s.legacyDomainLists())
}

func (s *Server) domainViewWithLists(e *domainlist.Entry, set *domainlist.Set, lists *blocking.Snapshot) DomainEntry {
	p := e.Primary()
	v := DomainEntry{
		ID: e.ID(), Action: string(e.Action), Namespace: e.Namespace, Value: e.Value, Type: string(e.Type), Rule: e.Rule(),
		Enabled: e.Enabled(), Comment: p.Comment, Everyone: e.Everyone, Groups: nonNil(slices.Clone(e.Groups)),
		Lists: []DomainEntryList{}, Policies: []string{}, Created: timePtr(p.Created), CreatedBy: p.CreatedBy, Modified: timePtr(p.Modified),
		Origin: string(p.Origin), Request: p.Request, Overrides: []DomainOverride{}, StillMatchedBy: []DomainOverride{}, Kind: e.Kind,
	}
	if v.Kind == "" {
		v.Kind = domainlist.KindEntry
	}
	if e.Kind == domainlist.KindException {
		return s.exceptionViewWithLists(v, e, set, lists)
	}
	if v.Origin == "" {
		v.Origin = string(v1alpha1.OriginManual)
	}
	for _, c := range e.Copies {
		managed := c.ManagedBy
		if managed == store.ManagerAPI {
			managed = ""
		}
		v.Lists = append(v.Lists, DomainEntryList{List: c.List, Source: c.Source, Group: c.Group, ManagedBy: managed})
		if set != nil {
			for _, l := range set.Lists {
				if l.List == c.List {
					for _, pol := range l.Policies {
						if !slices.Contains(v.Policies, pol) {
							v.Policies = append(v.Policies, pol)
						}
					}
				}
			}
		}
	}
	slices.Sort(v.Policies)
	if owner := e.ReadOnly(); owner != "" {
		v.ReadOnly, v.ReadOnlyReason = true, "a list of this entry is managed by "+owner+"; change it there"
	}
	v.Overrides = domainOverridesWithLists(e, lists)
	return v
}

// exceptionViewWithLists completes the API form of a list exception.
func (s *Server) exceptionViewWithLists(v DomainEntry, e *domainlist.Entry, set *domainlist.Set, lists *blocking.Snapshot) DomainEntry {
	c := e.Copies[0]
	v.Origin = "list"
	managed := c.ManagedBy
	if managed == "api" {
		managed = ""
	}
	v.Lists = []DomainEntryList{{List: c.List, Source: c.Source, ManagedBy: managed}}
	if set != nil {
		v.Policies = nonNil(slices.Clone(set.ExceptionPolicies[c.List]))
	}
	v.Overrides = []DomainOverride{{List: c.List, Rule: e.Value}}
	if c.ManagedBy != "" && c.ManagedBy != "api" && c.ManagedBy != "bootstrap" {
		v.ReadOnly, v.ReadOnlyReason = true, "the list is managed by "+c.ManagedBy+"; change it there"
	}
	t, name, ok := domainlist.FromRule(e.Value, "")
	if !ok || lists == nil {
		return v
	}
	n := domainlist.MatchName(t, name)
	if n == "" {
		return v
	}
	hits := lists.Contains(n)
	if e.Action == domainlist.Deny {
		hits = lists.ContainsAllow(n)
	}
	for list, rule := range hits {
		if list != c.List {
			v.StillMatchedBy = append(v.StillMatchedBy, DomainOverride{List: list, Rule: rule})
		}
	}
	slices.SortFunc(v.StillMatchedBy, func(a, b DomainOverride) int { return strings.Compare(a.List, b.List) })
	return v
}

// domainOverridesWithLists finds the compiled lists an entry interacts with: for an
// allow entry the block lists that contain the name, for a deny entry the
// allow lists that win over it.
func domainOverridesWithLists(e *domainlist.Entry, lists *blocking.Snapshot) []DomainOverride {
	out := []DomainOverride{}
	name := domainlist.MatchName(e.Type, e.Value)
	if name == "" || lists == nil {
		return out
	}
	hits := lists.Contains(name)
	if e.Action == domainlist.Deny {
		hits = lists.ContainsAllow(name)
	}
	own := map[string]bool{}
	for _, c := range e.Copies {
		own[c.List] = true
	}
	for list, rule := range hits {
		if !own[list] {
			out = append(out, DomainOverride{List: list, Rule: rule})
		}
	}
	slices.SortFunc(out, func(a, b DomainOverride) int { return strings.Compare(a.List, b.List) })
	return out
}

func (s *Server) findDomainEntry(ctx context.Context, a domainlist.Action, id string) (*domainlist.Entry, *domainlist.Set, error) {
	if ns, list, source, value, ok := domainlist.ParseExceptionID(id); ok {
		return s.exceptionEntry(ctx, a, ns, list, source, value)
	}
	ns, t, v, err := domainlist.ParseID(id)
	if err != nil {
		return nil, nil, domainError(err)
	}
	set, err := s.domainLists().Load(ctx, ns)
	if err != nil {
		return nil, nil, storeError(err)
	}
	e := set.Find(a, ns, t, v)
	if e == nil {
		return nil, nil, errNotFound(fmt.Sprintf("%slist entry %s %q in namespace %s", a, t, v, ns))
	}
	return e, set, nil
}

// writeResult builds the answer of a write from the stored entries.
func (s *Server) writeResult(ctx context.Context, a domainlist.Action, ns string, res *domainlist.Result, processed ProcessedItems) (*DomainWriteResult, error) {
	out := &DomainWriteResult{Items: []DomainEntry{}, Processed: processed, Attached: nonNil(res.Attached), Skipped: nonNil(res.Skipped)}
	if len(res.IDs) > 0 {
		set, err := s.domainLists().Load(ctx, ns)
		if err != nil {
			return nil, storeError(err)
		}
		for _, id := range res.IDs {
			_, t, v, err := domainlist.ParseID(id)
			if err != nil {
				continue
			}
			if e := set.Find(a, ns, t, v); e != nil {
				out.Items = append(out.Items, s.domainView(e, set))
			}
		}
	}
	out.Flushed = s.flushNames(res.Names)
	return out, nil
}

func (s *Server) createDomainEntries(ctx context.Context, a domainlist.Action, in *DomainEntriesCreate) (*domainWriteOutput, error) {
	if s.useDomainCommand() {
		return s.commandCreateDomainEntries(ctx, a, in)
	}
	if err := s.readOnly(); err != nil {
		return nil, err
	}
	if in.List != "" {
		return s.createExceptions(ctx, a, in)
	}
	t, err := domainlist.ParseType(in.Type)
	if err != nil {
		return nil, domainError(err)
	}
	origin := v1alpha1.EntryOrigin(in.Origin)
	if origin == "" {
		origin = v1alpha1.OriginManual
	}
	ns := s.namespaceOr(in.Namespace)
	res, err := s.domainLists().Add(ctx, domainlist.AddRequest{
		Action: a, Namespace: ns, Type: t, Values: in.Values, Scope: domainlist.Scope{Groups: in.Groups},
		Comment: in.Comment, Enabled: in.Enabled, Origin: origin, Actor: s.actor(ctx),
	})
	s.recordDomainAction(ctx, a, "add", ns, res, err, map[string]any{"type": t, "values": in.Values, "groups": in.Groups, "comment": in.Comment, "origin": origin})
	if err != nil {
		return nil, domainError(err)
	}
	processed := ProcessedItems{Success: []ProcessedItem{}, Errors: []ProcessedItem{}}
	failed := map[string]bool{}
	for _, e := range res.Errors {
		failed[e.Item] = true
		processed.Errors = append(processed.Errors, ProcessedItem{Item: e.Item, Error: itemError(e.Err)})
	}
	for _, v := range in.Values {
		if !failed[v] {
			processed.Success = append(processed.Success, ProcessedItem{Item: v})
		}
	}
	out, err := s.writeResult(ctx, a, ns, res, processed)
	if err != nil {
		return nil, err
	}
	return &domainWriteOutput{Body: out}, nil
}

func (s *Server) patchDomainEntry(ctx context.Context, a domainlist.Action, id string, in *DomainEntryPatch) (*domainWriteOutput, error) {
	if s.useDomainCommand() {
		return s.commandPatchDomainEntry(ctx, a, id, in)
	}
	if err := s.readOnly(); err != nil {
		return nil, err
	}
	if _, _, _, _, ok := domainlist.ParseExceptionID(id); ok {
		if in.Comment != nil || in.Value != nil || in.Type != nil || in.Scope != nil || len(in.Groups) > 0 {
			return nil, newError(http.StatusUnprocessableEntity, KeyUnprocessable, "a list exception has only its switch", "edit the list's entries on its page, or add an Allowlist or Denylist entry")
		}
		if in.Enabled == nil {
			return nil, errInvalid("body: enabled is required for a list exception")
		}
		v, err := s.setException(ctx, a, id, *in.Enabled)
		if err != nil {
			return nil, err
		}
		return &domainWriteOutput{Body: &DomainWriteResult{Items: []DomainEntry{*v}, Processed: ProcessedItems{Success: []ProcessedItem{{Item: id}}, Errors: []ProcessedItem{}}, Attached: []string{}, Skipped: []string{}}}, nil
	}
	ns, t, v, err := domainlist.ParseID(id)
	if err != nil {
		return nil, domainError(err)
	}
	p := domainlist.Patch{Enabled: in.Enabled, Comment: in.Comment, Value: in.Value, Actor: s.actor(ctx)}
	if in.Type != nil {
		nt, err := domainlist.ParseType(*in.Type)
		if err != nil {
			return nil, domainError(err)
		}
		p.Type = &nt
	}
	switch {
	case in.Scope == nil && len(in.Groups) > 0:
		p.Scope = &domainlist.Scope{Groups: in.Groups}
	case in.Scope == nil:
	case *in.Scope == "everyone":
		p.Scope = &domainlist.Scope{}
	case len(in.Groups) == 0:
		return nil, errInvalid("groups: scope groups needs at least one ClientGroup")
	default:
		p.Scope = &domainlist.Scope{Groups: in.Groups}
	}
	res, err := s.domainLists().Update(ctx, a, ns, t, v, p)
	s.recordDomainAction(ctx, a, "update", ns, res, err, map[string]any{"type": t, "value": v, "patch": in})
	if err != nil {
		return nil, domainError(err)
	}
	out, err := s.writeResult(ctx, a, ns, res, ProcessedItems{Success: []ProcessedItem{{Item: id}}, Errors: []ProcessedItem{}})
	if err != nil {
		return nil, err
	}
	return &domainWriteOutput{Body: out}, nil
}

func (s *Server) batchDomainEntries(ctx context.Context, a domainlist.Action, in *DomainEntriesBatch) (*domainWriteOutput, error) {
	if s.useDomainCommand() {
		return s.commandBatchDomainEntries(ctx, a, in)
	}
	if err := s.readOnly(); err != nil {
		return nil, err
	}
	processed := ProcessedItems{Success: []ProcessedItem{}, Errors: []ProcessedItem{}}
	total := &domainlist.Result{}
	byNS := map[string][]string{}
	var exceptions []DomainEntry
	for _, id := range in.IDs {
		if _, _, _, _, ok := domainlist.ParseExceptionID(id); ok {
			ev, err := s.setException(ctx, a, id, in.Op == "enable")
			if err != nil {
				processed.Errors = append(processed.Errors, ProcessedItem{Item: id, Error: errorMessage(err)})
				continue
			}
			processed.Success = append(processed.Success, ProcessedItem{Item: id})
			if in.Op != "delete" {
				exceptions = append(exceptions, *ev)
			}
			continue
		}
		ns, t, v, err := domainlist.ParseID(id)
		if err == nil {
			var res *domainlist.Result
			switch in.Op {
			case "delete":
				res, err = s.domainLists().Delete(ctx, a, ns, t, v)
			default:
				on := in.Op == "enable"
				res, err = s.domainLists().Update(ctx, a, ns, t, v, domainlist.Patch{Enabled: &on, Actor: s.actor(ctx)})
			}
			if err == nil {
				total.Names = append(total.Names, res.Names...)
				total.Attached = append(total.Attached, res.Attached...)
				total.Skipped = append(total.Skipped, res.Skipped...)
				total.Lists = append(total.Lists, res.Lists...)
				if in.Op != "delete" {
					byNS[ns] = append(byNS[ns], res.IDs...)
				}
			}
		}
		if err != nil {
			processed.Errors = append(processed.Errors, ProcessedItem{Item: id, Error: errorMessage(domainError(err))})
			continue
		}
		processed.Success = append(processed.Success, ProcessedItem{Item: id})
	}
	slices.Sort(total.Attached)
	total.Attached = slices.Compact(total.Attached)
	slices.Sort(total.Skipped)
	total.Skipped = slices.Compact(total.Skipped)
	s.recordDomainAction(ctx, a, "batch", s.opts.DefaultNamespace, total, nil, map[string]any{"op": in.Op, "ids": in.IDs, "errors": len(processed.Errors)})
	out := &DomainWriteResult{Items: []DomainEntry{}, Processed: processed, Attached: nonNil(total.Attached), Skipped: nonNil(total.Skipped)}
	for ns, ids := range byNS {
		res, err := s.writeResult(ctx, a, ns, &domainlist.Result{IDs: ids}, processed)
		if err != nil {
			return nil, err
		}
		out.Items = append(out.Items, res.Items...)
	}
	out.Items = append(out.Items, exceptions...)
	out.Flushed = s.flushNames(total.Names)
	return &domainWriteOutput{Body: out}, nil
}

// errorMessage is the message of an API error.
func errorMessage(err error) string {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr.Body.Message
	}
	return err.Error()
}
