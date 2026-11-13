package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// Integration objects are written through the generic object routes
// (/objects/integrations, POST /apply, POST /integrations:batchDelete) with
// two additions (D400):
//
//   - binding or changing spec.credentials needs integrations.secrets besides
//     integrations.write. A credential reference names a file or environment
//     variable the node operator allowlisted for every Integration; holding
//     write alone lets a caller change what an instance publishes, not which
//     of the node's credentials it uses. The check is decided on the stored
//     object that the write is compare-and-swapped against (in the SQLite
//     command, on its own read).
//   - creating an Integration needs the integrations license feature (402).
//     Replacing and deleting one never do, so an operator without a license
//     can still suspend, change the deletion policy or remove an instance.
//
// The documents carry references only; the CRD has no field for a value,
// and strict decoding refuses one. An error message never repeats a string
// of the document's spec.credentials, in case a value was pasted into a
// reference field.

// integrationKind reports whether k is the Integration kind.
func integrationKind(k kindInfo) bool { return k.kind == "Integration" }

// integrationCredentialsChanged reports whether the document body binds
// credentials that differ from those of the stored Integration existing
// (nil for a create). A body whose credentials do not decode is a change.
func integrationCredentialsChanged(body map[string]any, existing store.Object) bool {
	var want map[string]v1alpha1.CredentialReference
	spec, _ := body["spec"].(map[string]any)
	if raw, present := spec["credentials"]; present && raw != nil {
		encoded, err := json.Marshal(raw)
		if err != nil {
			return true
		}
		d := json.NewDecoder(bytes.NewReader(encoded))
		d.DisallowUnknownFields()
		if d.Decode(&want) != nil {
			return true
		}
	}
	var have map[string]v1alpha1.CredentialReference
	if i, ok := existing.(*v1alpha1.Integration); ok && i != nil {
		have = i.Spec.Credentials
	} else if existing != nil {
		return true
	}
	if len(want) != len(have) {
		return true
	}
	for name, ref := range want {
		old, ok := have[name]
		if !ok || !reflect.DeepEqual(ref, old) {
			return true
		}
	}
	return false
}

// integrationSecretsAttributes is the integrations.secrets check of a write
// of key that binds or changes credentials.
func integrationSecretsAttributes(key store.Key) rbac.Attributes {
	return rbac.Attributes{Resource: "integrations", Verb: "secrets", Scope: clusterScope, Namespace: key.Namespace, Name: key.Name}
}

// integrationLicenseRequired is the 402 of a new Integration without the
// integrations license feature.
func integrationLicenseRequired() *Error {
	return newError(http.StatusPaymentRequired, KeyLicense, "creating an Integration needs a license key that covers integrations",
		"add a license key; existing Integrations can still be changed, suspended and deleted, and DNS keeps working")
}

// commandIntegrationLicense admits a new Integration inside the SQLite
// command: the license cut must allow integrations and still be current at
// commit, as for the Policy limit.
func (s *Server) commandIntegrationLicense(tx *sqlite.CommandTx) error {
	if s.opts.License == nil {
		return nil
	}
	manager, ok := s.opts.License.(*license.Manager)
	if !ok {
		return errUnavailable("current integration license capability unavailable")
	}
	cut := manager.Capture()
	if !cut.Current() {
		return errUnavailable("current integration license unavailable")
	}
	if !cut.Status().Allows(license.FeatureIntegrations) {
		return integrationLicenseRequired()
	}
	return tx.RequireCommitCheck(func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !cut.Current() {
			return errUnavailable("current integration license expired")
		}
		return nil
	})
}

// checkIntegrationWrite is the ordinary (Kubernetes) write path's admission
// of one Integration write, decided on the stored object existing that the
// write is compare-and-swapped against.
func (s *Server) checkIntegrationWrite(ctx context.Context, key store.Key, body map[string]any, existing store.Object) error {
	if existing == nil {
		if s.opts.License != nil && !s.opts.License.Status().Allows(license.FeatureIntegrations) {
			return integrationLicenseRequired()
		}
	}
	if integrationCredentialsChanged(body, existing) {
		return s.require(ctx, "integrations", "secrets", clusterScope, key.Name)
	}
	return nil
}

// integrationCredentialStrings are the strings of the document's
// spec.credentials, long enough to be worth hiding.
func integrationCredentialStrings(body map[string]any) []string {
	spec, _ := body["spec"].(map[string]any)
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for _, x := range t {
				walk(x)
			}
		case []any:
			for _, x := range t {
				walk(x)
			}
		case string:
			if len(t) >= 6 {
				out = append(out, t)
			}
		}
	}
	walk(spec["credentials"])
	return out
}

// redactIntegrationEcho removes every spec.credentials string of an
// Integration document from the message and hint of a refusal, so that a
// value pasted into a reference field is not repeated to the caller, to a
// follower that relays the answer, or to a log.
func redactIntegrationEcho(k kindInfo, body map[string]any, err error) error {
	if err == nil || !integrationKind(k) {
		return err
	}
	secrets := integrationCredentialStrings(body)
	if len(secrets) == 0 {
		return err
	}
	redact := func(s string) (string, bool) {
		changed := false
		for _, secret := range secrets {
			if strings.Contains(s, secret) {
				s, changed = strings.ReplaceAll(s, secret, "[redacted]"), true
			}
		}
		return s, changed
	}
	var e *Error
	if !errors.As(storeError(err), &e) {
		if _, changed := redact(err.Error()); changed {
			return errInvalid("spec.credentials: invalid credential reference (a reference names a file or environment variable; values are never accepted)")
		}
		return err
	}
	out := *e
	var changedMessage, changedHint bool
	out.Body.Message, changedMessage = redact(out.Body.Message)
	if hint, ok := out.Body.Hint.(string); ok {
		out.Body.Hint, changedHint = redact(hint)
	} else if out.Body.Hint != nil {
		raw, mErr := json.Marshal(out.Body.Hint)
		if mErr == nil {
			_, changedHint = redact(string(raw))
		}
		if mErr != nil || changedHint {
			out.Body.Hint, changedHint = nil, true
		}
	}
	if !changedMessage && !changedHint {
		// Keep the original error, which callers still classify.
		return err
	}
	return &out
}
