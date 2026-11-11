package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/auth"
	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

const memberBlockingToggle = "blocking.toggle"

// The wire carries original absolute intent. A sender must resolve a relative
// timer before dispatch; a receiver cannot restart its duration after a delay.
type memberBlockingChange struct {
	Blocking *bool      `json:"blocking"`
	Until    *time.Time `json:"until,omitempty"`
}

// executeMemberCommand is the root-owned scoped member executor. The closed
// domains are blocking.toggle, the canonical public object writes/deletes, the
// zone convenience commands, local login, sign-out with own-session revocation
// and the cluster settings patch; status, IAM and other writes refuse. Public
// forwarding is mounted separately.
func (s *servingRuntime) executeMemberCommand(ctx context.Context, request *sqlite.MemberRequest, command clusterreplica.Command, emit func(context.Context, clusterreplica.Outcome) error) error {
	if s == nil || ctx == nil || request == nil || emit == nil {
		return clusterreplica.ErrProtocol
	}
	switch {
	case command.Version == 1 && command.Operation == memberLocalLogin:
		// A login carries the password in its payload instead of a prior proof.
	case command.Version == 2 && (command.Operation == memberBlockingToggle || command.Operation == memberObjectWrite || command.Operation == memberObjectDelete || command.Operation == memberZoneCommand ||
		command.Operation == memberLogout || command.Operation == memberSettingsPatch):
	default:
		return clusterreplica.ErrProtocol
	}
	status := s.primary.driver.Status()
	if !status.Running || !status.Ready {
		return replication.ErrFenced
	}
	original := s.primary.writer.active.Load()
	ctx, release, err := s.primary.writer.bind(ctx, status.Writer)
	if err != nil {
		return err
	}
	defer release()
	// Keep the envelope's original bound even when this callback is invoked
	// directly. The peer and credential may each shorten it further.
	ctx, cancel := context.WithDeadline(ctx, command.NotAfter)
	defer cancel()
	target := serving.TermActivation{Term: status.Writer.Term, Incarnation: status.Writer.Incarnation}
	check := func(ctx context.Context) error {
		if !s.primary.writer.current(original) {
			return replication.ErrFenced
		}
		return ctx.Err()
	}
	var outcome clusterreplica.Outcome
	domain := s.memberCommandDomain(command.Operation)
	committed, err := s.providers.WithMemberCommandOutput(ctx, target, request, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		if err := check(ctx); err != nil {
			return err
		}
		domain.bind(deps)
		service := clusterreplica.CommandService{Domain: domain, Now: s.opts.Now}
		var err error
		outcome, err = service.StageSQLite(ctx, tx, command)
		if err != nil {
			return err
		}
		return tx.RequireCommitCheck(check)
	}, func(scope context.Context) error {
		return domain.output(scope, func(output context.Context) error {
			if err := check(output); err != nil {
				return err
			}
			if err := emit(output, outcome); err != nil {
				return err
			}
			return check(output)
		})
	})
	if committed {
		s.publisher.Wake()
	}
	return err
}

// rootMemberDomain is one closed typed member-command domain.
type rootMemberDomain interface {
	clusterreplica.Domain
	bind(servingauth.Dependencies)
	// output emits under the domain's verified credential lifetime. A domain
	// without a prior credential (login) emits only under the command scope.
	output(context.Context, func(context.Context) error) error
}

func credentialOutput(scope context.Context, lifetime *servingauth.CredentialLifetime, write func(context.Context) error) error {
	if lifetime == nil {
		return clusterreplica.ErrForbidden
	}
	return lifetime.WithOutput(scope, write)
}

func (s *servingRuntime) memberCommandDomain(operation string) rootMemberDomain {
	switch operation {
	case memberObjectWrite, memberObjectDelete:
		return &rootMemberObjectDomain{runtime: s}
	case memberZoneCommand:
		return &rootMemberZoneDomain{runtime: s}
	case memberLocalLogin:
		return &rootMemberLoginDomain{runtime: s}
	case memberLogout:
		return &rootMemberLogoutDomain{runtime: s}
	case memberSettingsPatch:
		return &rootMemberSettingsDomain{runtime: s}
	}
	return &rootMemberBlockingDomain{runtime: s}
}

func (d *rootMemberBlockingDomain) bind(deps servingauth.Dependencies) { d.deps = deps }
func (d *rootMemberBlockingDomain) output(scope context.Context, write func(context.Context) error) error {
	return credentialOutput(scope, d.lifetime, write)
}
func (d *rootMemberObjectDomain) bind(deps servingauth.Dependencies) { d.deps = deps }
func (d *rootMemberObjectDomain) output(scope context.Context, write func(context.Context) error) error {
	return credentialOutput(scope, d.lifetime, write)
}

type rootMemberBlockingDomain struct {
	runtime  *servingRuntime
	deps     servingauth.Dependencies
	lifetime *servingauth.CredentialLifetime
	change   memberBlockingChange
}

func (d *rootMemberBlockingDomain) Authorize(ctx context.Context, tx clusterreplica.CommandTx, command clusterreplica.Command) (context.Context, error) {
	concrete, ok := tx.State().(*sqlite.CommandTx)
	if !ok || concrete == nil || command.Version != 2 || command.Operation != memberBlockingToggle || len(command.Preconditions) != 1 {
		return nil, clusterreplica.ErrProtocol
	}
	precondition := command.Preconditions[0]
	if precondition.Kind != "Kudurru" || precondition.Namespace != "" || precondition.Name != "default" || precondition.ResourceVersion == "" {
		return nil, clusterreplica.ErrProtocol
	}
	// CommandService already rejects duplicate fields and trailing JSON. This
	// typed decoder additionally refuses unknown or omitted intent fields.
	decoder := json.NewDecoder(bytes.NewReader(command.Payload))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&d.change) != nil || d.change.Blocking == nil || *d.change.Blocking && d.change.Until != nil {
		return nil, clusterreplica.ErrProtocol
	}
	raw, err := command.Credential.Raw()
	if err != nil {
		return nil, err
	}
	principal, decision, lifetime, err := servingauth.ReauthorizeCurrentForOutput(ctx, concrete, d.deps, raw, blockingToggleAttrs, d.runtime.opts.Now)
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
	d.lifetime = lifetime
	actor := audit.Actor{Kind: string(principal.Kind), ID: principal.ID, Name: principal.Name, Groups: principal.Groups, Auth: principal.AuthMethod}
	ctx = auth.WithPrincipal(ctx, principal)
	ctx = sqlite.WithActor(ctx, actor.String())
	return audit.WithRequest(ctx, actor, audit.Request{ID: command.ID, Method: http.MethodPost, Path: clusterreplica.PathWrite}, func() string { return decision.Path }), nil
}

func (d *rootMemberBlockingDomain) Execute(ctx context.Context, tx clusterreplica.CommandTx, command clusterreplica.Command) (clusterreplica.Outcome, error) {
	concrete, ok := tx.State().(*sqlite.CommandTx)
	if !ok || d.lifetime == nil || d.change.Blocking == nil {
		return clusterreplica.Outcome{}, clusterreplica.ErrForbidden
	}
	settings, err := blockingCommandSettings(ctx, concrete)
	if err != nil {
		return clusterreplica.Outcome{}, err
	}
	if settings.ResourceVersion != command.Preconditions[0].ResourceVersion {
		return clusterreplica.Outcome{}, replication.ErrCAS
	}
	document, err := servingSettingsDocument(settings)
	if err != nil {
		return clusterreplica.Outcome{}, err
	}
	if _, _, err := config.ApplyClusterDocument(d.runtime.opts.Config, document); err != nil {
		return clusterreplica.Outcome{}, err
	}
	change := api.BlockingChange{Blocking: *d.change.Blocking}
	if d.change.Until != nil {
		change.HasUntil, change.Until = true, *d.change.Until
	}
	status, err := blockingStatus(change, d.runtime.opts.Now())
	if err != nil {
		return clusterreplica.Outcome{}, err
	}
	if !sameBlocking(settings.Status.Blocking, status) {
		settings.Status.Blocking = status
		if err := concrete.UpdateStatus(ctx, settings); err != nil {
			return clusterreplica.Outcome{}, err
		}
	}
	settings, err = blockingCommandSettings(ctx, concrete)
	if err != nil {
		return clusterreplica.Outcome{}, err
	}
	receipt := blockingReceipt{Committed: true, SettingsUID: string(settings.UID), ResourceVersion: settings.ResourceVersion, Disabled: settings.Status.Blocking.Disabled, Publication: "pending"}
	if settings.Status.Blocking.DisabledUntil != nil {
		until := settings.Status.Blocking.DisabledUntil.UTC()
		receipt.DisabledUntil = &until
	}
	body, err := json.Marshal(receipt)
	return clusterreplica.Outcome{Status: http.StatusOK, Body: body}, err
}
