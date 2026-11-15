package api

// CreateLocalUserRequest creates an ordinary API-managed local account. The
// server requires a password change on first login and assigns no groups.
type CreateLocalUserRequest struct {
	Name        string `json:"name" minLength:"1" maxLength:"253" doc:"Canonical local username, already normalized as a DNS subdomain."`
	DisplayName string `json:"displayName,omitempty" maxLength:"256" doc:"Display name; bounded to 256 UTF-8 bytes without control characters."`
	Password    string `json:"password" minLength:"8" maxLength:"1024" writeOnly:"true" doc:"Initial password, at least 8 characters and at most 1024 bytes; never returned or logged."`
}

// ResetLocalUserPasswordRequest targets an exact observed account. Reset keeps
// its factor and disabled state, requires a password change, revokes sessions,
// and deletes its local personal tokens in the same transaction.
type ResetLocalUserPasswordRequest struct {
	UID             string `json:"uid" minLength:"1" maxLength:"512" doc:"Exact immutable UID from the authorized user inventory."`
	ResourceVersion string `json:"resourceVersion" minLength:"1" maxLength:"64" doc:"Exact observed opaque resourceVersion; a conflict requires a fresh read and deliberate request."`
	NewPassword     string `json:"newPassword" minLength:"8" maxLength:"1024" writeOnly:"true" doc:"Replacement password, at least 8 characters and at most 1024 bytes; never returned or logged. This does not recover a lost authenticator."`
}
