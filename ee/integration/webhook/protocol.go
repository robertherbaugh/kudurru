package webhook

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/ee/integration/externaldns"
)

const maxItems = 4096

type declaration struct {
	Filters      []string             `json:"filters"`
	Capabilities []string             `json:"capabilities"`
	Manifest     integration.Manifest `json:"manifest"`
}

// advertisement is detached, original protocol evidence. It grants neither
// remote ownership nor a complete inventory, source admission or mutation retry.
type advertisement struct {
	media       string
	declaration declaration
	filter      externaldns.DomainFilter
	actions     []integration.ActionSpec
	digest      string
	// The compiled filter expressions of an external-dns peer, if set.
	regexInclude, regexExclude *regexp.Regexp
}

func (a *advertisement) has(capability string) bool {
	return a != nil && slices.Contains(a.declaration.Capabilities, capability)
}

// negotiate uses one original context across the declaration and action-schema
// reads. Callers must compare the resulting exact identity before using an
// already provisioned instance; negotiation is not permission to adopt changes.
func (t *transport) negotiate(ctx context.Context) (*advertisement, error) {
	r, err := t.request(ctx, http.MethodGet, "/", "", nil, nil)
	if err != nil {
		return nil, err
	}
	if r.status != http.StatusOK || validJSON(r.body) != nil || t.containsCredential(r.body) {
		return nil, errProtocol
	}
	a := &advertisement{media: r.media, actions: []integration.ActionSpec{}}
	switch r.media {
	case externalMedia:
		if !objectJSON(r.body) || decode(r.body, &a.filter) != nil || !validFilter(a.filter) {
			return nil, errProtocol
		}
		a.declaration.Capabilities = []string{"records"}
		if a.filter.RegexInclude != "" {
			a.regexInclude = regexp.MustCompile(a.filter.RegexInclude)
		}
		if a.filter.RegexExclude != "" {
			a.regexExclude = regexp.MustCompile(a.filter.RegexExclude)
		}
	case integrationMedia:
		if !objectJSON(r.body) || decode(r.body, &a.declaration) != nil || !validDeclaration(a.declaration) {
			return nil, errProtocol
		}
	default:
		return nil, errProtocol
	}
	if a.has("actions") {
		r, err = t.request(ctx, http.MethodGet, "/actions", a.media, nil, nil)
		if err != nil {
			return nil, err
		}
		if r.status != http.StatusOK || r.media != a.media || validJSON(r.body) != nil || t.containsCredential(r.body) {
			return nil, errProtocol
		}
		a.actions, err = decodeActions(r.body)
		if err != nil {
			return nil, err
		}
	}
	// JSON encoding fixes map ordering while retaining exact schemas and order.
	// Cosmetic provider declaration changes may conservatively replace identity.
	b, err := json.Marshal(struct {
		Media       string
		Declaration declaration
		Filter      externaldns.DomainFilter
		Actions     []integration.ActionSpec
	}{a.media, a.declaration, a.filter, a.actions})
	if err != nil {
		return nil, errProtocol
	}
	hash := sha256.Sum256(b)
	a.digest = hex.EncodeToString(hash[:])
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return a, nil
}

func validDeclaration(d declaration) bool {
	m := d.Manifest
	if len(d.Capabilities) == 0 || len(d.Capabilities) > 4 || len(d.Filters) > 256 || !textBound(m.Type, 128) || !textBound(m.DisplayName, 256) || !textBound(m.Version, 128) || len(m.Docs) > 2048 || len(m.SecretKeys) > 16 || len(m.ConfigSchema) > 64<<10 || !objectJSON(m.ConfigSchema) || validJSON(m.ConfigSchema) != nil {
		return false
	}
	switch m.IOClass {
	case "cloud_polling", "local_polling", "local_push":
	default:
		return false
	}
	caps := map[string]bool{}
	for _, cap := range d.Capabilities {
		if caps[cap] {
			return false
		}
		switch cap {
		case "records", "clients", "hooks", "actions":
		default:
			return false
		}
		caps[cap] = true
	}
	if len(m.Capabilities) != len(caps) {
		return false
	}
	seen := map[string]bool{}
	for _, cap := range m.Capabilities {
		var wire string
		switch cap {
		case integration.CapRecordSink:
			wire = "records"
		case integration.CapClientSource:
			wire = "clients"
		case integration.CapHook:
			wire = "hooks"
		case integration.CapActions:
			wire = "actions"
		default:
			return false
		}
		if !caps[wire] || seen[wire] {
			return false
		}
		seen[wire] = true
	}
	for _, filter := range d.Filters {
		if !textBound(filter, 253) {
			return false
		}
	}
	seen = map[string]bool{}
	for _, secret := range m.SecretKeys {
		if !textBound(secret.Path, 128) || secret.Verify != "" && !textBound(secret.Verify, 256) || seen[secret.Path] {
			return false
		}
		seen[secret.Path] = true
	}
	return true
}

func validFilter(f externaldns.DomainFilter) bool {
	if len(f.Include) > 256 || len(f.Exclude) > 256 {
		return false
	}
	for _, names := range [][]string{f.Include, f.Exclude} {
		for _, name := range names {
			if !textBound(name, 253) {
				return false
			}
		}
	}
	for _, pattern := range []string{f.RegexInclude, f.RegexExclude} {
		if len(pattern) > 4096 {
			return false
		}
		if _, err := regexp.Compile(pattern); err != nil {
			return false
		}
	}
	return true
}

func decodeActions(raw []byte) ([]integration.ActionSpec, error) {
	var values []*integration.ActionSpec
	if !arrayJSON(raw) || decode(raw, &values) != nil || len(values) > 128 {
		return nil, errProtocol
	}
	actions := make([]integration.ActionSpec, 0, len(values))
	seen, total := map[string]bool{}, 0
	for _, action := range values {
		if action == nil || !actionName(action.Name) || seen[action.Name] || action.Verb != "integrations.act" || len(action.Params) > 64<<10 || !objectJSON(action.Params) || validJSON(action.Params) != nil {
			return nil, errProtocol
		}
		total += len(action.Params)
		if total > 256<<10 {
			return nil, errProtocol
		}
		seen[action.Name] = true
		actions = append(actions, *action)
	}
	return actions, nil
}

func textBound(s string, limit int) bool {
	return s != "" && len(s) <= limit && !strings.ContainsFunc(s, func(c rune) bool { return c < 0x20 || c == 0x7f })
}

func objectJSON(b []byte) bool { return bytes.HasPrefix(bytes.TrimSpace(b), []byte("{")) }
func arrayJSON(b []byte) bool  { return bytes.HasPrefix(bytes.TrimSpace(b), []byte("[")) }

func decode(raw []byte, out any) error { return decodeItems(raw, out, maxItems) }

// decodeItems is decode with a larger bound on the elements of one array or
// object, for the record list (D380).
func decodeItems(raw []byte, out any, limit int) error {
	if err := validJSONItems(raw, limit); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return errProtocol
	}
	return nil
}

// validJSON bounds work before typed decoding and rejects duplicate keys,
// including case aliases accepted by encoding/json's struct-field matching.
func validJSON(raw []byte) error { return validJSONItems(raw, maxItems) }

// validJSONItems is validJSON with limit elements per array or object.
func validJSONItems(raw []byte, limit int) error {
	if len(raw) == 0 || len(raw) > maxBody || !utf8.Valid(raw) {
		return errProtocol
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	budget := 1 << 20
	var value func(int) bool
	value = func(depth int) bool {
		budget--
		if depth > 32 || budget < 0 {
			return false
		}
		token, err := d.Token()
		if err != nil {
			return false
		}
		delim, compound := token.(json.Delim)
		if !compound {
			return true
		}
		if delim != '{' && delim != '[' {
			return false
		}
		keys, count := map[string]bool{}, 0
		for d.More() {
			count++
			if count > limit {
				return false
			}
			if delim == '{' {
				token, err := d.Token()
				name, ok := token.(string)
				if err != nil || !ok {
					return false
				}
				name = foldJSONKey(name)
				if keys[name] {
					return false
				}
				keys[name] = true
			}
			if !value(depth + 1) {
				return false
			}
		}
		end, err := d.Token()
		return err == nil && (delim == '{' && end == json.Delim('}') || delim == '[' && end == json.Delim(']'))
	}
	if !value(0) {
		return errProtocol
	}
	if _, err := d.Token(); err != io.EOF {
		return errProtocol
	}
	return nil
}

// Match encoding/json's Unicode case folding, including non-ASCII aliases of
// ASCII field names such as the long s. Lowercasing alone misses those aliases.
func foldJSONKey(s string) string {
	return strings.Map(func(r rune) rune {
		least := r
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			least = min(least, next)
		}
		return least
	}, s)
}

func (t *transport) containsCredential(raw []byte) bool {
	if t.token == "" {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	for {
		token, err := d.Token()
		if err == io.EOF {
			return false
		}
		if err != nil {
			return true
		}
		if value, ok := token.(string); ok && strings.Contains(value, t.token) {
			return true
		}
	}
}
