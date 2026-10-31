package browseroidc

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

func logoutFixture(t *testing.T) (*LogoutCodec, Logout, time.Time) {
	t.Helper()
	now := time.Now().UTC()
	codec, err := NewLogoutCodec([]byte(strings.Repeat("k", 32)), nil)
	if err != nil {
		t.Fatal(err)
	}
	l, err := NewLogoutHint(LogoutBinding{Provider: "corp", ProviderUID: "original-uid", Origin: "config", SpecIdentity: strings.Repeat("a", 64), ResourceIdentity: strings.Repeat("b", 64), Issuer: "https://idp.example", ClientID: "client", RedirectURI: "https://app.example/prefix/api/v1/auth/oidc/corp/callback"}, strings.Repeat("A", 43), "private-id-token", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return codec, l, now
}
func TestLogoutPrivatePhasesOriginalTimeAndPurpose(t *testing.T) {
	codec, hint, now := logoutFixture(t)
	wire, err := codec.Seal(hint, now)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := codec.Open(wire, now)
	if err != nil || opened.Binding() != hint.Binding() || opened.Phase() != "hint" {
		t.Fatal("hint binding changed")
	}
	for _, at := range []time.Time{now.Add(-time.Nanosecond), hint.ExpiresAt(), hint.ExpiresAt().Add(time.Second)} {
		if _, err = codec.Open(wire, at); err == nil {
			t.Fatal("original deadline was extended")
		}
	}
	login, _ := NewCodec([]byte(strings.Repeat("k", 32)), nil)
	if _, err = login.Open(wire, now); err == nil {
		t.Fatal("logout hint became login preauth")
	}
	next, err := hint.Continue(now.Add(time.Second))
	if err != nil || next.ExpiresAt() != hint.ExpiresAt() {
		t.Fatal("continuation renewed original expiry")
	}
	target, err := next.RedirectURL("https://idp.example/end?tenant=one", now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(target)
	q := u.Query()
	if q.Get("id_token_hint") != "private-id-token" || q.Get("post_logout_redirect_uri") != "https://app.example/prefix/api/v1/auth/oidc/corp/logout/callback" || q.Get("client_id") != "client" || !next.MatchState(q.Get("state")) {
		t.Fatal("incorrect explicit RP logout profile")
	}
	state, err := next.Callback(now.Add(time.Second))
	if err != nil || state.value.IDToken != "" || !state.MatchState(q.Get("state")) || state.MatchState(strings.Repeat("z", 43)) {
		t.Fatal("callback state or discarded hint")
	}
	if _, err = state.RedirectURL("https://idp.example/end", now.Add(time.Second)); err == nil {
		t.Fatal("callback issued another provider redirect")
	}
	for _, private := range []string{"private-id-token", q.Get("state")} {
		raw, _ := json.Marshal(hint)
		if strings.Contains(fmt.Sprintf("%v %#v %s", hint, hint, raw), private) {
			t.Fatal("private logout material entered diagnostics")
		}
	}
}
func TestLogoutRedirectRefusalsAndCookieBounds(t *testing.T) {
	codec, hint, now := logoutFixture(t)
	next, _ := hint.Continue(now)
	for _, endpoint := range []string{"//idp.example/end", "https://user@idp.example/end", "https://idp.example/end#fragment", "javascript:logout", "https://idp.example/end?state=x", "https://idp.example/end?id_token_hint=x", "https://idp.example/end?post_logout_redirect_uri=https://evil.example", "https://idp.example/end?client_id=x", "https://idp.example/end?bad=%zz"} {
		if _, err := next.RedirectURL(endpoint, now); err == nil {
			t.Fatal("unsafe endpoint accepted")
		}
	}
	for _, redirect := range []string{"https://app.example/other/callback", "https://app.example/api/v1/auth/oidc/other/callback", "https://user@app.example/api/v1/auth/oidc/corp/callback", "https://app.example/api/v1/auth/oidc/corp/callback?next=https://evil.example", "https://app.example/api/v1/auth/oidc/corp/callback#x", "https://app.example/%2f/api/v1/auth/oidc/corp/callback", "https://app.example/../api/v1/auth/oidc/corp/callback"} {
		b := hint.Binding()
		b.RedirectURI = redirect
		if _, _, err := LogoutTargets(b); err == nil {
			t.Fatal("unsafe callback derivation accepted")
		}
	}
	hint.value.IDToken = strings.Repeat("x", 2000)
	if _, err := codec.Seal(hint, now); err == nil {
		t.Fatal("oversize cookie accepted")
	}
	if _, err := codec.Open(strings.Repeat("x", MaxCookieBytes+1), now); err == nil {
		t.Fatal("oversize cookie input accepted")
	}
	old, _ := NewLogoutCodec([]byte(strings.Repeat("p", 32)), nil)
	wire, _ := old.Seal(next, now)
	rotated, _ := NewLogoutCodec([]byte(strings.Repeat("n", 32)), []byte(strings.Repeat("p", 32)))
	if _, err := rotated.Open(wire, now); err != nil {
		t.Fatal("original previous-key compatibility lost")
	}
	unrelated, _ := NewLogoutCodec([]byte(strings.Repeat("u", 32)), nil)
	if _, err := unrelated.Open(wire, now); err == nil {
		t.Fatal("unrelated key accepted")
	}
}
