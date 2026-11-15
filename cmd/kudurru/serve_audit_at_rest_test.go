package main

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/cookiejar"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestRunServeSQLiteAuditAtRestAttribution(t *testing.T) {
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
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
				t.Error("audit mount shutdown failed")
			}
		case <-time.After(15 * time.Second):
			t.Error("audit mount shutdown did not join")
		}
	}
	t.Cleanup(stop)
	deadline := time.Now().Add(20 * time.Second)
	for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK {
		select {
		case err := <-done:
			t.Fatalf("audit mount startup ended: failed=%t", err != nil)
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("audit mount never became ready")
		}
	}
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	base := "http://" + cfg.Listeners.API.Address + api.BasePath
	var login api.SessionInfo
	mountedFactorRequest(t, client, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &login)
	const name = "audit-managed-user"
	var created api.IAMUser
	// One actual current mutation, without a retry transport.
	mountedFactorRequest(t, client, base, http.MethodPost, "/iam/users", login.CSRF, api.CreateLocalUserRequest{Name: name, DisplayName: "Audit account", Password: "audit-fixture-password"}, http.StatusCreated, &created)
	if created.UID == "" {
		t.Fatal("current user mutation did not return exact identity")
	}
	transport.CloseIdleConnections()
	stop()
	db, err := sql.Open("sqlite", filepath.Join(cfg.Node.DataDir, sqlite.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var version int
	var actor, authz, id, ip, ua, method, path string
	var before, after sql.NullString
	err = db.QueryRow(`SELECT chain_version,actor,authz,request_id,client_ip,user_agent,request_method,request_path,before,after FROM audit WHERE action='localuser.create' AND name=?`, name).Scan(&version, &actor, &authz, &id, &ip, &ua, &method, &path, &before, &after)
	if err != nil {
		t.Fatal("current mutation missing persisted audit row")
	}
	if version != 2 || actor != "User:admin" || authz != "local" || id == "" || ip != "127.0.0.1" || ua != "Go-http-client/1.1" || method != http.MethodPost || path != "/api/v1/iam/users" || before.Valid || after.Valid {
		t.Fatal("production audit attribution or private redaction incorrect")
	}
	// Inspect all persisted raw audit documents, including bootstrap and login.
	rows, err := db.Query(`SELECT before,after FROM audit`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var b, a sql.NullString
		if err := rows.Scan(&b, &a); err != nil {
			t.Fatal(err)
		}
		for _, raw := range []string{b.String, a.String} {
			if strings.Contains(raw, "passwordHash") || strings.Contains(raw, "secretHash") || strings.Contains(raw, "audit-fixture-password") || strings.Contains(raw, "mounted-test-password-1") {
				t.Fatal("production raw audit retained credential")
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
