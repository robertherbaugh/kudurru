package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

// IntegrationAdministration is available only with the complete original
// request/current-command runtime. A nil capability cannot launch a provider.
type IntegrationAdministration interface {
	IntegrationList(context.Context, string) ([]IntegrationSummary, error)
	IntegrationGet(context.Context, string, string) (*IntegrationSummary, error)
	IntegrationExecute(context.Context, IntegrationExecution) (*IntegrationExecutionResult, error)
	// IntegrationRelease ends the cleanup of one exact terminating
	// Integration without contacting its provider (D367).
	IntegrationRelease(context.Context, string, string, IntegrationReview) (*IntegrationReleaseResult, error)
	// IntegrationManifests describes the provider types this node's host
	// runs and what it refuses on its driver (D400). It reads no store and
	// resolves no credential.
	IntegrationManifests(context.Context) (*IntegrationManifests, error)
}

// IntegrationManifests is the answer of GET /integrations/manifests: what a
// form needs to write an Integration this node's host would run (D400).
type IntegrationManifests struct {
	// Driver is the store driver of this node: sqlite or kube.
	Driver string `json:"driver" enum:"sqlite,kube"`
	// Namespace is the only namespace whose Integrations the host runs, ""
	// when any namespace runs (SQLite). On Kubernetes it is the release
	// namespace; an Integration elsewhere is refused on its object.
	Namespace string `json:"namespace,omitempty"`
	// Licensed reports whether the license covers integrations now. Without
	// it a new Integration is refused (402) and existing ones pause.
	Licensed bool `json:"licensed"`
	// CredentialSources are the reference forms this node resolves for an
	// Integration: file and env, each only while this node's
	// credentials.allowedFiles or credentials.allowedEnvironment names one.
	// A secretRef is not resolved for Integrations on either driver.
	CredentialSources []string `json:"credentialSources" nullable:"false"`
	// AllowedFiles and AllowedEnvironment are this node's credential grants.
	// They are names, never values, and are present only for a caller that
	// holds integrations.secrets, the permission that binds them.
	AllowedFiles       []string `json:"allowedFiles,omitempty"`
	AllowedEnvironment []string `json:"allowedEnvironment,omitempty"`
	// Items has one entry per provider type compiled into this build.
	Items []IntegrationManifest `json:"items" nullable:"false"`
}

// IntegrationManifest describes one provider type.
type IntegrationManifest struct {
	Type         string   `json:"type"`
	DisplayName  string   `json:"displayName"`
	Version      string   `json:"version"`
	Capabilities []string `json:"capabilities" nullable:"false" doc:"RecordSink, ClientSource, Hook, Actions."`
	IOClass      string   `json:"ioClass"`
	// ConfigField is the spec field that holds the provider block, for
	// example cloudflare for spec.cloudflare.
	ConfigField string `json:"configField"`
	// ConfigSchema is the JSON Schema of the provider block.
	ConfigSchema json.RawMessage `json:"configSchema"`
	// Credentials are the spec.credentials keys the provider reads. Each
	// value in an Integration is a reference (file or env), never a value.
	Credentials []IntegrationCredentialKey `json:"credentials" nullable:"false"`
	Docs        string                     `json:"docs,omitempty"`
	// Supported is false when this node's host refuses every instance of
	// the type (a webhook on Kubernetes); Reason is the status reason the
	// instance would show.
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
	// Refused lists spec fields that, when set, make the host refuse the
	// instance on this node (for example spec.unifi.push).
	Refused []string `json:"refused" nullable:"false"`
}

// IntegrationCredentialKey is one credential a provider reads.
type IntegrationCredentialKey struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
	// Verify names what check calls to prove it, when it is checked.
	Verify string `json:"verify,omitempty"`
}

// IntegrationSummary deliberately excludes credential references and private
// provider/ledger identities. Known status must match this exact revision.
//
// Terminating is read from the stored object, not from the host's status: it
// is true once a delete was requested (DeletionRequestedAt) and the object is
// held by a finalizer, so it is shown even before the host reports Phase
// Terminating. Remaining, Blocked and Uncertain are the counts of the known
// Terminating status (record sets still to remove, conflicts that block,
// uncertain intents); they are zero otherwise.
type IntegrationSummary struct {
	Namespace           string     `json:"namespace"`
	Name                string     `json:"name"`
	UID                 string     `json:"uid"`
	Revision            string     `json:"revision"`
	Type                string     `json:"type"`
	Suspended           bool       `json:"suspended"`
	DeletionPolicy      string     `json:"deletionPolicy" enum:"Retain,Delete"`
	Terminating         bool       `json:"terminating"`
	DeletionRequestedAt *time.Time `json:"deletionRequestedAt,omitempty"`
	Remaining           int        `json:"remaining"`
	Blocked             int        `json:"blocked"`
	Uncertain           int        `json:"uncertain"`
	StatusKnown         bool       `json:"statusKnown"`
	Phase               string     `json:"phase"`
	Reason              string     `json:"reason"`
	Leader              bool       `json:"leader"`
	LastSync            *time.Time `json:"lastSync,omitempty"`
}

// IntegrationReleaseResult acknowledges a committed release. The object is
// gone; what it published and its private ledger stay. LedgerRows and
// Operations count what was retained for the released incarnation, and are
// -1 when the store held more rows than one command scans.
type IntegrationReleaseResult struct {
	Released   bool `json:"released"`
	LedgerRows int  `json:"ledgerRows"`
	Operations int  `json:"operations"`
}

// IntegrationReview is an operator's exact review, never a request to refresh.
type IntegrationReview struct {
	UID           string          `json:"uid" minLength:"1" maxLength:"128"`
	Revision      string          `json:"revision" minLength:"1" maxLength:"256"`
	Mode          string          `json:"mode,omitempty" enum:"publish,observe"`
	PreviewDigest string          `json:"previewDigest,omitempty" maxLength:"64"`
	Params        json.RawMessage `json:"params,omitempty"`
}

// IntegrationExecution selects a declared command; action is only an already
// advertised provider capability. No endpoint or credential override is accepted.
type IntegrationExecution struct {
	Namespace, Name, Command, Action string
	Review                           IntegrationReview
}

// IntegrationRecord is the DNS projection of a plan, without remote row IDs,
// versions, provider-specific properties or raw response envelopes.
type IntegrationRecord struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	TTL     int64    `json:"ttl"`
	Targets []string `json:"targets" nullable:"false"`
}

// IntegrationPlanOperation exposes one DNS change without remote ownership IDs.
type IntegrationPlanOperation struct {
	Domains []string           `json:"domains" nullable:"false"`
	Name    string             `json:"name"`
	Type    string             `json:"type"`
	Action  string             `json:"action"`
	Policy  string             `json:"policy"`
	Before  *IntegrationRecord `json:"before,omitempty"`
	After   *IntegrationRecord `json:"after,omitempty"`
}

// IntegrationPlanConflict identifies a conflicting DNS owner in public domains.
type IntegrationPlanConflict struct {
	Domains []string `json:"domains" nullable:"false"`
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Reason  string   `json:"reason"`
}

// IntegrationPreview binds all bounded public changes to one exact review digest.
type IntegrationPreview struct {
	Digest     string                     `json:"digest"`
	Operations []IntegrationPlanOperation `json:"operations" nullable:"false"`
	Conflicts  []IntegrationPlanConflict  `json:"conflicts" nullable:"false"`
	Unchanged  int                        `json:"unchanged"`
}

// IntegrationAction describes an action gated by integrations.act.
type IntegrationAction struct {
	Name   string          `json:"name"`
	Params json.RawMessage `json:"params"`
}

// IntegrationExecutionResult reports acknowledged work only. On uncertain
// failure, any confirmed partial outcome is attached to the error's safe hint.
type IntegrationExecutionResult struct {
	Completed       bool                `json:"completed"`
	Uncertain       bool                `json:"uncertain"`
	Reachable       bool                `json:"reachable,omitempty"`
	Authenticated   bool                `json:"authenticated,omitempty"`
	Preview         *IntegrationPreview `json:"preview,omitempty"`
	Actions         []IntegrationAction `json:"actions,omitempty"`
	Applied         int                 `json:"applied"`
	Unchanged       int                 `json:"unchanged"`
	Pending         int                 `json:"pending"`
	Conflicts       int                 `json:"conflicts"`
	Clients         int                 `json:"clients"`
	SourceCommitted bool                `json:"sourceCommitted"`
	SourcePublished bool                `json:"sourcePublished"`
	// Result is the bounded answer of an acknowledged action, such as a
	// UniFi dhcp-dns-servers plan with its review digest (D401).
	Result json.RawMessage `json:"result,omitempty"`
}

type integrationListInput struct {
	Namespace string `query:"namespace" required:"true" minLength:"1" maxLength:"63"`
}
type integrationListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Items []IntegrationSummary `json:"items" nullable:"false"`
	}
}

// IntegrationPath holds exported path fields for the request decoder.
type IntegrationPath struct {
	Namespace string `path:"namespace" minLength:"1" maxLength:"63"`
	Name      string `path:"name" minLength:"1" maxLength:"253"`
}
type integrationGetOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         *IntegrationSummary
}
type integrationCommandInput struct {
	IntegrationPath
	Body IntegrationReview
}
type integrationActionsInput struct {
	IntegrationPath
	UID      string `query:"uid" required:"true" minLength:"1" maxLength:"128"`
	Revision string `query:"revision" required:"true" minLength:"1" maxLength:"256"`
}
type integrationActInput struct {
	IntegrationPath
	Action string `path:"action" minLength:"1" maxLength:"128"`
	Body   IntegrationReview
}
type integrationReleaseInput struct {
	IntegrationPath
	Body IntegrationReview
}
type integrationReleaseOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         *IntegrationReleaseResult
}
type integrationExecutionOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         *IntegrationExecutionResult
}

type integrationManifestsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         *IntegrationManifests
}

func (s *Server) registerIntegrations() {
	read := perm{resource: "integrations", verb: "read"}
	manifests := integrationOperation(http.MethodGet, "/integrations/manifests", "listIntegrationManifests", "List provider manifests and this node's integration limits", "integrations")
	manifests.Description = "Provider types, their configuration schema and credential keys, and what this node's host refuses. " +
		"Integrations themselves are written as objects: /objects/integrations (integrations.write, integrations.delete; " +
		"binding or changing spec.credentials also needs integrations.secrets) (D400)."
	register(s, manifests, read, func(ctx context.Context, _ *struct{}) (*integrationManifestsOutput, error) {
		if s.opts.Integrations == nil {
			return nil, errUnavailable("complete integration administration unavailable")
		}
		out, err := s.opts.Integrations.IntegrationManifests(ctx)
		if err != nil {
			return nil, err
		}
		if out == nil {
			return nil, errUnavailable("integration manifests unavailable")
		}
		copied := *out
		if !s.allowed(ctx, "integrations", "secrets", clusterScope) {
			copied.AllowedFiles, copied.AllowedEnvironment = nil, nil
		}
		return &integrationManifestsOutput{CacheControl: "no-store", Body: &copied}, nil
	})
	register(s, integrationOperation(http.MethodGet, "/integrations", "listIntegrations", "List bounded integration status", "integrations"), read, func(ctx context.Context, in *integrationListInput) (*integrationListOutput, error) {
		if s.opts.Integrations == nil {
			return nil, errIntegrationsUnavailable()
		}
		items, err := s.opts.Integrations.IntegrationList(ctx, in.Namespace)
		if err != nil {
			return nil, err
		}
		out := &integrationListOutput{CacheControl: "no-store"}
		out.Body.Items = items
		return out, nil
	})
	base := "/integrations/{namespace}/{name}"
	register(s, integrationOperation(http.MethodGet, base, "getIntegration", "Read exact integration status", "integrations"), read, func(ctx context.Context, in *IntegrationPath) (*integrationGetOutput, error) {
		if s.opts.Integrations == nil {
			return nil, errIntegrationsUnavailable()
		}
		value, err := s.opts.Integrations.IntegrationGet(ctx, in.Namespace, in.Name)
		if err != nil {
			return nil, err
		}
		return &integrationGetOutput{CacheControl: "no-store", Body: value}, nil
	})
	for _, cmd := range []struct{ path, id, verb string }{{"check", "checkIntegration", "check"}, {"preview", "previewIntegration", "read"}, {"sync", "syncIntegration", "sync"}} {
		op := integrationOperation(http.MethodPost, base+"/"+cmd.path, cmd.id, "Run a reviewed integration "+cmd.path, "integrations")
		op.MaxBodyBytes = 4096
		register(s, op, perm{resource: "integrations", verb: cmd.verb}, func(ctx context.Context, in *integrationCommandInput) (*integrationExecutionOutput, error) {
			return s.executeIntegration(ctx, IntegrationExecution{Namespace: in.Namespace, Name: in.Name, Command: cmd.path, Review: in.Body})
		})
	}
	register(s, integrationOperation(http.MethodGet, base+"/actions", "listIntegrationActions", "List original advertised actions", "integrations"), read, func(ctx context.Context, in *integrationActionsInput) (*integrationExecutionOutput, error) {
		return s.executeIntegration(ctx, IntegrationExecution{Namespace: in.Namespace, Name: in.Name, Command: "actions", Review: IntegrationReview{UID: in.UID, Revision: in.Revision}})
	})
	op := integrationOperation(http.MethodPost, base+"/actions/{action}", "invokeIntegrationAction", "Invoke one reviewed advertised action", "integrations")
	op.MaxBodyBytes = 64 << 10
	register(s, op, perm{resource: "integrations", verb: "act"}, func(ctx context.Context, in *integrationActInput) (*integrationExecutionOutput, error) {
		return s.executeIntegration(ctx, IntegrationExecution{Namespace: in.Namespace, Name: in.Name, Command: "act", Action: in.Action, Review: in.Body})
	})
	// Release abandons the cleanup of a terminating Integration whose policy
	// is Delete and removes the object; its provider records and private
	// ledger stay (D367). It needs no license, credential or provider.
	op = integrationOperation(http.MethodPost, base+"/release", "releaseIntegration", "Release one reviewed terminating integration without cleanup", "integrations")
	op.MaxBodyBytes = 4096
	register(s, op, perm{resource: "integrations", verb: "release"}, func(ctx context.Context, in *integrationReleaseInput) (*integrationReleaseOutput, error) {
		if s.opts.Integrations == nil {
			return nil, errIntegrationsUnavailable()
		}
		result, err := s.opts.Integrations.IntegrationRelease(ctx, in.Namespace, in.Name, in.Body)
		if err != nil {
			return nil, err
		}
		return &integrationReleaseOutput{CacheControl: "no-store", Body: result}, nil
	})
}

// errIntegrationsUnavailable refuses integration administration on a server
// without the complete SQLite integration runtime (the Kubernetes driver).
func errIntegrationsUnavailable() *Error {
	return newError(http.StatusServiceUnavailable, KeyUnavailable, "complete integration administration unavailable",
		"integrations run only on the SQLite store; see docs/deploy/integrations.md")
}

func (s *Server) executeIntegration(ctx context.Context, in IntegrationExecution) (*integrationExecutionOutput, error) {
	if s.opts.Integrations == nil {
		return nil, errIntegrationsUnavailable()
	}
	result, err := s.opts.Integrations.IntegrationExecute(ctx, in)
	if err != nil {
		return nil, err
	}
	return &integrationExecutionOutput{CacheControl: "no-store", Body: result}, nil
}

// Every integration route can reject bounded projections or unsupported host
// capabilities. Execution reads also require the exact reviewed revision.
func integrationOperation(method, path, id, summary string, tags ...string) huma.Operation {
	op := operation(method, path, id, summary, tags...)
	addErrorResponses(&op, http.StatusNotImplemented, http.StatusRequestEntityTooLarge, http.StatusConflict)
	return op
}
