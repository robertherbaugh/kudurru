package api

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// KeyIAMManagementUnavailable is the 503 of a ServiceAccount or
// IdentityProvider command on a runtime that does not offer it (the Kubernetes
// driver, D396). It is durable: waiting does not change the answer.
const KeyIAMManagementUnavailable = "iam_management_unavailable"

// IAMObjectCommandRuntime is mounted only by the complete atomic SQLite
// runtime. Every other runtime refuses the ServiceAccount and IdentityProvider
// commands instead of falling back to ordinary object writes (D396).
type IAMObjectCommandRuntime interface{ IAMObjectCommandsAvailable() bool }

// ObjectReview names the exact independently reviewed incarnation and revision
// of an IAM object or account. A conflict requires a new operator decision,
// never an automatic read or retry.
type ObjectReview struct {
	UID             string `json:"uid" minLength:"1" maxLength:"128" doc:"Exact immutable UID from the reviewed read."`
	ResourceVersion string `json:"resourceVersion" minLength:"1" maxLength:"128" doc:"Exact resourceVersion from the reviewed read; a conflict requires a fresh read and a deliberate new request."`
}

// UpdateLocalUserRequest changes the operator-controlled fields of an exact
// API-managed local account. An absent field is kept; at least one is
// required. The password, authenticator and password-change requirement are
// never changed here.
type UpdateLocalUserRequest struct {
	UID             string    `json:"uid" minLength:"1" maxLength:"512" doc:"Exact immutable UID from the authorized user inventory."`
	ResourceVersion string    `json:"resourceVersion" minLength:"1" maxLength:"64" doc:"Exact observed opaque resourceVersion."`
	DisplayName     *string   `json:"displayName,omitempty" maxLength:"256" doc:"New display name; an empty string clears it."`
	Groups          *[]string `json:"groups,omitempty" maxItems:"64" doc:"The complete new list of local groups (replaces the old list; [] removes every group). A group name has at most 253 bytes and no colon, control character or surrounding space."`
	Disabled        *bool     `json:"disabled,omitempty" doc:"true disables the account: its sessions and API tokens end in the same commit. false enables it again without restoring them."`
}

// ServiceAccountMetadata contains operator-controlled metadata only.
type ServiceAccountMetadata struct {
	Name        string            `json:"name" minLength:"1" maxLength:"253"`
	Namespace   string            `json:"namespace" minLength:"1" maxLength:"63"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// ServiceAccountFields are the operator-controlled fields of a ServiceAccount.
// Token provisioning into a Kubernetes Secret (spec.token) is not offered by
// this API: tokens are issued once through the token route instead.
type ServiceAccountFields struct {
	Description string `json:"description,omitempty" maxLength:"2048"`
	Disabled    bool   `json:"disabled,omitempty" doc:"Refuses every token of the account. Disabling deletes its tokens in the same commit; enabling restores none."`
}

// ServiceAccountDesired is a public ServiceAccount manifest without server
// metadata.
type ServiceAccountDesired struct {
	APIVersion string                 `json:"apiVersion"`
	Kind       string                 `json:"kind"`
	Metadata   ServiceAccountMetadata `json:"metadata"`
	Spec       ServiceAccountFields   `json:"spec"`
}

// ServiceAccountDocument is the stored ServiceAccount with its exact
// server-owned metadata for review. It never carries a token.
type ServiceAccountDocument struct {
	APIVersion string                        `json:"apiVersion"`
	Kind       string                        `json:"kind"`
	Metadata   metav1.ObjectMeta             `json:"metadata"`
	Spec       v1alpha1.ServiceAccountSpec   `json:"spec"`
	Status     v1alpha1.ServiceAccountStatus `json:"status,omitempty"`
}

// ReplaceServiceAccountCommand carries desired state and the explicit review.
type ReplaceServiceAccountCommand struct {
	ObjectReview
	ServiceAccount ServiceAccountDesired `json:"serviceAccount"`
}

// CreateServiceAccountTokenRequest issues one API token for an exact service
// account. The secret is returned once, in the answer of this request.
type CreateServiceAccountTokenRequest struct {
	UID         string   `json:"uid" minLength:"1" maxLength:"128" doc:"Exact immutable UID of the reviewed service account."`
	Description string   `json:"description,omitempty" maxLength:"4096" doc:"Free text shown in the token list."`
	ExpiresIn   string   `json:"expiresIn,omitempty" doc:"Go duration (720h); empty means auth.tokens.maxLifetime, or never."`
	Scopes      []string `json:"scopes,omitempty" maxItems:"64" doc:"Narrow the token to these grants in wire form; empty keeps the account's bindings."`
}

// IdentityProviderMetadata contains operator-controlled metadata only.
// IdentityProvider is cluster-scoped; its name is the provider id.
type IdentityProviderMetadata struct {
	Name        string            `json:"name" minLength:"1" maxLength:"63" doc:"The provider id, a DNS label."`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// IdentityProviderDesired is a public IdentityProvider manifest without server
// metadata. spec.clientSecretRef is a reference (Secret key, allowlisted file
// or environment variable); a secret value is never accepted or returned.
type IdentityProviderDesired struct {
	APIVersion string                        `json:"apiVersion"`
	Kind       string                        `json:"kind"`
	Metadata   IdentityProviderMetadata      `json:"metadata"`
	Spec       v1alpha1.IdentityProviderSpec `json:"spec"`
}

// IdentityProviderDocument is the stored IdentityProvider with its exact
// server-owned metadata for review.
type IdentityProviderDocument struct {
	APIVersion string                          `json:"apiVersion"`
	Kind       string                          `json:"kind"`
	Metadata   metav1.ObjectMeta               `json:"metadata"`
	Spec       v1alpha1.IdentityProviderSpec   `json:"spec"`
	Status     v1alpha1.IdentityProviderStatus `json:"status,omitempty"`
}

// ReplaceIdentityProviderCommand carries desired state and the explicit review.
type ReplaceIdentityProviderCommand struct {
	ObjectReview
	IdentityProvider IdentityProviderDesired `json:"identityProvider"`
}

// iamObjectsAvailable refuses the ServiceAccount and IdentityProvider commands
// on a runtime without the complete SQLite IAM command path.
func (s *Server) iamObjectsAvailable() error {
	runtime, ok := s.opts.CanonicalSettings.(IAMObjectCommandRuntime)
	if !ok || !runtime.IAMObjectCommandsAvailable() || s.opts.AuthHandler == nil {
		return newError(http.StatusServiceUnavailable, KeyIAMManagementUnavailable, "ServiceAccount and IdentityProvider commands require the complete SQLite runtime",
			"on the Kubernetes driver, create, change and delete kudurru.io ServiceAccount and IdentityProvider objects with kubectl or your GitOps tool")
	}
	return nil
}

// registerIAMWriters registers the reviewed IAM commands of D396: local user
// changes and deletion, and ServiceAccount and IdentityProvider create,
// replace and delete, plus service account token issuance.
func (s *Server) registerIAMWriters() {
	bounded := func(method, path, id, summary string) huma.Operation {
		op := operation(method, path, id, summary, "iam")
		op.MaxBodyBytes = 16 << 10
		return op
	}
	update := bounded(http.MethodPatch, "/iam/users/{name}", "updateLocalUser", "Change an exact API-managed local account's display name, groups or disabled state")
	update.Description = "Complete SQLite runtime only. Requires exact UID and resourceVersion. Absent fields are kept. Changing groups or disabling the account ends its sessions and deletes its API tokens in the same commit; enabling restores none. The password, authenticator and password-change requirement are unchanged. Built-in and non-API-managed accounts are excluded. No automatic retry."
	s.forward(update, perm{resource: "users", verb: "write"}, UpdateLocalUserRequest{}, IAMUser{})
	remove := bounded(http.MethodDelete, "/iam/users/{name}", "deleteLocalUser", "Delete an exact API-managed local account")
	remove.Description = "Complete SQLite runtime only. Requires exact UID and resourceVersion. Ends the account's sessions and deletes its API tokens in the same commit; a later account with the same name never inherits them. Built-in and non-API-managed accounts are excluded."
	s.forward(withStatus(remove, http.StatusNoContent), perm{resource: "users", verb: "delete"}, ObjectReview{}, nil)

	saItem := "/iam/serviceaccounts/{namespace}/{name}"
	s.forward(operation(http.MethodGet, saItem, "getServiceAccount", "Read a ServiceAccount and its current UID/resourceVersion", "iam"), perm{resource: "serviceaccounts", verb: "read"}, nil, ServiceAccountDocument{})
	saCreate := withStatus(operation(http.MethodPost, "/iam/serviceaccounts", "createServiceAccount", "Create a ServiceAccount only if absent", "iam"), http.StatusCreated)
	saCreate.Description = "Complete SQLite runtime only (503 iam_management_unavailable elsewhere). Creates once; never adopts an existing account. Only public operator metadata and spec.description/spec.disabled are accepted. No token is issued: use POST /iam/serviceaccounts/{namespace}/{name}/tokens."
	s.forwardReviewedCommand(saCreate, perm{resource: "serviceaccounts", verb: "write"}, ServiceAccountDesired{}, ServiceAccountDocument{}, s.iamObjectsAvailable)
	saReplace := operation(http.MethodPut, saItem, "replaceServiceAccount", "Replace an explicitly reviewed API-owned ServiceAccount", "iam")
	saReplace.Description = "Complete SQLite runtime only. Requires exact reviewed UID and resourceVersion. Only API-owned accounts may be replaced. Disabling raises the account's revocation floor and deletes its tokens atomically; enabling restores none. No implicit refresh or retry."
	s.forwardReviewedCommand(saReplace, perm{resource: "serviceaccounts", verb: "write"}, ReplaceServiceAccountCommand{}, ServiceAccountDocument{}, s.iamObjectsAvailable)
	if op := documented(s.api.OpenAPI(), http.MethodPut, saItem); op != nil {
		delete(op.Responses, "201")
	}
	saDelete := withStatus(operation(http.MethodDelete, saItem, "deleteServiceAccount", "Delete an explicitly reviewed API-owned ServiceAccount", "iam"), http.StatusNoContent)
	saDelete.Description = "Complete SQLite runtime only. Requires exact reviewed UID and resourceVersion. Only API-owned accounts without bootstrap authority may be deleted. The account's revocation floor and the deletion of its tokens commit with the object. No implicit refresh or retry."
	s.forwardReviewedCommand(saDelete, perm{resource: "serviceaccounts", verb: "delete"}, ObjectReview{}, nil, s.iamObjectsAvailable)
	saToken := withStatus(operation(http.MethodPost, saItem+"/tokens", "createServiceAccountToken", "Issue an API token for an exact ServiceAccount (the secret is returned once)", "iam"), http.StatusCreated)
	saToken.Description = "Complete SQLite runtime only. Needs serviceaccounts.write and tokens.write at cluster scope and the exact account UID. The kdr_ secret appears only in this answer; it is never stored, listed or logged. Refused for a disabled account. List and revoke the token with GET /auth/tokens?all=true and DELETE /auth/tokens/{id}."
	s.forwardReviewedCommand(saToken, perm{resource: "serviceaccounts", verb: "write"}, CreateServiceAccountTokenRequest{}, CreatedToken{}, s.iamObjectsAvailable)

	idpItem := "/iam/identityproviders/{name}"
	s.forward(operation(http.MethodGet, idpItem, "getIdentityProvider", "Read an IdentityProvider object and its current UID/resourceVersion", "iam"), perm{resource: "identityproviders", verb: "read"}, nil, IdentityProviderDocument{})
	idpCreate := withStatus(operation(http.MethodPost, "/iam/identityproviders", "createIdentityProvider", "Create an IdentityProvider object only if absent", "iam"), http.StatusCreated)
	idpCreate.Description = "Complete SQLite runtime only (503 iam_management_unavailable elsewhere). Creates once; never adopts an existing object or a provider id of the configuration file. spec.clientSecretRef names a credential reference; a secret value is never accepted. The provider is offered for login after its discovery and license admission."
	s.forwardReviewedCommand(idpCreate, perm{resource: "identityproviders", verb: "write"}, IdentityProviderDesired{}, IdentityProviderDocument{}, s.iamObjectsAvailable)
	idpReplace := operation(http.MethodPut, idpItem, "replaceIdentityProvider", "Replace an explicitly reviewed API-owned IdentityProvider", "iam")
	idpReplace.Description = "Complete SQLite runtime only. Requires exact reviewed UID and resourceVersion. Only API-owned objects may be replaced. A changed spec raises the provider's revocation floor in the same commit: every session and token that came from it ends. No implicit refresh or retry."
	s.forwardReviewedCommand(idpReplace, perm{resource: "identityproviders", verb: "write"}, ReplaceIdentityProviderCommand{}, IdentityProviderDocument{}, s.iamObjectsAvailable)
	if op := documented(s.api.OpenAPI(), http.MethodPut, idpItem); op != nil {
		delete(op.Responses, "201")
	}
	idpDelete := withStatus(operation(http.MethodDelete, idpItem, "deleteIdentityProvider", "Delete an explicitly reviewed API-owned IdentityProvider", "iam"), http.StatusNoContent)
	idpDelete.Description = "Complete SQLite runtime only. Requires exact reviewed UID and resourceVersion. Only API-owned objects may be deleted. The provider's revocation floor commits with the deletion: every session and token that came from it ends, also across restarts and on replicas. No implicit refresh or retry."
	s.forwardReviewedCommand(idpDelete, perm{resource: "identityproviders", verb: "delete"}, ObjectReview{}, nil, s.iamObjectsAvailable)
}
