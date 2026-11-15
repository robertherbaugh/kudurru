package api

import (
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

// ExternalUser describes an exact persisted external identity. Name is the
// canonical provider-incarnation/issuer/subject digest, never a username.
type ExternalUser struct {
	Name        string     `json:"name"`
	UID         string     `json:"uid"`
	Revision    string     `json:"revision"`
	Provider    string     `json:"provider"`
	ProviderUID string     `json:"providerUID"`
	Origin      string     `json:"origin"`
	Issuer      string     `json:"issuer"`
	Subject     string     `json:"subject"`
	Username    string     `json:"username"`
	DisplayName string     `json:"displayName"`
	Email       string     `json:"email"`
	Groups      []string   `json:"groups" nullable:"false"`
	Disabled    bool       `json:"disabled"`
	CreatedAt   time.Time  `json:"createdAt"`
	ValidAfter  *time.Time `json:"validAfter,omitempty"`
}

// ExternalUserCommand requires the exact operator-reviewed account incarnation.
type ExternalUserCommand struct {
	UID      string `json:"uid" minLength:"32" maxLength:"32"`
	Revision string `json:"revision" minLength:"64" maxLength:"64"`
}

func (s *Server) registerExternalUsers() {
	list := operation(http.MethodGet, "/iam/external-users", "listExternalUsers", "List persisted external accounts", "iam")
	list.Parameters = []*huma.Param{
		{Name: "limit", In: "query", Schema: &huma.Schema{Type: "integer", Minimum: new(float64(1)), Maximum: new(float64(100))}},
		{Name: "continue", In: "query", Schema: &huma.Schema{Type: "string"}},
	}
	s.forward(list, perm{resource: "users", verb: "read"}, nil, AuthList[ExternalUser]{})
	s.forward(operation(http.MethodGet, "/iam/external-users/{name}", "getExternalUser", "Read an exact external account", "iam"), perm{resource: "users", verb: "read"}, nil, ExternalUser{})
	for _, action := range []struct{ path, id, summary string }{
		{"disable", "disableExternalUser", "Disable an external account and revoke its credentials"},
		{"enable", "enableExternalUser", "Enable an external account without restoring credentials"},
		{"logout", "logoutExternalUser", "Revoke all credentials of an external account"},
	} {
		op := withStatus(operation(http.MethodPost, "/iam/external-users/{name}/"+action.path, action.id, action.summary, "iam"), http.StatusNoContent)
		op.MaxBodyBytes = 4096
		op.Description = "Requires the complete SQLite runtime, exact UID/revision and current users.write authority. Revocation is local to Kudurru; no remote identity-provider session is ended. No retry or credential issuance."
		s.forward(op, perm{resource: "users", verb: "write"}, ExternalUserCommand{}, nil)
	}
}
