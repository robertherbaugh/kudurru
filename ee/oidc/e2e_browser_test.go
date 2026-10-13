package oidc

import (
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/session"
)

// browser is a cookie-jar HTTP client that never follows redirects on its
// own, so the suite can inspect and tamper with every hop.
type browser struct {
	t      *testing.T
	h      *harness
	jar    *cookiejar.Jar
	client *http.Client
}

func (h *harness) newBrowser() *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{t: h.t, h: h, jar: jar, client: &http.Client{
		Jar:           jar,
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (b *browser) do(req *http.Request) (*http.Response, string) {
	b.t.Helper()
	resp, err := b.client.Do(req)
	if err != nil {
		b.t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func (b *browser) get(u string) (*http.Response, string) {
	b.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	return b.do(req)
}

// startLogin opens /start and returns the IdP authorization URL.
func (b *browser) startLogin(next string) *url.URL {
	b.t.Helper()
	u := b.h.srv.URL + "/api/v1/auth/oidc/" + b.h.idp.id + "/start"
	if next != "" {
		u += "?next=" + url.QueryEscape(next)
	}
	resp, body := b.get(u)
	if resp.StatusCode != http.StatusFound {
		b.t.Fatalf("start: status %d: %s", resp.StatusCode, body)
	}
	loc, err := resp.Location()
	if err != nil || !strings.HasPrefix(loc.String(), b.h.idp.issuer) {
		b.t.Fatalf("start redirected to %v (%v), want the IdP %s", loc, err, b.h.idp.issuer)
	}
	q := loc.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("nonce") == "" || q.Get("state") == "" {
		b.t.Fatalf("authorization request lacks PKCE/nonce/state: %s", loc)
	}
	return loc
}

var formAction = regexp.MustCompile(`(?is)<form[^>]*\baction="([^"]*)"`)

// authenticateAtIDP walks the IdP's pages from the authorization URL,
// submits login/password on the first form, and returns the redirect back
// to kudurru (not followed).
func (b *browser) authenticateAtIDP(authURL *url.URL, login string) *url.URL {
	b.t.Helper()
	kudurru, _ := url.Parse(b.h.srv.URL)
	cur := authURL
	req, _ := http.NewRequest(http.MethodGet, cur.String(), nil)
	posted := false
	for range 20 {
		resp, body := b.do(req)
		switch {
		case resp.StatusCode >= 300 && resp.StatusCode < 400:
			loc, err := resp.Location()
			if err != nil {
				b.t.Fatalf("IdP redirect without Location: %v", err)
			}
			if loc.Host == kudurru.Host {
				return loc
			}
			cur = loc
			req, _ = http.NewRequest(http.MethodGet, cur.String(), nil)
		case resp.StatusCode == http.StatusOK && !posted:
			m := formAction.FindStringSubmatch(body)
			if m == nil {
				b.t.Fatalf("IdP page at %s has no form:\n%s", cur, body)
			}
			action, err := cur.Parse(html.UnescapeString(m[1]))
			if err != nil {
				b.t.Fatal(err)
			}
			form := url.Values{"login": {login}, "password": {e2ePassword}}
			req, _ = http.NewRequest(http.MethodPost, action.String(), strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			cur, posted = action, true
		default:
			b.t.Fatalf("IdP %s: status %d after login=%v:\n%s", cur, resp.StatusCode, posted, body)
		}
	}
	b.t.Fatal("too many IdP hops")
	return nil
}

// finish delivers the IdP's redirect to kudurru's callback.
func (b *browser) finish(cb *url.URL) (*http.Response, string) {
	b.t.Helper()
	return b.get(cb.String())
}

// login runs the whole flow and returns kudurru's callback response.
func (b *browser) login(login, next string) (*http.Response, string) {
	b.t.Helper()
	return b.finish(b.authenticateAtIDP(b.startLogin(next), login))
}

// mustLogin logs in and asserts success.
func (b *browser) mustLogin(login string) {
	b.t.Helper()
	resp, body := b.login(login, "/zones")
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/zones" {
		b.t.Fatalf("login %s: status %d Location %q: %s", login, resp.StatusCode, resp.Header.Get("Location"), body)
	}
}

// cookie returns the named cookie the jar would send to the API.
func (b *browser) cookie(name string) *http.Cookie {
	u, _ := url.Parse(b.h.srv.URL + "/api/v1/")
	for _, c := range b.jar.Cookies(u) {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func (b *browser) me() (int, session.MeResponse) {
	b.t.Helper()
	resp, body := b.get(b.h.srv.URL + "/api/v1/auth/me")
	var me session.MeResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal([]byte(body), &me); err != nil {
			b.t.Fatalf("/me body %q: %v", body, err)
		}
	}
	return resp.StatusCode, me
}

func (b *browser) mutate(csrf string, hdr map[string]string) int {
	b.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, b.h.srv.URL+"/api/v1/zones/home-example-com/records", strings.NewReader(`{}`))
	if csrf != "" {
		req.Header.Set(session.CSRFHeader, csrf)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, _ := b.do(req)
	return resp.StatusCode
}

func (b *browser) logout(csrf string) int {
	b.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, b.h.srv.URL+"/api/v1/auth/logout", nil)
	if csrf != "" {
		req.Header.Set(session.CSRFHeader, csrf)
	}
	resp, _ := b.do(req)
	return resp.StatusCode
}

// meWithCookie calls /me with an explicit session cookie value and no jar.
func (h *harness) meWithCookie(value string) int {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/api/v1/auth/me", nil)
	req.Header.Set("Cookie", session.DefaultCookieName+"="+value)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// wantLoginError asserts a refused login: status, error code in the body,
// and no session cookie.
func wantLoginError(t *testing.T, resp *http.Response, body string, status int, code string) {
	t.Helper()
	if resp.StatusCode != status || !strings.Contains(body, "("+code+")") {
		t.Fatalf("status %d body %q, want %d (%s)", resp.StatusCode, body, status, code)
	}
	for _, c := range resp.Cookies() {
		if c.Name == session.DefaultCookieName && c.MaxAge >= 0 {
			t.Fatalf("refused login set a session cookie: %+v", c)
		}
	}
}
