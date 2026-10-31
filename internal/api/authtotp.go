package api

import "time"

// TOTPStatus reports only whether the current local user has an active factor.
type TOTPStatus struct {
	Enabled bool `json:"enabled"`
}

// TOTPProofRequest verifies the current password and any already enrolled factor.
type TOTPProofRequest struct {
	CurrentPassword string `json:"currentPassword" maxLength:"1024"`
	TOTPCode        string `json:"totpCode,omitempty" pattern:"^[0-9]{6}$" doc:"The current factor's code, when already enabled."`
}

// TOTPConfirmRequest proves both the current account and the new candidate seed.
type TOTPConfirmRequest struct {
	CurrentPassword string `json:"currentPassword" maxLength:"1024"`
	TOTPCode        string `json:"totpCode,omitempty" pattern:"^[0-9]{6}$" doc:"The previously enrolled factor's code, when rotating."`
	Challenge       string `json:"challenge" maxLength:"8192"`
	Code            string `json:"code" pattern:"^[0-9]{6}$" doc:"The new candidate factor's code."`
}

// TOTPSetupResponse intentionally discloses provisioning material once, after
// the setup command commits. Clients must not persist or log these fields.
type TOTPSetupResponse struct {
	Challenge  string    `json:"challenge"`
	Secret     string    `json:"secret" doc:"Base32 provisioning secret, shown only during setup."`
	OTPAuthURI string    `json:"otpauthURI" doc:"Local authenticator provisioning URI; never send it to an external QR service."`
	ExpiresAt  time.Time `json:"expiresAt"`
}

// TOTPRecoverRequest binds administrator recovery to the exact account incarnation.
type TOTPRecoverRequest struct {
	UID string `json:"uid" minLength:"1" doc:"The current immutable UID from the authorized user inventory."`
}
