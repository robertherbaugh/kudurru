package api

import (
	"net/http"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/clioidc"
)

// CLILoginPrepareBody selects a public flow without endpoint overrides.
type CLILoginPrepareBody clioidc.PrepareRequest

// CLILoginPreparedBody returns public source correlation, never credentials.
type CLILoginPreparedBody clioidc.Preparation

// CLILoginVerifyBody carries transient proof with the original raw bearer.
type CLILoginVerifyBody clioidc.VerifyRequest

// CLILoginProviderBody describes advertised public-client support.
type CLILoginProviderBody clioidc.Provider

// CLILoginVerifiedBody returns the original bearer identity and expiry.
type CLILoginVerifiedBody struct {
	Me        SessionInfo `json:"me"`
	ExpiresAt time.Time   `json:"expiresAt"`
}

func (s *Server) registerCLILogin() {
	public := perm{public: true}
	s.forward(operation(http.MethodGet, "/auth/cli/providers", "listCLILoginProviders", "Advertised public-client login options", "auth"), public, nil, AuthList[CLILoginProviderBody]{})
	s.forward(withRateLimit(operation(http.MethodPost, "/auth/cli/{idp}/prepare", "prepareCLILogin", "Correlate a public CLI login source without issuing authority", "auth")), public, CLILoginPrepareBody{}, CLILoginPreparedBody{})
	s.forward(operation(http.MethodPost, "/auth/cli/{idp}/verify", "verifyCLILogin", "Verify original CLI ID-token proof without issuing authority", "auth"), perm{authenticated: true}, CLILoginVerifyBody{}, CLILoginVerifiedBody{})
}
