package oidc

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Login error codes. They are stable identifiers the login page can map to
// help text; Message carries the human explanation.
const (
	CodeIDPError         = "idp_error"
	CodeIDPUnavailable   = "idp_unavailable"
	CodeMissingPreAuth   = "missing_preauth"
	CodeInvalidPreAuth   = "invalid_preauth"
	CodeExpiredPreAuth   = "expired_preauth"
	CodeStateMismatch    = "state_mismatch"
	CodeMissingCode      = "missing_code"
	CodeExchangeFailed   = "exchange_failed"
	CodeMissingIDToken   = "missing_id_token"
	CodeInvalidIDToken   = "invalid_id_token"
	CodeNonceMismatch    = "nonce_mismatch"
	CodeAudience         = "audience_mismatch"
	CodeClaimRule        = "claim_rule_failed"
	CodeClaims           = "claims_unusable"
	CodeDistributedClaim = "distributed_groups"
	CodeNotProvisioned   = "not_provisioned"
	CodeNoBindings       = "no_bindings"
	CodeSessionTooLarge  = "session_too_large"
	CodeInternal         = "internal"
)

// LoginError is a refused login. Message is safe to show to the user and
// never contains tokens or raw claim values; Err is the internal cause and
// is only logged.
type LoginError struct {
	Status  int
	Code    string
	Message string
	Err     error
}

func (e *LoginError) Error() string {
	if e.Err != nil {
		return e.Code + ": " + e.Message + ": " + e.Err.Error()
	}
	return e.Code + ": " + e.Message
}

func (e *LoginError) Unwrap() error { return e.Err }

func loginErr(status int, code, msg string, cause error) *LoginError {
	return &LoginError{Status: status, Code: code, Message: msg, Err: cause}
}

// ErrorHandler renders a refused login. The default writes JSON when the
// client asks for it and plain text otherwise; the SPA build replaces it
// with a redirect to /login?error=<code>.
type ErrorHandler func(w http.ResponseWriter, r *http.Request, e *LoginError)

// DefaultErrorHandler is the ErrorHandler used when Options.ErrorHandler is
// nil.
func DefaultErrorHandler(w http.ResponseWriter, r *http.Request, e *LoginError) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		h.Set("Content-Type", "application/json")
		w.WriteHeader(e.Status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": e.Code, "reason": e.Message})
		return
	}
	h.Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(e.Status)
	// text/plain with nosniff: the username in Message cannot become markup.
	_, _ = fmt.Fprintf(w, "kudurru login failed (%s): %s\n", e.Code, e.Message) //nolint:gosec // G705: see above
}

// sanitizeIDPError keeps an IdP-supplied OAuth error code printable and
// short before it is echoed (RFC 6749 error codes are %x20-21 / %x23-5B /
// %x5D-7E).
func sanitizeIDPError(s string) string {
	if len(s) > 64 {
		s = s[:64]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' || r == '<' || r == '>' {
			return '_'
		}
		return r
	}, s)
}
