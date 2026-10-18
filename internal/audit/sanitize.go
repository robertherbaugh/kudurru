package audit

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// Sanitize returns ev with every string valid UTF-8 and free of NUL
// bytes, each offending byte replaced by U+FFFD. Request fields come from
// clients (a %00 in a path, any bytes in a User-Agent); a text column
// (PostgreSQL) refuses them and JSON encoding (spool, AuditLog) would
// replace them after the hash was taken. Recorders sanitize before
// chaining; Sanitize is idempotent, so a stored event re-hashes the same.
func Sanitize(ev Event) Event {
	ev.Node = clean(ev.Node)
	ev.Actor.Kind, ev.Actor.ID, ev.Actor.Name, ev.Actor.Auth = clean(ev.Actor.Kind), clean(ev.Actor.ID), clean(ev.Actor.Name), clean(ev.Actor.Auth)
	if dirty(ev.Actor.Groups...) {
		groups := make([]string, len(ev.Actor.Groups))
		for i, g := range ev.Actor.Groups {
			groups[i] = clean(g)
		}
		ev.Actor.Groups = groups
	}
	ev.Action, ev.Result, ev.Reason, ev.Authz, ev.Manager, ev.ResourceVersion =
		clean(ev.Action), clean(ev.Result), clean(ev.Reason), clean(ev.Authz), clean(ev.Manager), clean(ev.ResourceVersion)
	ev.Resource.Kind, ev.Resource.Namespace, ev.Resource.Name = clean(ev.Resource.Kind), clean(ev.Resource.Namespace), clean(ev.Resource.Name)
	r := &ev.Request
	r.ID, r.IP, r.UA, r.Method, r.Path = clean(r.ID), clean(r.IP), clean(r.UA), clean(r.Method), clean(r.Path)
	ev.Before, ev.After = cleanDoc(ev.Before), cleanDoc(ev.After)
	return ev
}

func dirty(ss ...string) bool {
	for _, s := range ss {
		if !utf8.ValidString(s) || strings.IndexByte(s, 0) >= 0 {
			return true
		}
	}
	return false
}

func clean(s string) string {
	if !dirty(s) {
		return s
	}
	return strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", "�"), "�")
}

// cleanDoc keeps a document's bytes unless they are not storable text
// (json.Marshal never produces such bytes; a document from elsewhere may).
func cleanDoc(b json.RawMessage) json.RawMessage {
	if b == nil || !dirty(string(b)) {
		return b
	}
	return json.RawMessage(clean(string(b)))
}

// Truncate cuts s to at most n bytes without splitting a UTF-8 sequence.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
