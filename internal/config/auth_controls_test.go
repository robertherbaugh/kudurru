package config

import (
	"strings"
	"testing"
	"time"
)

func TestAuthControlsValidateCanonicalBeforeInstalledLocks(t *testing.T) {
	host, err := FromFlat(Defaults())
	if err != nil {
		t.Fatal(err)
	}
	host.Auth.LocalLogin = LocalLoginDisabled
	host.Auth.Session.IdleTimeout = Duration(2 * time.Minute)
	host.Auth.Session.Lifetime = Duration(7 * time.Minute)
	host.Auth.Session.Secure = "always"
	host.Auth.Tokens.MaxLifetime = Duration(time.Hour)
	host.EnvLocked = []string{"auth.localLogin", "auth.session.idleTimeout", "auth.session.lifetime", "auth.session.secure", "auth.tokens.maxLifetime"}
	overrides := CaptureAuthOverrides(host)
	host.Auth.Session.Lifetime = Duration(24 * time.Hour) // captured input cannot drift
	document := map[string]any{"auth": map[string]any{"localLogin": "enabled", "session": map[string]any{"idleTimeout": "3m", "lifetime": "9m", "secure": "never"}, "tokens": map[string]any{"maxLifetime": "0s"}}}
	got, err := ResolveAuthControls(document, overrides)
	want := AuthControls{LocalLogin: LocalLoginDisabled, SessionIdleTimeout: 2 * time.Minute, SessionLifetime: 7 * time.Minute, SessionSecure: "always", TokenMaxLifetime: time.Hour}
	if err != nil || got != want {
		t.Fatal("installed policy was not detached or did not win")
	}
	for _, invalid := range []map[string]any{
		{"auth": map[string]any{"session": map[string]any{"idleTimeout": "5s"}}},
		{"auth": map[string]any{"session": map[string]any{"secure": "invalid"}}},
		{"auth": map[string]any{"localLogin": "invalid"}},
		{"auth": map[string]any{"tokens": map[string]any{"maxLifetime": "-1s"}}},
		{"auth": map[string]any{"adminSocket": map[string]any{"path": "/tmp/not-public"}}},
	} {
		if _, err := ResolveAuthControls(invalid, overrides); err == nil {
			t.Fatal("installed lock hid invalid canonical input")
		}
	}
	if _, err := ResolveAuthControls(map[string]any{"auth": map[string]any{"session": map[string]any{"idleTimeout": "8m", "lifetime": "9m"}}}, AuthControlOverrides{Values: AuthControls{SessionLifetime: 7 * time.Minute}, SessionLifeLocked: true}); err == nil {
		t.Fatal("effective idle/lifetime conflict was accepted")
	}
	defaults, err := ResolveAuthControls(nil, AuthControlOverrides{})
	if err != nil || defaults.LocalLogin != LocalLoginEnabled || defaults.SessionSecure != "auto" || defaults.SessionLifetime != 12*time.Hour {
		t.Fatal("zero locks did not resolve canonical defaults")
	}
}

func TestAuthControlsRestartCapabilityRemainsRuntimeSpecific(t *testing.T) {
	count, hot := 0, 0
	for _, k := range Schema {
		if !strings.HasPrefix(k.Path, "auth.") {
			continue
		}
		count++
		if !k.RestartRequired || !RequiresRestart(k, false) {
			t.Fatal("global/legacy auth metadata lost startup semantics")
		}
		if !RequiresRestart(k, true) {
			hot++
			if k.HostOnly {
				t.Fatal("host authority advertised as hot")
			}
		}
	}
	if count != 15 || hot != 5 {
		t.Fatalf("auth schema classification changed: leaves=%d hot=%d", count, hot)
	}
}
