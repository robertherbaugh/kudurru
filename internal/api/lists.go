package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/domainlist"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// UI list names: the Allowlist and Denylist keep their everyone entries
// in these BlockLists (internal/domainlist), created on demand.
const (
	UIAllowList = domainlist.AllowList
	UIDenyList  = domainlist.DenyList
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
	Domains   []string `json:"domains" minItems:"1" maxItems:"1000"`
	Namespace string   `json:"namespace,omitempty" doc:"Namespace of the list; defaults to the server's default namespace (the release namespace in Kubernetes, or \"default\" for legacy installs and other modes)."`
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
	s.registerSourceEntries()
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
		mode := string(bl.Spec.Mode)
		if mode == "" {
			mode = string(v1alpha1ListModeBlock)
		}
		for _, src := range bl.Spec.Sources {
			if !src.IsEnabled() {
				continue
			}
			lines := src.Inline
			for _, e := range src.Entries {
				if e.IsEnabled() {
					lines = append(lines[:len(lines):len(lines)], v1alpha1.EntryRule(e.Type, e.Value))
				}
			}
			for _, e := range lines {
				if !slices.Contains(src.Exclude, e) && inlineMatches(e, domain) {
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

// inlineMatches is the fallback search over inline and structured
// entries: exact names, ||name^ and *.name rules and /regex/ entries.
func inlineMatches(entry, domain string) bool {
	e := strings.TrimSpace(entry)
	if strings.HasPrefix(e, "/") && strings.HasSuffix(e, "/") && len(e) > 2 {
		re, err := regexp.Compile(e[1 : len(e)-1])
		return err == nil && re.MatchString(domain)
	}
	sub := strings.HasPrefix(e, "||") || strings.HasPrefix(e, "*.")
	e = strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(e, "@@"), "||"), "*.")
	e = strings.TrimSuffix(e, "^")
	e = normName(e)
	return e == domain || sub && strings.HasSuffix(domain, "."+e)
}

// addDomains is the compatibility route of the Allowlist and Denylist:
// exact domains ("*.name" ones become wildcard entries) or regular
// expressions for everyone, written as structured entries with origin
// api. A list name other than the ui lists gets the entries too and is
// referenced from the default policy.
func (s *Server) addDomains(ctx context.Context, in *domainsInput) (*processedOutput, error) {
	if err := s.readOnly(); err != nil {
		return nil, err
	}
	ns := s.namespaceOr(in.Body.Namespace)
	action := domainlist.Deny
	if in.Action == "allow" {
		action = domainlist.Allow
	}
	out := newProcessed()
	byType := map[v1alpha1.EntryType][]string{}
	var order []v1alpha1.EntryType
	for _, d := range in.Body.Domains {
		t := v1alpha1.EntryExact
		switch v := strings.TrimSpace(d); {
		case in.Kind == "regex":
			t = v1alpha1.EntryRegex
		case strings.HasPrefix(v, "*.") || strings.HasPrefix(v, "||"):
			t = v1alpha1.EntryWildcard
		}
		if _, ok := byType[t]; !ok {
			order = append(order, t)
		}
		byType[t] = append(byType[t], d)
	}
	_, _, uiList := domainlist.ParseListName(in.Body.List)
	for _, t := range order {
		req := domainlist.AddRequest{Action: action, Namespace: ns, Type: t, Values: byType[t], Origin: v1alpha1.OriginAPI, Actor: s.actor(ctx)}
		var res *domainlist.Result
		var err error
		if in.Body.List == "" || uiList {
			if uiList {
				_, g, _ := domainlist.ParseListName(in.Body.List)
				if g != "" {
					req.Scope = domainlist.Scope{Groups: []string{g}}
				}
			}
			res, err = s.domainLists().Add(ctx, req)
		} else {
			res, err = s.domainLists().AddToList(ctx, req, in.Body.List)
		}
		s.recordDomainAction(ctx, action, "add", ns, res, err, map[string]any{"type": t, "values": req.Values, "list": in.Body.List})
		if err != nil {
			for _, d := range byType[t] {
				out.fail(d, domainError(err))
			}
			continue
		}
		failed := map[string]bool{}
		for _, e := range res.Errors {
			failed[e.Item] = true
			out.Processed.Errors = append(out.Processed.Errors, ProcessedItem{Item: e.Item, Error: itemError(e.Err)})
		}
		for _, d := range byType[t] {
			if !failed[d] {
				out.ok(d)
			}
		}
		s.flushNames(res.Names)
	}
	return &processedOutput{Body: out}, nil
}
