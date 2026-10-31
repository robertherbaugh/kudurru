package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/unblock"
)

// UnblockBlock is why the name was blocked when the request was made.
type UnblockBlock struct {
	Kind   string `json:"kind" doc:"list, service, safesearch, policy (allow-only) or cname."`
	List   string `json:"list,omitempty" doc:"The deciding list: ns/name, service:<id> or safesearch:<engine>."`
	Rule   string `json:"rule,omitempty"`
	Policy string `json:"policy,omitempty" doc:"The client's policy, ns/name."`
}

// UnblockAllow is the allow entry an approval created.
type UnblockAllow struct {
	Scope string `json:"scope" enum:"everyone,group"`
	Match string `json:"match" enum:"exact,subdomains"`
	Group string `json:"group,omitempty" doc:"The ClientGroup of a group approval, ns/name."`
	List  string `json:"list" doc:"The allow BlockList, ns/name (ui-allow or ui-allow-<group>)."`
	Entry string `json:"entry" doc:"The line in the list's inline source ui."`
	Added bool   `json:"added" doc:"The approval added the entry (it was not on the list); only then does its end remove it."`
	// Policies the list was attached to by the approval.
	Policies  []string   `json:"policies" nullable:"false"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty" doc:"End of a time-limited approval; absent is permanent."`
	Removed   *time.Time `json:"removed,omitempty" doc:"When the entry was released (expiry or revocation)."`
}

// UnblockDecision is one earlier decision of a request asked again.
type UnblockDecision struct {
	State     string        `json:"state" enum:"Approved,Denied,Expired"`
	DecidedBy string        `json:"decidedBy,omitempty"`
	DecidedAt *time.Time    `json:"decidedAt,omitempty"`
	Note      string        `json:"note,omitempty"`
	Allow     *UnblockAllow `json:"allow,omitempty"`
}

// UnblockRequest is one request from the block page (the UnblockRequest
// object, flattened).
type UnblockRequest struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Domain    string `json:"domain"`
	// Client is the requester as querylog.privacy allowed recording it.
	Client        string        `json:"client,omitempty" doc:"The client address (full), its /24 or /48 network address (prefix), or absent (hidden)."`
	ClientPrivacy string        `json:"clientPrivacy" enum:"full,prefix,hidden" doc:"How the client was recorded under querylog.privacy."`
	Groups        []string      `json:"groups" nullable:"false" doc:"The client's ClientGroups at request time, ns/name."`
	Policies      []string      `json:"policies" nullable:"false" doc:"The client's policies at request time, highest priority first."`
	Node          string        `json:"node,omitempty" doc:"The node whose block page took the request."`
	RequesterName string        `json:"requesterName,omitempty" doc:"Plain text the user typed."`
	Contact       string        `json:"contact,omitempty" doc:"Plain text the user typed."`
	Reason        string        `json:"reason,omitempty" doc:"Plain text the user typed (at most 500 characters)."`
	Block         *UnblockBlock `json:"block,omitempty"`
	CreatedAt     time.Time     `json:"createdAt"`
	RequestedAt   time.Time     `json:"requestedAt" doc:"The latest submission."`
	Submissions   int           `json:"submissions"`
	State         string        `json:"state" enum:"Pending,Approved,Denied,Expired"`
	DecidedBy     string        `json:"decidedBy,omitempty"`
	DecidedAt     *time.Time    `json:"decidedAt,omitempty"`
	Note          string        `json:"note,omitempty" doc:"The administrator's note, shown on the block page."`
	Allow         *UnblockAllow `json:"allow,omitempty"`
	// ExpiresAt is when a pending request expires undecided.
	ExpiresAt       *time.Time        `json:"expiresAt,omitempty" doc:"When a pending request expires undecided (unblockRequests.pendingExpiry)."`
	History         []UnblockDecision `json:"history" nullable:"false"`
	ResourceVersion string            `json:"resourceVersion"`
}

// UnblockCounts counts the requests by state.
type UnblockCounts struct {
	Pending  int `json:"pending"`
	Approved int `json:"approved"`
	Denied   int `json:"denied"`
	Expired  int `json:"expired"`
}

// UnblockRequestList is GET /unblockrequests.
type UnblockRequestList struct {
	Items    []UnblockRequest `json:"items" nullable:"false"`
	Continue string           `json:"continue,omitempty" doc:"Token for the next page; empty on the last page."`
	Counts   UnblockCounts    `json:"counts" doc:"Every request by state (not only this page)."`
	Took
}

// UnblockApproval is the body of POST …/approve.
type UnblockApproval struct {
	Scope string `json:"scope,omitempty" enum:"everyone,group" doc:"everyone: the everyone list ui-allow, attached to every policy of the namespace that kudurru may edit (policies managed by a file or an integration are skipped). group: ui-allow-<group>, attached to the policies of the group. Default everyone. Approving needs only unblockrequests.decide: the approval writes the allow list and the policy references itself."`
	Match string `json:"match,omitempty" enum:"exact,subdomains" doc:"exact: the name only; subdomains: the name and every name below it (||name^). Default exact."`
	Group string `json:"group,omitempty" maxLength:"300" doc:"The ClientGroup of a group approval (ns/name or a name in the request's namespace); default the request's first group."`
	// Duration limits the approval; empty is permanent.
	Duration string `json:"duration,omitempty" maxLength:"32" doc:"Go duration (1h, 24h, 168h); empty is permanent. The entry is removed when it ends."`
	Note     string `json:"note,omitempty" maxLength:"500" doc:"Shown to the user on the block page."`
}

// UnblockDenial is the body of POST …/deny.
type UnblockDenial struct {
	Note string `json:"note,omitempty" maxLength:"500" doc:"Shown to the user on the block page."`
}

// UnblockDecisionResult is the answer of approve and deny.
type UnblockDecisionResult struct {
	Request UnblockRequest `json:"request"`
	// Attached are the policies the allow list was added to; Skipped are
	// managed by a file or an integration and were left alone.
	Attached []string `json:"attached" nullable:"false"`
	Skipped  []string `json:"skipped" nullable:"false" doc:"Policies that should reference the list but are managed elsewhere; add the list there."`
	// Flushed counts the response-cache entries of the name removed on this node.
	Flushed int `json:"flushed"`
	Took
}

type unblockListInput struct {
	State    string `query:"state" enum:"Pending,Approved,Denied,Expired" doc:"Only requests in this state."`
	Limit    int    `query:"limit" minimum:"0" maximum:"500" doc:"Page size (default 100)."`
	Continue string `query:"continue"`
}

type unblockListOutput struct {
	Body *UnblockRequestList
}

type unblockOutput struct {
	Body *UnblockRequest
}

type unblockApproveInput struct {
	Namespace string `path:"namespace"`
	Name      string `path:"name"`
	Body      UnblockApproval
}

type unblockDenyInput struct {
	Namespace string `path:"namespace"`
	Name      string `path:"name"`
	Body      UnblockDenial
}

type unblockDecisionOutput struct {
	Body *UnblockDecisionResult
}

// maxApprovalDuration bounds a time-limited approval.
const maxApprovalDuration = 366 * 24 * time.Hour

// registerUnblock registers /unblockrequests (docs/deploy/blocking-page.md,
// "Unblock requests").
func (s *Server) registerUnblock() {
	list := operation(http.MethodGet, "/unblockrequests", "listUnblockRequests", "List the unblock requests users made on the block page, newest first", "unblockrequests")
	list.Description = "Cluster-wide: the requests are UnblockRequest objects in the store. counts covers every request, so ?state=Pending&limit=1 is the pending count."
	register(s, list, perm{resource: "unblockrequests", verb: "read"}, s.listUnblock)
	register(s, operation(http.MethodGet, "/unblockrequests/{namespace}/{name}", "getUnblockRequest", "Get an unblock request", "unblockrequests"),
		perm{resource: "unblockrequests", verb: "read"}, func(ctx context.Context, in *nsObjectInput) (*unblockOutput, error) {
			if err := s.requireRequests(); err != nil {
				return nil, err
			}
			r := &v1alpha1.UnblockRequest{}
			if err := s.opts.Store.Get(ctx, store.Key{Kind: unblock.Kind, Namespace: in.Namespace, Name: in.Name}, r); err != nil {
				return nil, unblockError(err, in.Namespace, in.Name)
			}
			v := s.unblockView(r)
			return &unblockOutput{Body: &v}, nil
		})
	approve := operation(http.MethodPost, "/unblockrequests/{namespace}/{name}/approve", "approveUnblockRequest", "Approve an unblock request: add the allow entry", "unblockrequests")
	approve.Description = "Adds the name to the allow list of the scope (creating the list and attaching it to the policies concerned), records the decision and flushes the name from this node's response cache. " +
		"Every node recompiles its lists and flushes its local edge cache. A time-limited approval's entry is removed by the leader when it ends. " +
		"There is no per-client scope: a policy applies to ClientGroups, and a one-client group would take the client out of its own group's policy."
	approve.Errors = []int{http.StatusUnprocessableEntity}
	register(s, approve, perm{resource: "unblockrequests", verb: "decide"}, s.approveUnblock)
	deny := operation(http.MethodPost, "/unblockrequests/{namespace}/{name}/deny", "denyUnblockRequest", "Deny an unblock request (revokes an approval in force)", "unblockrequests")
	register(s, deny, perm{resource: "unblockrequests", verb: "decide"}, s.denyUnblock)
	del := operation(http.MethodDelete, "/unblockrequests/{namespace}/{name}", "deleteUnblockRequest", "Delete an unblock request", "unblockrequests")
	del.Description = "A permanent approval's entry stays on its list. A time-limited approval in force answers 409: deny it first, which removes its entry."
	register(s, withStatus(del, http.StatusNoContent), perm{resource: "unblockrequests", verb: "delete"}, s.deleteUnblock)
}

// requireRequests is the license gate of the unblock request workflow
// (license.FeatureRequests): the block page itself stays free, asking for an
// unblock and deciding on it do not.
func (s *Server) requireRequests() error {
	return s.requireFeature(license.FeatureRequests, "unblock requests")
}

// unblockService is Options.Unblock, or a store-only service when the
// node has none (tests, partial wirings).
func (s *Server) unblockService() *unblock.Service {
	if s.opts.Unblock != nil {
		return s.opts.Unblock
	}
	s.unblockOnce.Do(func() {
		s.unblockFallback = unblock.New(unblock.Options{Store: s.opts.Store, Namespace: s.opts.DefaultNamespace, Node: s.opts.NodeName, Now: s.opts.Now, Logger: s.log})
	})
	return s.unblockFallback
}

// allUnblock returns every request newest first: the index once it has
// synced, else the store.
func (s *Server) allUnblock(ctx context.Context) ([]*v1alpha1.UnblockRequest, error) {
	svc := s.unblockService()
	select {
	case <-svc.Synced():
		return svc.List(""), nil
	default:
	}
	var out []*v1alpha1.UnblockRequest
	err := s.listAll(ctx, unblock.Kind, "", func(o store.Object) {
		if r, ok := o.(*v1alpha1.UnblockRequest); ok {
			out = append(out, r)
		}
	})
	unblock.SortNewestFirst(out)
	return out, err
}

func (s *Server) listUnblock(ctx context.Context, in *unblockListInput) (*unblockListOutput, error) {
	if err := s.requireRequests(); err != nil {
		return nil, err
	}
	all, err := s.allUnblock(ctx)
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
	limit := clampLimit(in.Limit, 100, 500)
	out := &UnblockRequestList{Items: []UnblockRequest{}}
	i := 0
	for _, r := range all {
		st := r.EffectiveState()
		switch st {
		case v1alpha1.UnblockPending:
			out.Counts.Pending++
		case v1alpha1.UnblockApproved:
			out.Counts.Approved++
		case v1alpha1.UnblockDenied:
			out.Counts.Denied++
		case v1alpha1.UnblockExpired:
			out.Counts.Expired++
		}
		if in.State != "" && string(st) != in.State {
			continue
		}
		switch {
		case i < start:
		case len(out.Items) < limit:
			out.Items = append(out.Items, s.unblockView(r))
		case out.Continue == "":
			out.Continue = encodeCursor(strconv.Itoa(i))
		}
		i++
	}
	return &unblockListOutput{Body: out}, nil
}

func (s *Server) actor(ctx context.Context) string {
	p, ok := auth.PrincipalFrom(ctx)
	return actorOf(p, ok).String()
}

func (s *Server) approveUnblock(ctx context.Context, in *unblockApproveInput) (*unblockDecisionOutput, error) {
	if err := s.requireRequests(); err != nil {
		return nil, err
	}
	if err := s.readOnly(); err != nil {
		return nil, err
	}
	var d time.Duration
	if v := strings.TrimSpace(in.Body.Duration); v != "" {
		var err error
		if d, err = time.ParseDuration(v); err != nil || d <= 0 || d > maxApprovalDuration {
			return nil, errInvalid("duration: %q is not a duration between 1s and %s (empty is permanent)", v, maxApprovalDuration)
		}
	}
	res := audit.Resource{Kind: v1alpha1.KindUnblockRequest, Namespace: in.Namespace, Name: in.Name}
	out, err := s.runUnblockCommand(ctx, unblock.Target{Namespace: in.Namespace, Name: in.Name, Action: unblock.Approve}, unblock.Decision{
		Scope: in.Body.Scope, Match: in.Body.Match, Group: in.Body.Group, Duration: d, Note: in.Body.Note, Actor: s.actor(ctx),
	})
	if err != nil {
		s.recordAction(ctx, "unblockrequests.approve", res, err, nil, in.Body)
		return nil, unblockError(err, in.Namespace, in.Name)
	}
	a := out.Request.Status.Allow
	s.recordAction(ctx, "unblockrequests.approve", res, nil, nil, map[string]any{
		"domain": out.Request.Spec.Domain, "scope": a.Scope, "match": a.Match, "group": a.Group, "list": a.List, "entry": a.Entry,
		"expiresAt": a.ExpiresAt, "note": in.Body.Note, "attached": out.Attached, "skipped": out.Skipped,
	})
	body := &UnblockDecisionResult{Request: s.unblockView(out.Request), Attached: nonNil(out.Attached), Skipped: nonNil(out.Skipped)}
	body.Flushed = s.flushName(out.Request.Spec.Domain)
	return &unblockDecisionOutput{Body: body}, nil
}

func (s *Server) denyUnblock(ctx context.Context, in *unblockDenyInput) (*unblockDecisionOutput, error) {
	if err := s.requireRequests(); err != nil {
		return nil, err
	}
	if err := s.readOnly(); err != nil {
		return nil, err
	}
	res := audit.Resource{Kind: v1alpha1.KindUnblockRequest, Namespace: in.Namespace, Name: in.Name}
	out, err := s.runUnblockCommand(ctx, unblock.Target{Namespace: in.Namespace, Name: in.Name, Action: unblock.Deny}, unblock.Decision{Note: in.Body.Note, Actor: s.actor(ctx)})
	if err != nil {
		s.recordAction(ctx, "unblockrequests.deny", res, err, nil, in.Body)
		return nil, unblockError(err, in.Namespace, in.Name)
	}
	r := out.Request
	s.recordAction(ctx, "unblockrequests.deny", res, nil, nil, map[string]any{"domain": r.Spec.Domain, "note": in.Body.Note})
	body := &UnblockDecisionResult{Request: s.unblockView(r), Attached: []string{}, Skipped: []string{}}
	body.Flushed = s.flushName(r.Spec.Domain)
	return &unblockDecisionOutput{Body: body}, nil
}

func (s *Server) deleteUnblock(ctx context.Context, in *nsObjectInput) (*emptyOutput, error) {
	if err := s.requireRequests(); err != nil {
		return nil, err
	}
	if err := s.readOnly(); err != nil {
		return nil, err
	}
	res := audit.Resource{Kind: v1alpha1.KindUnblockRequest, Namespace: in.Namespace, Name: in.Name}
	out, err := s.runUnblockCommand(ctx, unblock.Target{Namespace: in.Namespace, Name: in.Name, Action: unblock.Delete}, unblock.Decision{})
	if err != nil {
		s.recordAction(ctx, "unblockrequests.delete", res, err, nil, nil)
		return nil, unblockError(err, in.Namespace, in.Name)
	}
	r := out.Request
	s.recordAction(ctx, "unblockrequests.delete", res, nil, map[string]any{"domain": r.Spec.Domain, "state": r.EffectiveState()}, nil)
	return &emptyOutput{}, nil
}

// flushName removes name from this node's response cache (every common
// type, class, flags, ECS and resolution scope), fences detached fills, and
// returns how many entries went.
func (s *Server) flushName(name string) int {
	if s.opts.ResponseCache == nil {
		return 0
	}
	return s.opts.ResponseCache.FlushName(dns.Fqdn(strings.ToLower(strings.TrimSpace(name))))
}

func unblockError(err error, ns, name string) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return errNotFound("UnblockRequest " + ns + "/" + name)
	case errors.Is(err, unblock.ErrState):
		return errConflict(err.Error(), "")
	case errors.Is(err, unblock.ErrInvalid):
		return newError(http.StatusUnprocessableEntity, KeyUnprocessable, err.Error(), "")
	}
	return storeError(err)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func timePtr(t *metav1.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := t.UTC()
	return &v
}

func (s *Server) unblockView(r *v1alpha1.UnblockRequest) UnblockRequest {
	v := UnblockRequest{
		Namespace: r.Namespace, Name: r.Name, Domain: r.Spec.Domain, Client: r.Spec.Client, ClientPrivacy: string(r.Spec.ClientPrivacy),
		Groups: nonNil(r.Spec.Groups), Policies: nonNil(r.Spec.Policies), Node: r.Spec.Node,
		RequesterName: r.Spec.RequesterName, Contact: r.Spec.Contact, Reason: r.Spec.Reason,
		CreatedAt: r.CreationTimestamp.UTC(), RequestedAt: unblock.RequestedAt(r).UTC(), Submissions: r.Spec.Submissions,
		State: string(r.EffectiveState()), DecidedBy: r.Status.DecidedBy, DecidedAt: timePtr(r.Status.DecidedAt), Note: r.Status.Note,
		Allow: allowView(r.Status.Allow), History: []UnblockDecision{}, ResourceVersion: r.ResourceVersion,
	}
	if v.ClientPrivacy == "" {
		v.ClientPrivacy = string(v1alpha1.UnblockClientFull)
	}
	if b := r.Spec.Block; b != nil {
		v.Block = &UnblockBlock{Kind: b.Kind, List: b.List, Rule: b.Rule, Policy: b.Policy}
	}
	if r.EffectiveState() == v1alpha1.UnblockPending {
		exp := unblock.RequestedAt(r).Add(s.unblockSettingsExpiry()).UTC()
		v.ExpiresAt = &exp
	}
	for _, h := range r.Status.History {
		v.History = append(v.History, UnblockDecision{State: string(h.State), DecidedBy: h.DecidedBy, DecidedAt: timePtr(h.DecidedAt), Note: h.Note, Allow: allowView(h.Allow)})
	}
	return v
}

// unblockSettingsExpiry is unblockRequests.pendingExpiry.
func (s *Server) unblockSettingsExpiry() time.Duration {
	if c := s.config(); c != nil && c.UnblockRequests.PendingExpiry > 0 {
		return c.UnblockRequests.PendingExpiry.Std()
	}
	return unblock.Defaults.PendingExpiry.Std()
}

func allowView(a *v1alpha1.UnblockAllow) *UnblockAllow {
	if a == nil {
		return nil
	}
	return &UnblockAllow{
		Scope: a.Scope, Match: a.Match, Group: a.Group, List: a.List, Entry: a.Entry, Added: a.Added,
		Policies: nonNil(a.Policies), ExpiresAt: timePtr(a.ExpiresAt), Removed: timePtr(a.Removed),
	}
}

// An installed current capability's refusal never falls back to live helpers.
func (s *Server) runUnblockCommand(ctx context.Context, target unblock.Target, d unblock.Decision) (*unblock.Outcome, error) {
	if s.opts.UnblockCommand != nil {
		out, err := s.opts.UnblockCommand(ctx, target, d)
		if err == nil && (out == nil || out.Request == nil) {
			return nil, errUnavailable("current unblock command returned no result")
		}
		return out, err
	}
	if s.opts.ObjectCommand != nil {
		return nil, errUnavailable("current unblock command unavailable")
	}
	svc := s.unblockService()
	switch target.Action {
	case unblock.Approve:
		return svc.Approve(ctx, target.Namespace, target.Name, d)
	case unblock.Deny:
		r, err := svc.Deny(ctx, target.Namespace, target.Name, d.Note, d.Actor)
		return &unblock.Outcome{Request: r}, err
	case unblock.Delete:
		r, err := svc.Delete(ctx, target.Namespace, target.Name)
		return &unblock.Outcome{Request: r}, err
	}
	return nil, errUnavailable("unsupported unblock command")
}
