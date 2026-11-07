package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // Fixed RFC6238 test profile.
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
)

// Exercise public provisioning and exact-UID recovery through the real server.
// Factor-bearing commands use one HTTP attempt: an ambiguous response must not
// silently replay a possibly consumed code.
func TestRunServeSQLiteTOTPEnrollmentRestartRecovery(t *testing.T) {
	cfg, _ := mountedTestConfig(t)
	var secret string
	var consumedStep int64
	for restart := range 2 {
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- runServe(ctx, cfg, "") }()
		stopped := false
		stop := func() {
			if stopped {
				return
			}
			stopped = true
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error("factor mount shutdown failed", err)
				}
			case <-time.After(15 * time.Second):
				t.Error("factor mount shutdown did not join")
			}
		}
		t.Cleanup(stop)
		deadline := time.Now().Add(20 * time.Second)
		for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK {
			select {
			case err := <-done:
				t.Fatal("factor mount startup ended", err)
			case <-time.After(20 * time.Millisecond):
			}
			if time.Now().After(deadline) {
				t.Fatal("factor mount never became ready")
			}
		}
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		client := &http.Client{Jar: jar, Timeout: 15 * time.Second}
		base := "http://" + cfg.Listeners.API.Address + api.BasePath
		loginRequest := api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}
		if restart == 1 {
			// A fresh code from the next accepted step proves the original
			// activation/replay floor survived the actual server restart.
			step := max(time.Now().Unix()/30, consumedStep+1)
			loginRequest.TOTPCode = mountedFactorCode(t, secret, step)
		}
		var login api.SessionInfo
		mountedFactorRequest(t, client, base, http.MethodPost, "/auth/login", "", loginRequest, http.StatusOK, &login)
		if login.CSRF == "" || login.Principal.Name != "admin" {
			t.Fatal("factor mount login did not issue an enrolled local session")
		}
		var status api.TOTPStatus
		mountedFactorRequest(t, client, base, http.MethodGet, "/auth/totp", login.CSRF, nil, http.StatusOK, &status)
		if status.Enabled != (restart == 1) {
			t.Fatal("factor status did not preserve durable activation")
		}
		if restart == 0 {
			var setup api.TOTPSetupResponse
			mountedFactorRequest(t, client, base, http.MethodPost, "/auth/totp/setup", login.CSRF,
				api.TOTPProofRequest{CurrentPassword: loginRequest.Password}, http.StatusOK, &setup)
			uri, err := url.Parse(setup.OTPAuthURI)
			if err != nil || uri.Scheme != "otpauth" || uri.Host != "totp" || uri.Query().Get("secret") != setup.Secret || setup.Challenge == "" || !setup.ExpiresAt.After(time.Now()) {
				t.Fatal("factor setup did not disclose valid bounded provisioning material")
			}
			secret, consumedStep = setup.Secret, time.Now().Unix()/30
			mountedFactorRequest(t, client, base, http.MethodPost, "/auth/totp/confirm", login.CSRF,
				api.TOTPConfirmRequest{CurrentPassword: loginRequest.Password, Challenge: setup.Challenge, Code: mountedFactorCode(t, secret, consumedStep)}, http.StatusNoContent, nil)
			mountedFactorRequest(t, client, base, http.MethodGet, "/auth/me", "", nil, http.StatusUnauthorized, nil)
		} else {
			var inventory api.AuthList[api.IAMUser]
			mountedFactorRequest(t, client, base, http.MethodGet, "/iam/users", login.CSRF, nil, http.StatusOK, &inventory)
			uid := ""
			for _, user := range inventory.Items {
				if user.Name == "admin" {
					uid = user.UID
				}
			}
			if uid == "" {
				t.Fatal("factor recovery inventory did not expose the account UID")
			}
			mountedFactorRequest(t, client, base, http.MethodPost, "/iam/users/admin/totp/recover", login.CSRF,
				api.TOTPRecoverRequest{UID: uid + "-recreated"}, http.StatusConflict, nil)
			mountedFactorRequest(t, client, base, http.MethodPost, "/iam/users/admin/totp/recover", login.CSRF,
				api.TOTPRecoverRequest{UID: uid}, http.StatusNoContent, nil)
			mountedFactorRequest(t, client, base, http.MethodGet, "/auth/me", "", nil, http.StatusUnauthorized, nil)
			loginRequest.TOTPCode = ""
			mountedFactorRequest(t, client, base, http.MethodPost, "/auth/login", "", loginRequest, http.StatusOK, &login)
			mountedFactorRequest(t, client, base, http.MethodGet, "/auth/totp", login.CSRF, nil, http.StatusOK, &status)
			if status.Enabled {
				t.Fatal("exact-UID recovery left the factor active")
			}
		}
		stop()
	}
}

func mountedFactorRequest(t *testing.T, client *http.Client, base, method, path, csrf string, body any, want int, out any) {
	t.Helper()
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			t.Fatal("factor request encoding failed")
		}
	}
	req, err := http.NewRequestWithContext(t.Context(), method, base+path, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal("factor request construction failed")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf != "" {
		req.Header.Set("X-Kudurru-CSRF", csrf)
	}
	reader := *client
	if method == http.MethodGet {
		// Observe the committed successor after publication. These are fresh
		// read requests only; factor mutations never use this retry transport.
		reader.Transport = mountedRetryTransport{}
	}
	response, err := reader.Do(req)
	if err != nil {
		t.Fatal("factor request transport failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != want {
		t.Fatalf("%s %s: HTTP %d, want %d", method, path, response.StatusCode, want)
	}
	if out != nil && json.NewDecoder(io.LimitReader(response.Body, 32<<10)).Decode(out) != nil {
		t.Fatal("factor response decoding failed")
	}
	if path == "/auth/totp/setup" && (response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Referrer-Policy") != "no-referrer") {
		t.Fatal("factor provisioning response lacks private response headers")
	}
}

func mountedFactorCode(t *testing.T, secret string, step int64) string {
	t.Helper()
	raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil || len(raw) != 20 || step < 0 {
		t.Fatal("invalid test provisioning material")
	}
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(step)) // #nosec G115 -- nonnegative bounded current test time.
	mac := hmac.New(sha1.New, raw)
	_, _ = mac.Write(counter[:])
	digest := mac.Sum(nil)
	offset := int(digest[len(digest)-1] & 15)
	return fmt.Sprintf("%06d", (binary.BigEndian.Uint32(digest[offset:offset+4])&0x7fffffff)%1000000)
}
