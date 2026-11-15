package browseroidc

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"k8s.io/apimachinery/pkg/util/validation"
)

// LogoutLifetime bounds a continuation and its callback, never a user session.
const LogoutLifetime = 2 * time.Minute

// LogoutBinding is detached original issuance provenance, not current authority.
type LogoutBinding struct {
	Provider, ProviderUID, Origin, SpecIdentity, ResourceIdentity string
	Issuer, ClientID, RedirectURI                                 string
}

type logoutPayload struct {
	Version   int           `json:"version"`
	Phase     string        `json:"phase"`
	Binding   LogoutBinding `json:"binding"`
	SessionID string        `json:"sessionID"`
	IDToken   string        `json:"idToken"`
	State     string        `json:"state"`
	IssuedAt  time.Time     `json:"issuedAt"`
	ExpiresAt time.Time     `json:"expiresAt"`
}

// Logout holds bounded private redirect evidence. It cannot authenticate users,
// authorize commands or extend the original token or enrolled session deadlines.
type Logout struct{ value logoutPayload }

func (Logout) String() string { return "private browser logout evidence" }

// GoString redacts token hints and state.
func (l Logout) GoString() string { return l.String() }

// MarshalJSON prevents token hints and state from entering diagnostics.
func (Logout) MarshalJSON() ([]byte, error) { return []byte(`"private logout evidence"`), nil }

// NewLogoutHint is called only after original browser proof verification. The
// trusted caller supplies its verified token expiry and local session deadlines.
func NewLogoutHint(binding LogoutBinding, sessionID, rawIDToken string, issuedAt, expiresAt time.Time) (Logout, error) {
	l := Logout{logoutPayload{Version: 1, Phase: "hint", Binding: binding, SessionID: sessionID, IDToken: rawIDToken, IssuedAt: issuedAt.UTC(), ExpiresAt: expiresAt.UTC()}}
	if err := l.ValidAt(issuedAt); err != nil {
		return Logout{}, err
	}
	return l, nil
}

// Binding returns only detached nonsecret provenance.
func (l Logout) Binding() LogoutBinding { return l.value.Binding }

// SessionID pins the exact enrollment that must be locally revoked first.
func (l Logout) SessionID() string { return l.value.SessionID }

// ExpiresAt is the original deadline, never refreshed on open.
func (l Logout) ExpiresAt() time.Time { return l.value.ExpiresAt }

// Phase separates enrollment hints, committed continuations and callbacks.
func (l Logout) Phase() string { return l.value.Phase }

// Continue derives a bounded redirect only after the caller commits revocation.
func (l Logout) Continue(now time.Time) (Logout, error) {
	if l.value.Phase != "hint" || l.ValidAt(now) != nil {
		return Logout{}, ErrInvalid
	}
	l.value.Phase = "continue"
	if deadline := now.Add(LogoutLifetime); deadline.Before(l.value.ExpiresAt) {
		l.value.ExpiresAt = deadline.UTC()
	}
	var random [32]byte
	_, _ = rand.Read(random[:])
	l.value.State = base64.RawURLEncoding.EncodeToString(random[:])
	return l, nil
}

// Callback discards the token before sealing the return-state cookie.
func (l Logout) Callback(now time.Time) (Logout, error) {
	if l.value.Phase != "continue" || l.ValidAt(now) != nil {
		return Logout{}, ErrInvalid
	}
	l.value.Phase = "callback"
	l.value.IDToken = ""
	return l, nil
}

// MatchState compares the exact bounded return state in constant time.
func (l Logout) MatchState(s string) bool {
	return randomValue(s) && subtle.ConstantTimeCompare([]byte(s), []byte(l.value.State)) == 1
}

// ValidAt rejects clock rollback and expiry equality for every phase.
func (l Logout) ValidAt(now time.Time) error {
	p, b := l.value, l.value.Binding
	if p.Version != 1 || len(validation.IsDNS1123Label(b.Provider)) != 0 || !logoutText(b.ProviderUID, 512) || b.Origin != "object" && b.Origin != "config" || !logoutText(b.SpecIdentity, 64) || len(b.SpecIdentity) != 64 || !logoutText(b.ResourceIdentity, 64) || len(b.ResourceIdentity) != 64 || !logoutText(b.Issuer, 2048) || !logoutText(b.ClientID, 1024) || !logoutText(b.RedirectURI, 2048) || !randomValue(p.SessionID) {
		return ErrInvalid
	}
	if !validTime(now) || !validTime(p.IssuedAt) || !validTime(p.ExpiresAt) || !p.ExpiresAt.After(p.IssuedAt) || now.Before(p.IssuedAt) || !now.Before(p.ExpiresAt) {
		return ErrTime
	}
	switch p.Phase {
	case "hint":
		if !logoutText(p.IDToken, 2000) || p.State != "" {
			return ErrInvalid
		}
	case "continue":
		if !logoutText(p.IDToken, 2000) || !randomValue(p.State) {
			return ErrInvalid
		}
	case "callback":
		if p.IDToken != "" || !randomValue(p.State) {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}
func logoutText(s string, limit int) bool {
	if len(s) == 0 || len(s) > limit {
		return false
	}
	for _, c := range s {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

// LogoutTargets derives both targets from the configured login callback, never
// from Host, forwarded headers, next, provider query parameters or browser input.
func LogoutTargets(binding LogoutBinding) (callback, local string, err error) {
	u, e := url.Parse(binding.RedirectURI)
	suffix := "/api/v1/auth/oidc/" + binding.Provider + "/callback"
	if e != nil || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.RawPath != "" || u.Scheme != "https" && u.Scheme != "http" || !strings.HasSuffix(u.Path, suffix) {
		return "", "", ErrInvalid
	}
	local = strings.TrimSuffix(u.Path, suffix) + "/"
	if _, e = SafeNext(local); e != nil {
		return "", "", ErrInvalid
	}
	u.Path = strings.TrimSuffix(u.Path, "/callback") + "/logout/callback"
	return u.String(), local, nil
}

// RedirectURL exposes the hint only in the deliberate provider redirect. The
// endpoint must already be validated by the captured discovery/network owner.
func (l Logout) RedirectURL(endpoint string, now time.Time) (string, error) {
	if l.value.Phase != "continue" || l.ValidAt(now) != nil {
		return "", ErrInvalid
	}
	callback, _, err := LogoutTargets(l.value.Binding)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || u.Scheme != "https" && u.Scheme != "http" || len(endpoint) > 4096 {
		return "", ErrInvalid
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(q) > 16 {
		return "", ErrInvalid
	}
	for _, key := range []string{"id_token_hint", "logout_hint", "client_id", "post_logout_redirect_uri", "state", "ui_locales", "client_secret", "redirect_uri", "request", "request_uri"} {
		if _, ok := q[key]; ok {
			return "", ErrInvalid
		}
	}
	q.Set("id_token_hint", l.value.IDToken)
	q.Set("client_id", l.value.Binding.ClientID)
	q.Set("post_logout_redirect_uri", callback)
	q.Set("state", l.value.State)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// LogoutCodec uses a distinct purpose from enrolled sessions and login preauth.
type LogoutCodec struct{ sealer *session.Sealer }

func (*LogoutCodec) String() string { return "private browser logout codec" }

// GoString redacts captured keys.
func (c *LogoutCodec) GoString() string { return c.String() }

// MarshalJSON prevents accidental key persistence.
func (*LogoutCodec) MarshalJSON() ([]byte, error) { return []byte(`"private logout codec"`), nil }

// NewLogoutCodec captures current/previous keys without I/O.
func NewLogoutCodec(current, previous []byte) (*LogoutCodec, error) {
	s, e := session.NewSealer("oidc-browser-logout/v1", current, previous)
	if e != nil {
		return nil, ErrInvalid
	}
	return &LogoutCodec{s}, nil
}

// Seal encrypts private redirect evidence under a bounded cookie format.
func (c *LogoutCodec) Seal(l Logout, now time.Time) (string, error) {
	if c == nil || c.sealer == nil || l.ValidAt(now) != nil {
		return "", ErrInvalid
	}
	raw, _ := json.Marshal(l.value)
	defer clear(raw)
	if len(raw) > maxPlainBytes || session.SealedLen(len(raw)) > MaxCookieBytes {
		return "", ErrBounds
	}
	return c.sealer.Seal(raw), nil
}

// Open authenticates the exact canonical format before exposing evidence.
func (c *LogoutCodec) Open(raw string, now time.Time) (Logout, error) {
	if c == nil || c.sealer == nil || len(raw) > MaxCookieBytes {
		return Logout{}, ErrInvalid
	}
	plain, _, e := c.sealer.Open(raw)
	if e != nil {
		return Logout{}, ErrInvalid
	}
	defer clear(plain)
	if len(plain) > maxPlainBytes {
		return Logout{}, ErrBounds
	}
	var p logoutPayload
	d := json.NewDecoder(bytes.NewReader(plain))
	d.DisallowUnknownFields()
	if d.Decode(&p) != nil || d.Decode(new(any)) != io.EOF {
		return Logout{}, ErrInvalid
	}
	canonical, _ := json.Marshal(p)
	defer clear(canonical)
	if !bytes.Equal(plain, canonical) {
		return Logout{}, ErrInvalid
	}
	l := Logout{p}
	if e = l.ValidAt(now); e != nil {
		return Logout{}, e
	}
	return l, nil
}
