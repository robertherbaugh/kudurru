package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/auth"
	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

const memberZoneCommand = "zones.command"

// memberZoneAttributes is the envelope-level admission of one zone command.
// Each plan still states and reauthorizes its own exact requirements.
func memberZoneAttributes(in api.ForwardedZone) (rbac.Attributes, bool) {
	target := api.ZoneCommandTarget{Zone: in.Zone, Action: in.Action}
	if !zoneCommandTarget(target) {
		return rbac.Attributes{}, false
	}
	resource, verb := "records", "write"
	switch in.Action {
	case api.ZoneCommandDeleteRecord:
		verb = "delete"
	case api.ZoneCommandDeleteZone:
		resource, verb = "zones", "delete"
	}
	return rbac.Attributes{Resource: resource, Verb: verb, Scope: rbac.Scope{Kind: rbac.ScopeZone, Name: in.Zone}, Name: in.Zone}, true
}

// ZoneForward sends one zone convenience command from an enrolled follower to
// its original primary. Primary, standalone and static nodes run locally.
func (s *servingRuntime) ZoneForward(ctx context.Context, in api.ForwardedZone) (json.RawMessage, bool, error) {
	token, err := servinghttp.SourceToken(ctx)
	if err != nil || token.Authority().Mode != replication.Follower {
		return nil, false, nil
	}
	attrs, ok := memberZoneAttributes(in)
	if !ok {
		return nil, true, objectForwardUnavailable("this zone command cannot be forwarded")
	}
	payload, err := json.Marshal(in)
	if err != nil {
		return nil, true, err
	}
	var result json.RawMessage
	err = s.forwardMemberCommand(ctx, token, memberZoneCommand, attrs, payload, func(body []byte) error {
		var probe any
		if json.Unmarshal(body, &probe) != nil {
			return clusterreplica.ErrProtocol
		}
		result = append(json.RawMessage(nil), body...)
		return nil
	})
	return result, true, err
}

// rootMemberZoneDomain runs a forwarded zone command through the primary's own
// route function. Its plan executes inside the member command transaction with
// the same sealed-plan admission as ZoneCommand.
type rootMemberZoneDomain struct {
	runtime  *servingRuntime
	deps     servingauth.Dependencies
	lifetime *servingauth.CredentialLifetime
	in       api.ForwardedZone
	raw      servingauth.Credential
}

func (d *rootMemberZoneDomain) bind(deps servingauth.Dependencies) { d.deps = deps }
func (d *rootMemberZoneDomain) outputLifetime() *servingauth.CredentialLifetime {
	return d.lifetime
}

func (d *rootMemberZoneDomain) Authorize(ctx context.Context, tx clusterreplica.CommandTx, command clusterreplica.Command) (context.Context, error) {
	concrete, ok := tx.State().(*sqlite.CommandTx)
	if !ok || concrete == nil || command.Version != 2 || command.Operation != memberZoneCommand || len(command.Preconditions) != 0 {
		return nil, clusterreplica.ErrProtocol
	}
	decoder := json.NewDecoder(bytes.NewReader(command.Payload))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&d.in) != nil {
		return nil, clusterreplica.ErrProtocol
	}
	attrs, ok := memberZoneAttributes(d.in)
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
	return audit.WithRequest(ctx, actor, audit.Request{ID: command.ID, Method: http.MethodPost, Path: clusterreplica.PathWrite}, func() string { return decision.Path }), nil
}

func (d *rootMemberZoneDomain) Execute(ctx context.Context, tx clusterreplica.CommandTx, _ clusterreplica.Command) (clusterreplica.Outcome, error) {
	concrete, ok := tx.State().(*sqlite.CommandTx)
	server := d.runtime.objectAPI.Load()
	if !ok || d.lifetime == nil || server == nil {
		return clusterreplica.Outcome{}, clusterreplica.ErrForbidden
	}
	var executed, applying, completed, denied atomic.Bool
	exec := func(ctx context.Context, target api.ZoneCommandTarget, prepare api.ZoneCommandPrepare) error {
		if target.Zone != d.in.Zone || target.Action != d.in.Action || !executed.CompareAndSwap(false, true) {
			denied.Store(true)
			return servingauth.ErrDenied
		}
		plan, err := prepare(ctx, concrete)
		if plan != nil {
			defer plan.Release()
		}
		if err != nil {
			return err
		}
		attrs := plan.Requirements()
		if !plan.PreparedFor(concrete, target) || !zoneCommandRequirements(target, attrs) {
			denied.Store(true)
			return servingauth.ErrDenied
		}
		if _, err := servingauth.ReauthorizeCurrentAllWithClock(ctx, concrete, d.deps, d.raw, attrs, d.runtime.opts.Now); err != nil {
			denied.Store(true)
			return err
		}
		applying.Store(true)
		if err := plan.Apply(ctx, concrete); err != nil {
			return err
		}
		completed.Store(true)
		return nil
	}
	body, err := server.ForwardedZoneCommand(ctx, d.in, exec)
	if denied.Load() {
		return clusterreplica.Outcome{}, clusterreplica.ErrForbidden
	}
	// A plan that began applying and did not complete may have staged part of
	// its mutations; only full rollback is safe, never a retained refusal.
	if applying.Load() && !completed.Load() {
		if err == nil {
			err = clusterreplica.ErrProtocol
		}
		return clusterreplica.Outcome{}, err
	}
	if err != nil {
		return refusalOutcome(err)
	}
	if !completed.Load() {
		return clusterreplica.Outcome{}, errors.Join(clusterreplica.ErrForbidden, servingauth.ErrDenied)
	}
	check := func(context.Context) error {
		if !completed.Load() || denied.Load() {
			return servingauth.ErrDenied
		}
		return nil
	}
	if err := concrete.RequireCommitCheck(check); err != nil {
		return clusterreplica.Outcome{}, err
	}
	return clusterreplica.Outcome{Status: http.StatusOK, Body: body}, nil
}
