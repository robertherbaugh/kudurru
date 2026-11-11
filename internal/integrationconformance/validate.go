package integrationconformance

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strings"

	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/ee/integration/externaldns"
)

// Check duplicate (including case-folded) keys and bounded nesting before typed
// decoding. Unknown fields are errors in this versioned protocol, not evidence
// that an unimplemented extension passed.
func validJSON(b []byte) bool {
	if len(b) == 0 || len(b) > MaxBody {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	budget := 1 << 20
	var value func(int) bool
	value = func(depth int) bool {
		budget--
		if depth > 32 || budget < 0 {
			return false
		}
		tok, err := d.Token()
		if err != nil {
			return false
		}
		delim, compound := tok.(json.Delim)
		if !compound {
			return true
		}
		if delim != '[' && delim != '{' {
			return false
		}
		keys := map[string]bool{}
		count := 0
		for d.More() {
			count++
			if count > MaxItems {
				return false
			}
			if delim == '{' {
				key, e := d.Token()
				if e != nil {
					return false
				}
				name, ok := key.(string)
				if !ok {
					return false
				}
				name = strings.ToLower(name)
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
		return err == nil && (delim == '[' && end == json.Delim(']') || delim == '{' && end == json.Delim('}'))
	}
	if !value(0) {
		return false
	}
	_, err := d.Token()
	return err == io.EOF
}

func decode(b []byte, target any) bool {
	if !validJSON(b) {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	return d.Decode(target) == nil
}

func jsonObject(b []byte) bool {
	b = bytes.TrimSpace(b)
	return len(b) > 0 && b[0] == '{' && validJSON(b)
}

func endpoints(eps []*externaldns.Endpoint, requireTargets bool) bool {
	if len(eps) > MaxItems {
		return false
	}
	for _, ep := range eps {
		if ep == nil || ep.DNSName == "" || len(ep.DNSName) > 255 || ep.RecordType == "" || len(ep.RecordType) > 32 || ep.RecordTTL < 0 || requireTargets && len(ep.Targets) == 0 || len(ep.Targets) > MaxItems {
			return false
		}
		for _, target := range ep.Targets {
			if target == "" || len(target) > 64<<10 {
				return false
			}
		}
	}
	return true
}

func validEndpoints(b []byte) bool {
	var eps []*externaldns.Endpoint
	return decode(b, &eps) && endpoints(eps, true) && bytes.HasPrefix(bytes.TrimSpace(b), []byte("["))
}

func validChanges(b []byte) bool {
	var ch externaldns.Changes
	// Removal identifies the old name/type/set; it need not repeat targets.
	return jsonObject(b) && decode(b, &ch) && endpoints(ch.Create, true) && endpoints(ch.UpdateOld, false) && endpoints(ch.UpdateNew, true) && endpoints(ch.Delete, false)
}

func validEvent(b []byte) bool {
	var ev integration.Event
	return decode(b, &ev) && ev.Topic != "" && len(ev.Topic) <= 256 && !ev.Time.IsZero() && len(ev.Payload) > 0
}

func actionName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

func validActions(b []byte) ([]integration.ActionSpec, bool) {
	var actions []integration.ActionSpec
	if !decode(b, &actions) || !bytes.HasPrefix(bytes.TrimSpace(b), []byte("[")) {
		return nil, false
	}
	seen := map[string]bool{}
	for _, action := range actions {
		if !actionName(action.Name) || seen[action.Name] || action.Verb != "integrations.act" || !jsonObject(action.Params) {
			return nil, false
		}
		seen[action.Name] = true
	}
	return actions, true
}

func validFilter(b []byte) bool {
	var df externaldns.DomainFilter
	if !jsonObject(b) || !decode(b, &df) {
		return false
	}
	for _, pattern := range []string{df.RegexInclude, df.RegexExclude} {
		if len(pattern) > 4096 {
			return false
		}
		if _, err := regexp.Compile(pattern); err != nil {
			return false
		}
	}
	return true
}

type declaration struct {
	Filters      []string             `json:"filters"`
	Capabilities []string             `json:"capabilities"`
	Manifest     integration.Manifest `json:"manifest"`
}

func validDeclaration(b []byte) (map[string]bool, bool) {
	var d declaration
	if !decode(b, &d) || d.Manifest.Type == "" || d.Manifest.DisplayName == "" || d.Manifest.Version == "" || !jsonObject(d.Manifest.ConfigSchema) {
		return nil, false
	}
	switch d.Manifest.IOClass {
	case "cloud_polling", "local_polling", "local_push":
	default:
		return nil, false
	}
	caps := map[string]bool{}
	for _, cap := range d.Capabilities {
		if caps[cap] {
			return nil, false
		}
		switch cap {
		case "records", "clients", "hooks", "actions":
		default:
			return nil, false
		}
		caps[cap] = true
	}
	manifestCaps := map[string]bool{}
	for _, cap := range d.Manifest.Capabilities {
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
			return nil, false
		}
		if manifestCaps[wire] || !caps[wire] {
			return nil, false
		}
		manifestCaps[wire] = true
	}
	return caps, len(caps) > 0 && len(caps) == len(manifestCaps)
}

func validInventory(b []byte, clients bool) bool {
	if !bytes.HasPrefix(bytes.TrimSpace(b), []byte("[")) {
		return false
	}
	if clients {
		var items []*integration.Client
		if !decode(b, &items) {
			return false
		}
		for _, item := range items {
			if item == nil {
				return false
			}
		}
		return true
	}
	var items []*integration.Network
	if !decode(b, &items) {
		return false
	}
	for _, item := range items {
		if item == nil {
			return false
		}
	}
	return true
}

// expectation is a read-back probe: the record list must contain every
// endpoint of Contain (same name, type and set identifier, the same target
// set and, when given, the same TTL) and none with the name, type and set
// identifier of an endpoint of Absent.
type expectation struct {
	Contain []*externaldns.Endpoint `json:"contain,omitempty"`
	Absent  []*externaldns.Endpoint `json:"absent,omitempty"`
}

func decodeExpectation(b []byte) *expectation {
	var e expectation
	if !jsonObject(b) || !decode(b, &e) || !endpoints(e.Contain, true) || !endpoints(e.Absent, false) || len(e.Contain)+len(e.Absent) == 0 || len(e.Contain)+len(e.Absent) > MaxItems {
		return nil
	}
	return &e
}

func endpointIdentity(e *externaldns.Endpoint) string {
	return strings.ToLower(strings.TrimSuffix(e.DNSName, ".")) + "\x00" + strings.ToUpper(e.RecordType) + "\x00" + e.SetIdentifier
}

// readBack reports whether listed meets the expectation's two halves.
func (e *expectation) readBack(listed []*externaldns.Endpoint) (contains, absent bool) {
	byIdentity := map[string]*externaldns.Endpoint{}
	for _, l := range listed {
		byIdentity[endpointIdentity(l)] = l
	}
	contains = true
	for _, want := range e.Contain {
		got := byIdentity[endpointIdentity(want)]
		if got == nil || want.RecordTTL != 0 && got.RecordTTL != want.RecordTTL || !sameTargets(got.Targets, want.Targets) {
			contains = false
		}
	}
	absent = true
	for _, gone := range e.Absent {
		if byIdentity[endpointIdentity(gone)] != nil {
			absent = false
		}
	}
	return contains, absent
}

func sameTargets(a, b []string) bool {
	set := map[string]bool{}
	for _, t := range a {
		set[t] = true
	}
	other := map[string]bool{}
	for _, t := range b {
		if !set[t] {
			return false
		}
		other[t] = true
	}
	return len(set) == len(other)
}
