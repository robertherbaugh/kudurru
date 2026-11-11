package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/auth"
	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

const memberSettingsPatch = "settings.patch"

// settingsWriteAttrs is the permission of PATCH /settings.
var settingsWriteAttrs = rbac.Attributes{Resource: "settings", Verb: "write", Scope: rbac.Scope{Kind: rbac.ScopeCluster}}

// memberSettings is the version-2 payload of one forwarded cluster settings
// patch: the route's merge patch, its optional If-Match and a random nonce.
// The dedup record keeps a fingerprint of the whole command; the unstored
// nonce keeps that fingerprint from confirming a guessed value offline.
type memberSettings struct {
	Patch   map[string]any `json:"patch"`
	IfMatch string         `json:"ifMatch,omitempty"`
	Nonce   string         `json:"nonce"`
}

// SettingsForward sends one cluster-scope PATCH /settings from an enrolled
// follower to its original primary as a settings.patch member command. The
// precondition is the resourceVersion of the settings singleton in the
// follower's own view, which the primary compares in its transaction: a patch
// is never merged into settings that the caller did not see. Nothing is
// retried; primary, standalone and static nodes run their own command.
func (s *servingRuntime) SettingsForward(ctx context.Context, in api.ForwardedSettings) (*api.SettingsBody, bool, error) {
	token, forward, err := memberForwardSource(ctx)
	if !forward || err != nil {
		return nil, forward, err
	}
	view, err := servinghttp.Component[*servingauth.View](ctx, "auth")
	if err != nil {
		return nil, true, objectForwardUnavailable("forwarding unavailable")
	}
	current, err := view.Settings()
	if err != nil || current.ResourceVersion == "" {
		return nil, true, objectForwardUnavailable("this follower's settings are unavailable; the change was not applied")
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, true, err
	}
	payload, err := json.Marshal(memberSettings{Patch: in.Patch, IfMatch: in.IfMatch, Nonce: base64.RawURLEncoding.EncodeToString(nonce[:])})
	if err != nil {
		return nil, true, err
	}
	precondition := []clusterreplica.Precondition{{Kind: "Kudurru", Name: "default", ResourceVersion: current.ResourceVersion}}
	var body api.SettingsBody
	err = s.forwardMemberCommand(ctx, token, memberSettingsPatch, settingsWriteAttrs, precondition, payload, func(raw []byte) error {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if d.Decode(&body) != nil || d.More() || body.Scope != "cluster" || body.Persisted == nil || !*body.Persisted {
			return clusterreplica.ErrProtocol
		}
		return nil
	})
	if err != nil {
		return nil, true, err
	}
	s.awaitForwardedSettings(ctx, current.ResourceVersion)
	return &body, true, nil
}

// forwardedSettingsSettle bounds how long a committed forwarded settings patch
// waits for this follower to serve it before answering.
const forwardedSettingsSettle = 5 * time.Second

// awaitForwardedSettings runs after the primary committed a forwarded settings
// patch: one explicit pull installs the primary's state and the follower
// answers once its published view no longer carries the settings version that
// the patch was made against. Without it the caller's next change through this
// follower, or a forwarded blocking switch, would carry this follower's own
// pre-commit version and the primary would refuse it as stale until the
// scheduled pull ran. The pull replays nothing. A failed pull or a publication
// that does not arrive within the bound still answers the committed result;
// the scheduled pull then installs the state.
func (s *servingRuntime) awaitForwardedSettings(ctx context.Context, seen string) {
	if s.member == nil || s.publisher == nil {
		return
	}
	limit := forwardedSettingsSettle
	if deadline, ok := ctx.Deadline(); ok {
		limit = min(limit, time.Until(deadline))
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	if s.member.Pull(ctx) != nil {
		return
	}
	s.publisher.Wake()
	for {
		changed := s.publisher.Subscribe()
		lease, err := s.publisher.CaptureHTTP(ctx)
		if err == nil {
			served := false
			if value, ok := lease.Bundle().Component("auth"); ok {
				if view, typed := value.(*servingauth.View); typed {
					current, err := view.Settings()
					served = err == nil && current.ResourceVersion != "" && current.ResourceVersion != seen
				}
			}
			lease.Release()
			if served {
				return
			}
		} else if !errors.Is(err, serving.ErrPending) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		}
	}
}

// rootMemberSettingsDomain runs one forwarded cluster settings patch on the
// primary through the public API's own validation and merge, inside the member
// command's transaction, so that the settings write, its attributed audit row
// and the dedup record commit together.
type rootMemberSettingsDomain struct {
	runtime      *servingRuntime
	deps         servingauth.Dependencies
	lifetime     *servingauth.CredentialLifetime
	in           memberSettings
	precondition string
}

func (d *rootMemberSettingsDomain) bind(deps servingauth.Dependencies) { d.deps = deps }
func (d *rootMemberSettingsDomain) output(scope context.Context, write func(context.Context) error) error {
	return credentialOutput(scope, d.lifetime, write)
}

func (d *rootMemberSettingsDomain) Authorize(ctx context.Context, tx clusterreplica.CommandTx, command clusterreplica.Command) (context.Context, error) {
	concrete, ok := tx.State().(*sqlite.CommandTx)
	if !ok || concrete == nil || command.Version != 2 || command.Operation != memberSettingsPatch || len(command.Preconditions) != 1 {
		return nil, clusterreplica.ErrProtocol
	}
	precondition := command.Preconditions[0]
	if precondition.Kind != "Kudurru" || precondition.Namespace != "" || precondition.Name != "default" || precondition.ResourceVersion == "" {
		return nil, clusterreplica.ErrProtocol
	}
	decoder := json.NewDecoder(bytes.NewReader(command.Payload))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&d.in) != nil || d.in.Patch == nil || len(d.in.Nonce) < 43 || len(d.in.IfMatch) > 1024 {
		return nil, clusterreplica.ErrProtocol
	}
	raw, err := command.Credential.Raw()
	if err != nil {
		return nil, err
	}
	principal, decision, lifetime, err := servingauth.ReauthorizeCurrentForOutput(ctx, concrete, d.deps, raw, settingsWriteAttrs, d.runtime.opts.Now)
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
	d.lifetime, d.precondition = lifetime, precondition.ResourceVersion
	actor := audit.Actor{Kind: string(principal.Kind), ID: principal.ID, Name: principal.Name, Groups: principal.Groups, Auth: principal.AuthMethod}
	ctx = auth.WithPrincipal(ctx, principal)
	ctx = sqlite.WithActor(ctx, actor.String())
	return audit.WithRequest(ctx, actor, audit.Request{ID: command.ID, Method: http.MethodPatch, Path: clusterreplica.PathWrite}, func() string { return decision.Path }), nil
}

func (d *rootMemberSettingsDomain) Execute(ctx context.Context, tx clusterreplica.CommandTx, _ clusterreplica.Command) (clusterreplica.Outcome, error) {
	concrete, ok := tx.State().(*sqlite.CommandTx)
	server := d.runtime.objectAPI.Load()
	if !ok || d.lifetime == nil || server == nil {
		return clusterreplica.Outcome{}, clusterreplica.ErrForbidden
	}
	var current public.Kudurru
	if err := concrete.Get(ctx, store.Key{Kind: "Kudurru", Name: "default"}, &current); err != nil {
		return clusterreplica.Outcome{}, err
	}
	if current.ResourceVersion != d.precondition {
		return retainedRefusal(&api.Error{Status: http.StatusConflict, Body: api.ErrorBody{Key: api.KeyConflict,
			Message: "the cluster settings changed since this follower's view; the change was not applied",
			Hint:    "reload the settings and review them before a new change"}})
	}
	// Branding needs the entitlement of this transaction's pinned license,
	// checked now and again at commit, as the primary's own route does with
	// its request's view.
	admit := func(ctx context.Context, tx *sqlite.CommandTx, plan *api.CanonicalSettingsPlan) error {
		if !plan.Branding() {
			return nil
		}
		check := func(context.Context) error { return brandingPolicyCheck(d.deps.License, d.runtime.opts.Now().UTC()) }
		if err := check(ctx); err != nil {
			return err
		}
		return tx.RequireCommitCheck(check)
	}
	body, err := server.ForwardedSettingsCommand(ctx, concrete, api.ForwardedSettings{Patch: d.in.Patch, IfMatch: d.in.IfMatch}, admit)
	if err != nil {
		if refusal, ok := api.ForwardedSettingsRefusal(err); ok {
			return retainedRefusal(refusal)
		}
		return clusterreplica.Outcome{}, err
	}
	encoded, err := json.Marshal(body)
	return clusterreplica.Outcome{Status: http.StatusOK, Body: encoded}, err
}

// brandingPolicyCheck is brandingWriteCheck for a pinned license policy: the
// entitlement holds only inside its original absolute window.
func brandingPolicyCheck(p servingauth.LicensePolicy, now time.Time) error {
	if p.Branding && !p.BrandingFrom.IsZero() && p.BrandingUntil.After(p.BrandingFrom) && !now.Before(p.BrandingFrom) && now.Before(p.BrandingUntil) {
		return nil
	}
	return &api.Error{Status: http.StatusPaymentRequired, Body: api.ErrorBody{Key: api.KeyLicense, Message: "custom branding requires an Enterprise license"}}
}

// retainedRefusal records a clean public refusal as the command's durable
// result, which the follower relays exactly.
func retainedRefusal(e *api.Error) (clusterreplica.Outcome, error) {
	body, err := json.Marshal(api.Error{Body: e.Body})
	if err != nil {
		return clusterreplica.Outcome{}, err
	}
	return clusterreplica.Outcome{Status: e.Status, Body: body}, nil
}
