package main

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"slices"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
)

// This uses the production mount and real restart. Credential commands are
// single attempts; only independent reads wait for complete publication.
func TestRunServeSQLiteLocalUsersAndNodeConfig(t *testing.T) {
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	startupLevel, startupFormat := cfg.Log.Level, cfg.Log.Format
	const name = "mounted-managed-user"
	const initial = "mounted-initial-password"
	const chosen = "mounted-user-chosen-password"
	const replacement = "mounted-replacement-password"
	const final = "mounted-final-user-password"
	var uid string
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
					t.Error("user mount shutdown failed", err)
				}
			case <-time.After(15 * time.Second):
				t.Error("user mount shutdown did not join")
			}
		}
		t.Cleanup(stop)
		deadline := time.Now().Add(20 * time.Second)
		for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK {
			select {
			case err := <-done:
				t.Fatal("user mount startup ended", err)
			case <-time.After(20 * time.Millisecond):
			}
			if time.Now().After(deadline) {
				t.Fatal("user mount never became ready")
			}
		}
		base := "http://" + cfg.Listeners.API.Address + api.BasePath
		newClient := func() *http.Client {
			jar, err := cookiejar.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			return &http.Client{Jar: jar, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		}
		admin, target := newClient(), newClient()
		var status api.AuthStatus
		mountedFactorRequest(t, admin, base, http.MethodGet, "/auth/status", "", nil, http.StatusOK, &status)
		var login api.SessionInfo
		mountedFactorRequest(t, admin, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &login)
		if login.CSRF == "" {
			t.Fatal("mounted admin login lacks CSRF")
		}
		var local api.SettingsBody
		mountedFactorRequest(t, admin, base, http.MethodGet, "/node/config", login.CSRF, nil, http.StatusOK, &local)
		logConfig, ok := local.Settings["log"].(map[string]any)
		if !ok || logConfig["level"] != startupLevel || logConfig["format"] != startupFormat {
			t.Fatal("local requested logger document survived restart or changed startup values")
		}
		if restart == 0 {
			format := "text"
			if startupFormat == format {
				format = "json"
			}
			mountedFactorRequest(t, admin, base, http.MethodPatch, "/node/config", login.CSRF,
				map[string]any{"log": map[string]any{"level": "warn", "format": format}}, http.StatusOK, &local)
			if local.Persisted == nil || *local.Persisted || !slices.Contains(local.RestartRequired, "log.format") {
				t.Fatal("node config claimed persistence or live format adoption")
			}
			mountedFactorRequest(t, admin, base, http.MethodGet, "/node/config", login.CSRF, nil, http.StatusOK, &local)
			logConfig, ok = local.Settings["log"].(map[string]any)
			if !ok || logConfig["level"] != "warn" || logConfig["format"] != format {
				t.Fatal("node config readback lost accepted local values")
			}
			var created api.IAMUser
			mountedFactorRequest(t, admin, base, http.MethodPost, "/iam/users", login.CSRF,
				api.CreateLocalUserRequest{Name: name, DisplayName: "Mounted user", Password: initial}, http.StatusCreated, &created)
			if created.Name != name || created.UID == "" || created.ResourceVersion == "" || created.BuiltinAdmin || created.Disabled || !created.MustChangePassword || len(created.Groups) != 0 || created.PasswordChangedAt == nil {
				t.Fatal("create response omitted exact safe account metadata")
			}
			uid = created.UID
			observed := awaitMountedLocalUser(t, admin, base, name, func(user api.IAMUser) bool { return user.UID == uid && user.ResourceVersion == created.ResourceVersion })
			var targetLogin api.SessionInfo
			mountedFactorRequest(t, target, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: name, Password: initial}, http.StatusOK, &targetLogin)
			if !targetLogin.MustChangePassword || targetLogin.CSRF == "" {
				t.Fatal("new account bypassed password-change restriction")
			}
			observed = awaitMountedLocalUser(t, admin, base, name, func(user api.IAMUser) bool {
				return user.UID == uid && user.ResourceVersion != observed.ResourceVersion
			})
			var changed api.SessionInfo
			mountedFactorRequest(t, target, base, http.MethodPost, "/auth/password", targetLogin.CSRF,
				api.PasswordChangeRequest{CurrentPassword: initial, NewPassword: chosen}, http.StatusOK, &changed)
			if changed.MustChangePassword || changed.CSRF == "" {
				t.Fatal("API-managed account could not finish its mandatory password change")
			}
			observed = awaitMountedLocalUser(t, admin, base, name, func(user api.IAMUser) bool {
				return user.UID == uid && !user.MustChangePassword && user.ResourceVersion != observed.ResourceVersion
			})
			mountedFactorRequest(t, admin, base, http.MethodPost, "/iam/users/"+name+"/password", login.CSRF,
				api.ResetLocalUserPasswordRequest{UID: uid, ResourceVersion: created.ResourceVersion, NewPassword: replacement}, http.StatusConflict, nil)
			mountedFactorRequest(t, admin, base, http.MethodPost, "/iam/users/"+name+"/password", login.CSRF,
				api.ResetLocalUserPasswordRequest{UID: uid, ResourceVersion: observed.ResourceVersion, NewPassword: replacement}, http.StatusNoContent, nil)
			awaitMountedLocalUser(t, admin, base, name, func(user api.IAMUser) bool {
				return user.UID == uid && user.ResourceVersion != observed.ResourceVersion
			})
			mountedFactorRequest(t, target, base, http.MethodGet, "/auth/me", "", nil, http.StatusUnauthorized, nil)
			mountedFactorRequest(t, newClient(), base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: name, Password: chosen}, http.StatusUnauthorized, nil)
		} else {
			observed := awaitMountedLocalUser(t, admin, base, name, func(user api.IAMUser) bool { return user.UID == uid })
			if observed.MustChangePassword || observed.BuiltinAdmin || observed.Disabled || observed.ResourceVersion == "" {
				t.Fatal("reset account lost durable identity or restrictions on restart")
			}
		}
		var replacementLogin api.SessionInfo
		password := replacement
		if restart == 1 {
			password = final
		}
		resetClient := newClient()
		mountedFactorRequest(t, resetClient, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: name, Password: password}, http.StatusOK, &replacementLogin)
		if replacementLogin.Principal.Name != name || replacementLogin.MustChangePassword != (restart == 0) {
			t.Fatal("replacement password did not retain mandatory self-service change")
		}
		if restart == 0 {
			var changed api.SessionInfo
			mountedFactorRequest(t, resetClient, base, http.MethodPost, "/auth/password", replacementLogin.CSRF,
				api.PasswordChangeRequest{CurrentPassword: replacement, NewPassword: final}, http.StatusOK, &changed)
			if changed.MustChangePassword || changed.CSRF == "" {
				t.Fatal("reset account could not finish its mandatory password change")
			}
		}
		stop()
	}
}

func awaitMountedLocalUser(t *testing.T, client *http.Client, base, name string, ready func(api.IAMUser) bool) api.IAMUser {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var inventory api.AuthList[api.IAMUser]
		mountedFactorRequest(t, client, base, http.MethodGet, "/iam/users", "", nil, http.StatusOK, &inventory)
		for _, user := range inventory.Items {
			if user.Name == name && ready(user) {
				return user
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("mounted user publication did not reach expected identity/version")
		}
		select {
		case <-t.Context().Done():
			t.Fatal("mounted user observation canceled")
		case <-time.After(20 * time.Millisecond):
		}
	}
}
