package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strconv"
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
//     integrations.write, and so does changing where bound credentials are
//     presented (spec.type, spec.unifi.endpoint and tlsFingerprint,
//     spec.webhook.endpoint). A credential reference names a file or
//     environment variable the node operator allowlisted for every
//     Integration; holding write alone lets a caller change what an
//     instance publishes, not which of the node's credentials it uses or
//     which server receives them. The check is decided on the stored
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

// integrationDestinationFields are the fields that decide where the
// host presents an Integration's bound credentials: the provider type and
// the endpoint and TLS pin it connects to. Changing one while credentials
// are bound sends the node's credential to a server the caller chose, so it
// is a binding change like a changed reference.
var integrationDestinationFields = [][]string{
	{"spec", "type"},
	{"spec", "unifi", "endpoint"},
	{"spec", "unifi", "tlsFingerprint"},
	{"spec", "webhook", "endpoint"},
}

// integrationDestinationChanged reports whether the document body and the
// stored Integration existing differ in a destination field. A field of
// the body that is present but not a string is a change.
func integrationDestinationChanged(body map[string]any, existing store.Object) bool {
	i, ok := existing.(*v1alpha1.Integration)
	if !ok || i == nil {
		return existing != nil
	}
	stored := map[string]any{"spec": map[string]any{"type": i.Spec.Type}}
	if u := i.Spec.UniFi; u != nil {
		stored["spec"].(map[string]any)["unifi"] = map[string]any{"endpoint": u.Endpoint, "tlsFingerprint": u.TLSFingerprint}
	}
	if w := i.Spec.Webhook; w != nil {
		stored["spec"].(map[string]any)["webhook"] = map[string]any{"endpoint": w.Endpoint}
	}
	for _, path := range integrationDestinationFields {
		want, err := nestedField(body, path)
		if err != nil {
			return true
		}
		have, _ := nestedField(stored, path)
		if want != have {
			return true
		}
	}
	return false
}

// nestedField is the string at path of m ("" when absent); a value on the
// path that is neither a map, a string nor null is an error.
func nestedField(m map[string]any, path []string) (string, error) {
	var cur any = m
	for _, p := range path {
		switch t := cur.(type) {
		case nil:
			return "", nil
		case map[string]any:
			cur = t[p]
		default:
			return "", errors.New("not an object")
		}
	}
	switch t := cur.(type) {
	case nil:
		return "", nil
	case string:
		return t, nil
	default:
		return "", errors.New("not a string")
	}
}

// integrationNeedsSecrets reports whether a write of body over the stored
// Integration existing changes a credential binding: the credentials
// themselves, or, while credentials are bound before or after, where the
// host presents them (integrationDestinationFields).
func integrationNeedsSecrets(body map[string]any, existing store.Object) bool {
	if integrationCredentialsChanged(body, existing) {
		return true
	}
	i, _ := existing.(*v1alpha1.Integration)
	if i == nil || len(i.Spec.Credentials) == 0 {
		// Unchanged credentials over nothing bound bind nothing.
		return false
	}
	return integrationDestinationChanged(body, existing)
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
	if integrationNeedsSecrets(body, existing) {
		return s.require(ctx, "integrations", "secrets", clusterScope, key.Name)
	}
	return nil
}

// integrationCredentialForms are the forms in which a string of the
// document's spec.credentials can appear in an error message: as sent, as
// Go's %q and strconv.Quote print it (the validator's "Invalid value"), and
// as JSON encodes it, each with and without the surrounding quotes. A
// quoted form is distinctive at any length; an unquoted one only from
// integrationEchoMinimum bytes on, so that a short value does not garble
// unrelated words of a message. Matching is a fallback: a validation
// failure that names spec.credentials is replaced as a whole
// (integrationCredentialsInvalid).
func integrationCredentialForms(body map[string]any) []string {
	spec, _ := body["spec"].(map[string]any)
	seen := map[string]bool{}
	var out []string
	add := func(form string, quoted bool) {
		if form == "" || seen[form] || !quoted && len(form) < integrationEchoMinimum {
			return
		}
		seen[form] = true
		out = append(out, form)
	}
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
			if t == "" {
				return
			}
			goQuoted := strconv.Quote(t)
			add(goQuoted, true)
			add(goQuoted[1:len(goQuoted)-1], false)
			if encoded, err := json.Marshal(t); err == nil {
				add(string(encoded), true)
				add(string(encoded[1:len(encoded)-1]), false)
			}
			add(t, false)
		}
	}
	walk(spec["credentials"])
	// Longest first, so that a form is replaced before a shorter one it
	// contains.
	slices.SortStableFunc(out, func(a, b string) int { return len(b) - len(a) })
	return out
}

// integrationEchoMinimum is the shortest unquoted form that is matched.
const integrationEchoMinimum = 4

// integrationCredentialsInvalid is the 400 that replaces a validation
// failure naming spec.credentials. It carries the field paths only: the
// validator's message prints the refused value, which may be a pasted
// credential.
func integrationCredentialsInvalid(fields []string, secrets []string) *Error {
	var kept []string
	for _, f := range fields {
		if !containsAny(f, secrets) {
			kept = append(kept, f)
		}
	}
	message := "spec.credentials: invalid credential reference (a reference names a file, an environment variable or a Secret key; values are never accepted, and the refused value is not repeated)"
	hint := "the object does not match the Integration schema"
	if len(kept) > 0 {
		message += ": " + strings.Join(kept, ", ")
		hint = "fix the listed fields and retry: " + strings.Join(kept, ", ")
	}
	e := newError(http.StatusBadRequest, KeyInvalid, message, hint)
	e.Body.Fields = kept
	return e
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// redactIntegrationEcho keeps a string of an Integration document's
// spec.credentials out of the message and hint of a refusal, so that a
// value pasted into a reference field is not repeated to the caller, to a
// follower that relays the answer, or to a log. A validation failure that
// names spec.credentials (in its message, hint or fields) is replaced by
// integrationCredentialsInvalid; any other refusal has every form of those
// strings replaced by "[redacted]".
func redactIntegrationEcho(k kindInfo, body map[string]any, err error) error {
	if err == nil || !integrationKind(k) {
		return err
	}
	secrets := integrationCredentialForms(body)
	var e *Error
	if !errors.As(storeError(err), &e) {
		if text := err.Error(); strings.Contains(text, "spec.credentials") || containsAny(text, secrets) {
			return integrationCredentialsInvalid(nil, secrets)
		}
		return err
	}
	if e.Body.Key == KeyInvalid {
		names := strings.Contains(e.Body.Message, "spec.credentials")
		if hint, ok := e.Body.Hint.(string); ok && strings.Contains(hint, "spec.credentials") {
			names = true
		}
		for _, f := range e.Body.Fields {
			if strings.HasPrefix(f, "spec.credentials") {
				names = true
			}
		}
		if names {
			return integrationCredentialsInvalid(e.Body.Fields, secrets)
		}
	}
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
