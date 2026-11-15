package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type totpTransport func(*http.Request) (*http.Response, error)

func (f totpTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAuthTOTPBodyAndExistingLogin(t *testing.T) {
	for _, factor := range []bool{false, true} {
		t.Run(strconv.FormatBool(factor), func(t *testing.T) {
			var posts, reads atomic.Int32
			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/auth/login" {
					posts.Add(1)
					var body map[string]string
					if json.NewDecoder(r.Body).Decode(&body) != nil || body["username"] != "local" || body["password"] != "test-password" {
						t.Error("credential body mismatch")
					}
					code, present := body["totpCode"]
					if present != factor || (factor && code != "012345") || len(body) != 2+btoi(factor) {
						t.Error("factor body mismatch")
					}
					http.SetCookie(w, &http.Cookie{Name: "session", Value: "fixture", Path: "/", HttpOnly: true}) //nolint:gosec // Plain HTTP fixture.
					_, _ = io.WriteString(w, `{"csrf":"initial-csrf"}`)
					return
				}
				reads.Add(1)
				if r.URL.Path != "/api/v1/auth/me" || r.Method != http.MethodGet || r.Header.Get("Cookie") == "" {
					t.Error("login observation lost cookie")
				}
				_, _ = io.WriteString(w, `{"principal":{"name":"local"},"csrf":"current-csrf"}`)
			}), Options{})
			var me *Me
			var err error
			if factor {
				me, err = c.LoginWithTOTP(t.Context(), "local", "test-password", "012345")
			} else {
				me, err = c.Login(t.Context(), "local", "test-password")
			}
			if err != nil || me == nil || posts.Load() != 1 || reads.Load() != 1 || c.csrf != "current-csrf" || c.Token() != "" {
				t.Fatal("single login/cookie/CSRF contract failed")
			}
		})
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestAuthTOTPInvalidBeforeNetwork(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }), Options{})
	for _, code := range []string{"", "12345", "1234567", "12345x", "１２３４５６", " 12345", "123456\n", "+12345", "12345\r"} {
		if _, err := c.LoginWithTOTP(t.Context(), "local", "password", code); err == nil {
			t.Error("invalid factor accepted")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid factor reached transport")
	}
}

func TestAuthPostsNeverRedirectOrMutateClient(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		for _, external := range []bool{false, true} {
			for _, operation := range []string{"password", "totp", "mint", "logout"} {
				t.Run(strconv.Itoa(status)+"/"+strconv.FormatBool(external)+"/"+operation, func(t *testing.T) {
					var destinations, posts, redirects atomic.Int32
					dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						destinations.Add(1)
						_, _ = io.WriteString(w, `{}`)
					}))
					defer dst.Close()
					src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/destination" {
							destinations.Add(1)
							_, _ = io.WriteString(w, `{}`)
							return
						}
						if r.Method == http.MethodPost {
							posts.Add(1)
						}
						location := "/destination"
						if external {
							location = dst.URL + location
						}
						http.Redirect(w, r, location, status)
					}))
					defer src.Close()
					hc := src.Client()
					hc.CheckRedirect = func(*http.Request, []*http.Request) error { redirects.Add(1); return nil }
					c, err := New(src.URL, Options{HTTPClient: hc})
					if err != nil {
						t.Fatal("client construction failed")
					}
					switch operation {
					case "password":
						_, err = c.Login(t.Context(), "local", "password")
					case "totp":
						_, err = c.LoginWithTOTP(t.Context(), "local", "password", "012345")
					case "mint":
						_, err = c.CreateToken(t.Context(), CreateTokenRequest{})
					case "logout":
						err = c.Logout(t.Context())
					}
					if !IsStatus(err, status) || posts.Load() != 1 || destinations.Load() != 0 || redirects.Load() != 0 {
						t.Fatal("auth POST followed redirect or repeated")
					}
					if err = c.Get(t.Context(), "/ordinary", nil, nil); err != nil || destinations.Load() != 1 || redirects.Load() != 1 || hc.CheckRedirect == nil {
						t.Fatal("auth request changed generic/shared redirect policy")
					}
				})
			}
		}
	}
}

func TestAuthPostsNoReplayAndSafeErrors(t *testing.T) {
	const reflected = "sensitive-proof-marker"
	for _, failure := range []string{"transport", "empty", "malformed", "rejected", "pending", "me"} {
		t.Run(failure, func(t *testing.T) {
			var calls atomic.Int32
			tr := totpTransport(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				if r.Method == http.MethodPost && (r.GetBody != nil || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-Idempotency-Key") != "") {
					t.Error("auth request is replayable")
				}
				if failure == "transport" {
					return nil, errors.New(reflected)
				}
				status, body := 200, ""
				switch failure {
				case "malformed":
					body = reflected
				case "rejected", "me":
					status, body = 401, `{"error":{"key":"`+reflected+`","message":"`+reflected+`","hint":"`+reflected+`"}}`
					if failure == "me" && r.Method == http.MethodPost {
						status, body = 200, `{}`
					}
				case "pending":
					status, body = 503, `{"error":{"key":"generation_pending","message":"`+reflected+`"}}`
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": {"999999999"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})
			c, err := New("https://fixture.invalid", Options{HTTPClient: &http.Client{Transport: tr}})
			if err != nil {
				t.Fatal("client construction failed")
			}
			_, err = c.LoginWithTOTP(t.Context(), "local", reflected, "012345")
			want := int32(1)
			if failure == "me" {
				want = 2 // one login, one observation; never another login
			}
			if err == nil || strings.Contains(err.Error(), reflected) || calls.Load() != want {
				t.Fatal("proof reflected or login repeated")
			}
			var apiErr *Error
			if errors.As(err, &apiErr) && (apiErr.Hint != "" || apiErr.Missing != nil || apiErr.RetryAfter > 24*time.Hour) {
				t.Fatal("unsafe error metadata retained")
			}
		})
	}
	c, _ := New("https://fixture.invalid", Options{HTTPClient: &http.Client{Transport: totpTransport(func(r *http.Request) (*http.Response, error) {
		if r.GetBody != nil || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-Idempotency-Key") != "" {
			t.Error("single-use request retained replay facilities")
		}
		return nil, context.Canceled
	})}})
	_, err := c.raw(t.Context(), Request{Method: http.MethodPost, Path: "/auth/login", Body: bytes.NewReader([]byte("fixture")), Header: http.Header{"Idempotency-Key": {"fixture"}, "X-Idempotency-Key": {"fixture"}}}, true)
	if !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation not preserved")
	}
}

func TestAuthPostsBoundSuccessfulResponses(t *testing.T) {
	const reflected = "oversized-proof-marker"
	for _, operation := range []string{"password", "totp", "mint", "logout"} {
		t.Run(operation, func(t *testing.T) {
			var calls atomic.Int32
			c, _ := New("https://fixture.invalid", Options{HTTPClient: &http.Client{Transport: totpTransport(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				body := `{"csrf":"` + reflected + strings.Repeat("x", maxErrorBody) + `"}`
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}})
			var err error
			switch operation {
			case "password":
				_, err = c.Login(t.Context(), "local", "password")
			case "totp":
				_, err = c.LoginWithTOTP(t.Context(), "local", "password", "012345")
			case "mint":
				_, err = c.CreateToken(t.Context(), CreateTokenRequest{})
			case "logout":
				err = c.Logout(t.Context())
			}
			if err == nil || strings.Contains(err.Error(), reflected) || calls.Load() != 1 {
				t.Fatal("oversized auth response accepted, reflected or retried")
			}
		})
	}
}
