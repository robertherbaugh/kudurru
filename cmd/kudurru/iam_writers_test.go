package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
)

// TestIAMWritersCLIReviewedCommands drives the full command parser against
// the real router of the complete SQLite runtime.
func TestIAMWritersCLIReviewedCommands(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	srv := httptest.NewServer(f.server)
	defer srv.Close()
	run := func(stdin, manifest string, args ...string) (string, error) {
		t.Helper()
		localUsersFresh(t, f)
		o := newCLIOpts()
		o.stdin = strings.NewReader(stdin)
		root := newRootWith(o)
		var out strings.Builder
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetIn(strings.NewReader(manifest))
		root.SetArgs(append([]string{"--server", srv.URL, "--token", f.token, "-o", "json", "iam"}, args...))
		err := root.ExecuteContext(t.Context())
		return out.String(), err
	}
	decode := func(raw string, out any) {
		t.Helper()
		if err := json.Unmarshal([]byte(raw), out); err != nil {
			t.Fatalf("CLI output %q: %v", raw, err)
		}
	}

	// Local accounts.
	raw, err := run("cli-initial-password\n", "", "users", "create", "cli-person", "--display-name", "CLI person", "--password-stdin")
	if err != nil {
		t.Fatal("users create", err)
	}
	var user api.IAMUser
	decode(raw, &user)
	if user.Name != "cli-person" || !user.MustChangePassword || strings.Contains(raw, "cli-initial-password") {
		t.Fatal("users create output", raw)
	}
	if _, err := run("", "", "users", "update", "cli-person", "--uid", user.UID, "--resource-version", user.ResourceVersion); err == nil {
		t.Fatal("update without a field was sent")
	}
	raw, err = run("", "", "users", "update", "cli-person", "--uid", user.UID, "--resource-version", user.ResourceVersion, "--group", "operators", "--disabled")
	if err != nil {
		t.Fatal("users update", err)
	}
	var updated api.IAMUser
	decode(raw, &updated)
	if !updated.Disabled || len(updated.Groups) != 1 || updated.Groups[0] != "operators" {
		t.Fatal("users update output", raw)
	}
	if _, err := run("cli-replacement-password\n", "", "users", "reset-password", "cli-person", "--uid", user.UID, "--resource-version", user.ResourceVersion, "--password-stdin"); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatal("stale reset was not refused as a conflict", err)
	}
	if _, err := run("", "", "users", "delete", "cli-person", "--uid", updated.UID, "--resource-version", updated.ResourceVersion); err != nil {
		t.Fatal("users delete", err)
	}

	// Service accounts and one-time token display.
	manifest := `apiVersion: kudurru.io/v1alpha1
kind: ServiceAccount
metadata:
  name: cli-robot
  namespace: apps
spec:
  description: CLI robot
`
	raw, err = run("", manifest, "serviceaccounts", "create", "-f", "-")
	if err != nil {
		t.Fatal("serviceaccounts create", err)
	}
	var sa publicv1.ServiceAccount
	decode(raw, &sa)
	if sa.UID == "" || sa.Spec.Description != "CLI robot" {
		t.Fatal("serviceaccounts create output", raw)
	}
	if _, err := run("", "", "sa", "create-token", "apps", "cli-robot"); err == nil {
		t.Fatal("token issued without an exact uid")
	}
	raw, err = run("", "", "sa", "create-token", "apps", "cli-robot", "--uid", string(sa.UID), "--expires-in", "1h", "--description", "cli")
	if err != nil {
		t.Fatal("create-token", err)
	}
	var token api.CreatedToken
	decode(raw, &token)
	if !strings.HasPrefix(token.Token, "kdr_") || token.Owner.UID != string(sa.UID) {
		t.Fatal("create-token output", raw)
	}
	if _, err := run("", strings.Replace(manifest, "cli-robot", "other", 1), "sa", "replace", "apps", "cli-robot", "-f", "-", "--uid", string(sa.UID), "--resource-version", sa.ResourceVersion); err == nil {
		t.Fatal("mismatched manifest and target was sent")
	}
	raw, err = run("", strings.Replace(manifest, "description: CLI robot", "disabled: true", 1), "sa", "replace", "apps", "cli-robot", "-f", "-", "--uid", string(sa.UID), "--resource-version", sa.ResourceVersion)
	if err != nil {
		t.Fatal("serviceaccounts replace", err)
	}
	var disabled publicv1.ServiceAccount
	decode(raw, &disabled)
	if !disabled.Spec.Disabled || ownedTokens(t, f.s.opts.Store, string(sa.UID)) != 0 {
		t.Fatal("disable through the CLI kept the token", raw)
	}
	if _, err := run("", "", "sa", "delete", "apps", "cli-robot", "--uid", string(sa.UID), "--resource-version", disabled.ResourceVersion); err != nil {
		t.Fatal("serviceaccounts delete", err)
	}

	// Identity providers.
	idp := `apiVersion: kudurru.io/v1alpha1
kind: IdentityProvider
metadata:
  name: cli-partner
spec:
  issuer: https://idp.invalid/cli-partner
  clientID: kudurru
  clientSecretRef:
    env: CLI_PARTNER_SECRET
`
	raw, err = run("", idp, "identityproviders", "create", "-f", "-")
	if err != nil {
		t.Fatal("identityproviders create", err)
	}
	var provider publicv1.IdentityProvider
	decode(raw, &provider)
	if provider.UID == "" || provider.Spec.ClientSecretRef == nil || provider.Spec.ClientSecretRef.Env != "CLI_PARTNER_SECRET" {
		t.Fatal("identityproviders create output", raw)
	}
	if _, err := run("", "", "idp", "get", "cli-partner"); err != nil {
		t.Fatal("identityproviders get", err)
	}
	if _, err := run("", "", "idp", "delete", "cli-partner", "--uid", string(provider.UID), "--resource-version", provider.ResourceVersion); err != nil {
		t.Fatal("identityproviders delete", err)
	}
}
