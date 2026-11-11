package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/auth"
	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

const (
	memberObjectWrite  = "objects.write"
	memberObjectDelete = "objects.delete"
	// memberForwardLifetime is the longest request lifetime that a follower
	// may capture for forwarding (sqlite.Store.CaptureMemberForward).
	memberForwardLifetime = 30 * time.Second
)

// memberForwardRoute reports whether a public request is one of the routes
// whose effect an enrolled follower sends to its primary as an objects.write,
// objects.delete or zones.command member command: the /objects routes, POST
// /apply, the /blocklists, /clientgroups and /policies routes, POST
// /{kind}:batchDelete and the four zone convenience commands. escaped is the
// request's escaped path. Segments are compared as the router compares them,
// so an encoded separator stays inside its segment.
// TestMemberForwardRouteCoversEveryForwardedRoute holds this predicate to the
// registered routes.
func memberForwardRoute(method, prefix, escaped string) bool {
	if method != http.MethodPost && method != http.MethodPut && method != http.MethodDelete {
		return false
	}
	segments := strings.Split(escaped, "/")
	for i, segment := range segments {
		unescaped, err := url.PathUnescape(segment)
		if err != nil {
			return false
		}
		segments[i] = unescaped
	}
	base := strings.Split(prefix, "/")
	if len(segments) <= len(base) || !slices.Equal(segments[:len(base)], base) || slices.Contains(segments[len(base):], "") {
		return false
	}
	parts := segments[len(base):]
	switch parts[0] {
	case "apply":
		return method == http.MethodPost && len(parts) == 1
	case "objects":
		switch method {
		case http.MethodPost:
			return len(parts) == 2
		case http.MethodPut, http.MethodDelete:
			return len(parts) == 3 || len(parts) == 4
		}
		return false
	case "blocklists", "clientgroups", "policies":
		switch method {
		case http.MethodPost:
			return len(parts) == 1
		case http.MethodPut, http.MethodDelete:
			return len(parts) == 3
		}
		return false
	case "zones":
		switch method {
		case http.MethodPost:
			return len(parts) == 3 && (parts[2] == "records" || parts[2] == "import")
		case http.MethodDelete:
			return len(parts) == 2 || len(parts) == 5 && parts[2] == "records"
		}
		return false
	}
	return method == http.MethodPost && len(parts) == 1 && strings.HasSuffix(parts[0], ":batchDelete")
}

// memberEarlierBound reports the requests that a follower bounded before
// memberForwardRoute named the forwarded routes: every POST, PUT and DELETE
// under /objects/ and /zones/. Of the registered routes only POST
// /zones/{zone}/notify is one that the predicate does not cover. A follower
// does not forward it, and it keeps the deadline that it had.
func memberEarlierBound(method, prefix, path string) bool {
	if method != http.MethodPost && method != http.MethodPut && method != http.MethodDelete {
		return false
	}
	return strings.HasPrefix(path, prefix+"/objects/") || strings.HasPrefix(path, prefix+"/zones/")
}

// boundMemberObjectWrites starts the one original 30-second lifetime of an
// enrolled follower's forwarded mutation before Huma acquires its body. A
// request with several items (POST /apply, :batchDelete) sends one command for
// each item inside this single lifetime. The other unsafe requests under
// /objects/ and /zones/ keep the same bound, which they had before the
// forwarded routes were named. Primary, standalone and static nodes keep their
// ordinary route lifetime, as does every other route on a follower.
func (h *servingHTTP) boundMemberObjectWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !memberForwardRoute(r.Method, h.opts.RoutePrefix, r.URL.EscapedPath()) && !memberEarlierBound(r.Method, h.opts.RoutePrefix, r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		token, err := servinghttp.SourceToken(r.Context())
		if err != nil || token.Authority().Mode != replication.Follower {
			next.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), memberForwardLifetime)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// memberObjectAttributes is the follower's early admission and the primary's
// envelope-level subject check. The primary's command body still authorizes
// the exact old/new scopes of the object it actually reads.
func memberObjectAttributes(in api.ForwardedObject, existing store.Object) (rbac.Attributes, bool) {
	key := store.Key{Kind: store.Kind(in.Kind), Namespace: in.Namespace, Name: in.Name}
	resource := objectCommandResource(key)
	if resource == "" {
		return rbac.Attributes{}, false
	}
	verb := "write"
	if in.Operation == api.ForwardedDelete {
		verb = "delete"
	}
	scope := rbac.Scope{Kind: rbac.ScopeCluster}
	switch in.Kind {
	case "DNSZone":
		if existing != nil || in.Operation == api.ForwardedDelete {
			scope = rbac.Scope{Kind: rbac.ScopeZone, Name: in.Name}
		}
	case "DNSRecordSet":
		zone := ""
		if in.Operation == api.ForwardedWrite {
			var doc struct {
				Spec struct {
					ZoneRef struct {
						Name string `json:"name"`
					} `json:"zoneRef"`
				} `json:"spec"`
			}
			if json.Unmarshal(in.Document, &doc) == nil {
				zone = doc.Spec.ZoneRef.Name
			}
		} else if records, ok := existing.(*public.DNSRecordSet); ok {
			zone = records.Spec.ZoneRef.Name
		}
		if zone != "" {
			scope = rbac.Scope{Kind: rbac.ScopeZone, Name: zone}
		}
	}
	return rbac.Attributes{Resource: resource, Verb: verb, Scope: scope, Namespace: in.Namespace, Name: in.Name}, true
}

// memberForwardSource returns the follower source that a public mutation began
// on. forward=false means that this node runs its own command. A request that
// began on a follower stays a forward after its context has ended: it is
// refused with that cause and never handed to the local command, whose
// credential check would report an expired session for a write that only ran
// out of time.
func memberForwardSource(ctx context.Context) (sqlite.GenerationToken, bool, error) {
	token, err := servinghttp.SourceToken(ctx)
	if err == nil {
		return token, token.Authority().Mode == replication.Follower, nil
	}
	if ctx == nil || ctx.Err() == nil {
		return sqlite.GenerationToken{}, false, nil
	}
	if mode, err := servinghttp.SourceMode(ctx); err != nil || mode != replication.Follower {
		return sqlite.GenerationToken{}, false, nil
	}
	return sqlite.GenerationToken{}, true, memberCaptureError(ctx, ctx.Err())
}

// ObjectForward sends one normalized public object mutation from an enrolled
// follower to its original primary over member mTLS. It never retries, never
// redirects and never falls back to a local command. The receipt is the
// primary's commit; the follower serves it only after a later pull installs it.
func (s *servingRuntime) ObjectForward(ctx context.Context, in api.ForwardedObject) (api.ForwardedObjectResult, bool, error) {
	token, forward, err := memberForwardSource(ctx)
	if !forward || err != nil {
		return api.ForwardedObjectResult{}, forward, err
	}
	operation := map[string]string{api.ForwardedWrite: memberObjectWrite, api.ForwardedDelete: memberObjectDelete}[in.Operation]
	var existing store.Object
	if objects, err := servinghttp.Component[*zonecache.Snapshot](ctx, "objects"); err == nil && objects != nil {
		existing, _ = objects.Objects.Get(store.Key{Kind: store.Kind(in.Kind), Namespace: in.Namespace, Name: in.Name})
	}
	attrs, ok := memberObjectAttributes(in, existing)
	if !ok || operation == "" {
		// No typed command carries this kind (DHCPScope): the same refusal as
		// every other mutation that a follower does not forward.
		return api.ForwardedObjectResult{}, true, api.PrimaryRequired(token.Authority().Term.PrimaryName)
	}
	payload, err := json.Marshal(in)
	if err != nil {
		return api.ForwardedObjectResult{}, true, err
	}
	var result api.ForwardedObjectResult
	err = s.forwardMemberCommand(ctx, token, operation, attrs, payload, func(body []byte) error {
		d := json.NewDecoder(bytes.NewReader(body))
		d.DisallowUnknownFields()
		if d.Decode(&result) != nil || (in.Operation == api.ForwardedDelete) != (result.Result == "deleted") {
			return clusterreplica.ErrProtocol
		}
		return nil
	})
	return result, true, err
}

// forwardMemberCommand sends one typed command for a follower's public request
// and accepts either the primary's committed result (accept) or its exact
// relayed public refusal. Admission, source capture and dependency pins are
// those of the original blocking forward; nothing is retried.
func (s *servingRuntime) forwardMemberCommand(ctx context.Context, token sqlite.GenerationToken, operation string, attrs rbac.Attributes, payload []byte, accept func([]byte) error) error {
	if s.memberForward == nil || s.opts.Config.Cluster.Role == "static" {
		return objectForwardUnavailable("writes on this follower require its enrolled primary")
	}
	view, err := servinghttp.Component[*servingauth.View](ctx, "auth")
	if err != nil {
		return objectForwardUnavailable("forwarding unavailable")
	}
	raw, err := servinghttp.Credential(ctx)
	if err != nil {
		return err
	}
	request, err := s.memberForward.Capture(ctx, token)
	if err != nil {
		return memberCaptureError(ctx, err)
	}
	issued := s.opts.Now().UTC()
	commandID := uuid.NewString()
	var refusal *api.Error
	sent := false
	err = s.withMemberForward(ctx, view, raw, attrs, func(ctx context.Context, principal auth.Principal, _ *servingauth.CredentialLifetime, current func(context.Context) error) error {
		proof, err := clusterreplica.NewCredentialProof(raw)
		if err != nil {
			return err
		}
		reference, err := proof.Reference()
		if err != nil {
			return err
		}
		kind := map[servingauth.CredentialKind]string{servingauth.APIToken: "api-token", servingauth.Cookie: "session", servingauth.OIDCJWT: "oidc"}[raw.Kind]
		deadline, _ := ctx.Deadline()
		command := clusterreplica.Command{Format: clusterreplica.CommandFormat, Version: 2, ID: commandID,
			ExpectedTerm: request.OriginalTerm(), Hops: 1, IssuedAt: issued, NotAfter: deadline, Operation: operation,
			Subject:    clusterreplica.SubjectRef{Kind: principal.Kind, ID: principal.ID, Provider: principal.Provider, CredentialKind: kind, CredentialID: reference, IssuedAt: principal.CredentialIssuedAt},
			Credential: proof, Payload: payload}
		received, err := s.memberForward.Forward(ctx, request, command, func(scope context.Context, out clusterreplica.Outcome) error {
			if err := current(scope); err != nil {
				return err
			}
			if out.Status == http.StatusOK {
				return accept(out.Body)
			}
			var relayed api.Error
			if out.Status < 400 || out.Status >= 500 || json.Unmarshal(out.Body, &relayed) != nil || relayed.Body.Key == "" {
				return clusterreplica.ErrProtocol
			}
			relayed.Status = out.Status
			refusal = &relayed
			return nil
		})
		sent = received
		return err
	})
	if err != nil {
		return objectForwardError(err, sent)
	}
	if refusal != nil {
		return refusal
	}
	return nil
}

func objectForwardUnavailable(message string) error {
	return &api.Error{Status: http.StatusServiceUnavailable, Body: api.ErrorBody{Key: api.KeyUnavailable, Message: message}}
}

// memberCaptureError explains why a follower could not bind its request for
// forwarding. Capture sends nothing, so in every case the write was not
// applied; only a changed source needs a review of current state first.
func memberCaptureError(ctx context.Context, err error) error {
	deadline, finite := ctx.Deadline()
	switch {
	case errors.Is(err, replication.ErrCAS):
		return objectForwardUnavailable("the follower's source changed; review current state before a new decision")
	case errors.Is(ctx.Err(), context.Canceled):
		return objectForwardUnavailable("the request was canceled before this write was sent; it was not applied")
	case !finite || time.Until(deadline) > memberForwardLifetime:
		// boundMemberObjectWrites bounds every forwarded route, so this request
		// came through a route that a follower does not forward.
		return objectForwardUnavailable("this route is not forwarded from a follower; the write was not applied; send it to the enrolled primary")
	case ctx.Err() != nil || !time.Now().Before(deadline):
		return objectForwardUnavailable("the request's 30-second forwarding lifetime ended before this write was sent; it was not applied")
	default:
		return objectForwardUnavailable("this follower cannot forward writes now; the write was not applied")
	}
}

// objectForwardError keeps clean primary refusals distinct from an uncertain
// outcome. Nothing is retried; an unknown result needs a fresh observation.
func objectForwardError(err error, sent bool) error {
	switch {
	case errors.Is(err, clusterreplica.ErrForbidden), errors.Is(err, servingauth.ErrDenied):
		return &api.Error{Status: http.StatusForbidden, Body: api.ErrorBody{Key: api.KeyForbidden, Message: "the primary refused this credential for the object"}}
	case errors.Is(err, clusterreplica.ErrUnauthorized):
		return &api.Error{Status: http.StatusUnauthorized, Body: api.ErrorBody{Key: api.KeyUnauthenticated, Message: "the primary did not accept this credential"}}
	case errors.Is(err, replication.ErrFenced), errors.Is(err, replication.ErrCAS), errors.Is(err, replication.ErrReadOnly), errors.Is(err, replication.ErrPromotionConflict), errors.Is(err, clusterreplica.ErrExpired):
		return objectForwardUnavailable("cluster authority changed; review current state before a new decision")
	case !sent && !errors.Is(err, clusterreplica.ErrOutcomeUnknown):
		return objectForwardUnavailable("the enrolled primary is unavailable; the write was not applied")
	default:
		return &api.Error{Status: http.StatusServiceUnavailable, Body: api.ErrorBody{Key: "completion_unknown",
			Message: "the forwarded write's outcome is unknown; inspect the object before a new decision; do not replay this request"}}
	}
}

// rootMemberObjectDomain runs a forwarded object mutation through the same
// command body as the primary's public route, inside the member command's
// transaction so that effect, audit and dedup commit together.
type rootMemberObjectDomain struct {
	runtime  *servingRuntime
	deps     servingauth.Dependencies
	lifetime *servingauth.CredentialLifetime
	in       api.ForwardedObject
	raw      servingauth.Credential
}

func (d *rootMemberObjectDomain) Authorize(ctx context.Context, tx clusterreplica.CommandTx, command clusterreplica.Command) (context.Context, error) {
	concrete, ok := tx.State().(*sqlite.CommandTx)
	if !ok || concrete == nil || command.Version != 2 || (command.Operation != memberObjectWrite && command.Operation != memberObjectDelete) || len(command.Preconditions) != 0 {
		return nil, clusterreplica.ErrProtocol
	}
	decoder := json.NewDecoder(bytes.NewReader(command.Payload))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&d.in) != nil || (command.Operation == memberObjectWrite) != (d.in.Operation == api.ForwardedWrite) {
		return nil, clusterreplica.ErrProtocol
	}
	key := store.Key{Kind: store.Kind(d.in.Kind), Namespace: d.in.Namespace, Name: d.in.Name}
	var existing store.Object
	if newObject := publicObjectFor(key.Kind); newObject != nil {
		if err := concrete.Get(ctx, key, newObject); err == nil {
			existing = newObject
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
	}
	attrs, ok := memberObjectAttributes(d.in, existing)
	if !ok {
		return nil, clusterreplica.ErrProtocol
	}
	raw, err := command.Credential.Raw()
	if err != nil {
		return nil, err
	}
	principal, decision, lifetime, err := servingauth.ReauthorizeCurrentForOutput(ctx, concrete, d.deps, raw, attrs, d.runtime.opts.Now)
	if err != nil {
		return nil, err
	}
	reference, err := command.Credential.Reference()
	if err != nil {
		return nil, err
	}
	claim := command.Subject
	if principal.Kind != claim.Kind || principal.ID != claim.ID || principal.Provider != claim.Provider || reference != claim.CredentialID || !principal.CredentialIssuedAt.Equal(claim.IssuedAt) {
		return nil, clusterreplica.ErrForbidden
	}
	d.lifetime, d.raw = lifetime, raw
	actor := audit.Actor{Kind: string(principal.Kind), ID: principal.ID, Name: principal.Name, Groups: principal.Groups, Auth: principal.AuthMethod}
	ctx = auth.WithPrincipal(ctx, principal)
	ctx = sqlite.WithActor(ctx, actor.String())
	method := http.MethodPut
	if d.in.Operation == api.ForwardedDelete {
		method = http.MethodDelete
	}
	return audit.WithRequest(ctx, actor, audit.Request{ID: command.ID, Method: method, Path: clusterreplica.PathWrite}, func() string { return decision.Path }), nil
}

func (d *rootMemberObjectDomain) Execute(ctx context.Context, tx clusterreplica.CommandTx, _ clusterreplica.Command) (clusterreplica.Outcome, error) {
	concrete, ok := tx.State().(*sqlite.CommandTx)
	server := d.runtime.objectAPI.Load()
	if !ok || d.lifetime == nil || server == nil {
		return clusterreplica.Outcome{}, clusterreplica.ErrForbidden
	}
	key, run, err := server.ForwardedObjectCommand(d.in)
	if err != nil {
		return refusalOutcome(err)
	}
	resource := objectCommandResource(key)
	if resource == "" {
		return clusterreplica.Outcome{}, clusterreplica.ErrProtocol
	}
	// The exact old/new scope check, as in the primary's own ObjectCommand.
	var attempted, authorized, poisoned atomic.Bool
	authorize := func(ctx context.Context, attrs []rbac.Attributes) error {
		if !attempted.CompareAndSwap(false, true) || !objectCommandAttributes(key, resource, attrs) {
			poisoned.Store(true)
			return servingauth.ErrDenied
		}
		if _, err := servingauth.ReauthorizeCurrentAllWithClock(ctx, concrete, d.deps, d.raw, attrs, d.runtime.opts.Now); err != nil {
			poisoned.Store(true)
			return err
		}
		authorized.Store(true)
		return nil
	}
	result, err := run(ctx, concrete, authorize)
	if err != nil {
		if poisoned.Load() {
			return clusterreplica.Outcome{}, clusterreplica.ErrForbidden
		}
		return refusalOutcome(err)
	}
	if !authorized.Load() || poisoned.Load() {
		return clusterreplica.Outcome{}, clusterreplica.ErrForbidden
	}
	check := func(context.Context) error {
		if !authorized.Load() || poisoned.Load() {
			return servingauth.ErrDenied
		}
		return nil
	}
	if err := concrete.RequireCommitCheck(check); err != nil {
		return clusterreplica.Outcome{}, err
	}
	body, err := json.Marshal(result)
	return clusterreplica.Outcome{Status: http.StatusOK, Body: body}, err
}

// refusalOutcome records a clean pre-mutation client refusal as the command's
// durable result so that the follower relays the primary's exact public error.
// Every other failure rolls back the whole command.
func refusalOutcome(err error) (clusterreplica.Outcome, error) {
	e, ok := api.ForwardedObjectRefusal(err)
	if !ok {
		return clusterreplica.Outcome{}, err
	}
	body, mErr := json.Marshal(api.Error{Body: e.Body})
	if mErr != nil {
		return clusterreplica.Outcome{}, mErr
	}
	return clusterreplica.Outcome{Status: e.Status, Body: body}, nil
}

func publicObjectFor(kind store.Kind) store.Object {
	switch kind {
	case "DNSZone":
		return &public.DNSZone{}
	case "DNSRecordSet":
		return &public.DNSRecordSet{}
	case "BlockList":
		return &public.BlockList{}
	case "ClientGroup":
		return &public.ClientGroup{}
	case "Policy":
		return &public.Policy{}
	}
	return nil
}
