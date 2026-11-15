package main

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// Actual host loading, mounted current-command HTTP, same-disk restarts and the
// actual process logger. Every mutation is one attempt; only readiness polls.
func TestRunServeNodeLogDurableSourcesResetAndFormat(t *testing.T) {
	fixture, _ := mountedTestConfig(t)
	fixture.Auth.IdentityProviders = nil
	raw, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	var host map[string]any
	if err := json.Unmarshal(raw, &host); err != nil {
		t.Fatal(err)
	}
	delete(host, "log") // delegate both leaves to this node's API
	path := filepath.Join(t.TempDir(), "node.json")
	stdout, originalLogger := os.Stdout, slog.Default()
	t.Cleanup(func() { os.Stdout = stdout; slog.SetDefault(originalLogger) })
	for stage := range 5 {
		var env []string
		delete(host, "log")
		wantLevel, wantFormat := "info", "json"
		switch stage {
		case 1:
			wantLevel, wantFormat = "debug", "text"
		case 2:
			host["log"] = map[string]any{"level": "warn", "format": "json"}
			wantLevel = "warn"
		case 3:
			env = []string{"KUDURRU_LOG__LEVEL=error", "KUDURRU_LOG__FORMAT=text"}
			wantLevel, wantFormat = "error", "text"
		}
		raw, err = json.Marshal(host)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(path, env)
		if err != nil {
			t.Fatal("actual node config load failed", err)
		}
		output, err := os.CreateTemp(t.TempDir(), "process-log")
		if err != nil {
			t.Fatal(err)
		}
		os.Stdout = output
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
					t.Error("node settings mount failed to stop", err)
				}
			case <-time.After(15 * time.Second):
				t.Error("node settings mount failed to join")
			}
		}
		t.Cleanup(stop)
		deadline := time.Now().Add(20 * time.Second)
		for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK {
			select {
			case err := <-done:
				t.Fatal("node settings mount ended before readiness", err)
			case <-time.After(20 * time.Millisecond):
			}
			if time.Now().After(deadline) {
				t.Fatal("node settings mount not ready")
			}
		}
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
		base := "http://" + cfg.Listeners.API.Address + api.BasePath
		var login api.SessionInfo
		controlMountedRequest(t, client, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &login)
		var body api.SettingsBody
		controlMountedRequest(t, client, base, http.MethodGet, "/node/config", login.CSRF, nil, http.StatusOK, &body)
		got, ok := body.Settings["log"].(map[string]any)
		if !ok || got["level"] != wantLevel || got["format"] != wantFormat {
			t.Fatal("mounted restart source precedence mismatch", stage)
		}
		if logLevel.Level() != parseLevel(wantLevel) {
			t.Fatal("actual process logger ignored startup overlay", stage)
		}
		patch := func(values map[string]any, status int) {
			body = api.SettingsBody{}
			controlMountedRequest(t, client, base, http.MethodPatch, "/node/config", login.CSRF, map[string]any{"log": values}, status, &body)
			if status == http.StatusOK && (body.Persisted == nil || !*body.Persisted) {
				t.Fatal("mounted command lacked durable receipt", stage)
			}
		}
		switch stage {
		case 0:
			patch(map[string]any{"level": "debug", "format": "text"}, http.StatusOK)
			if logLevel.Level() != slog.LevelDebug {
				t.Fatal("mounted logger failed live level adoption")
			}
			// Original handler remains JSON after a desired format change.
		case 1:
			// A live host-file edit cannot replace this process's original
			// delegation/baseline. A fresh restart must capture it instead.
			host["log"] = map[string]any{"level": "error"}
			changed, err := json.Marshal(host)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, changed, 0o600); err != nil {
				t.Fatal(err)
			}
			patch(map[string]any{"level": "debug"}, http.StatusOK)
		case 2:
			patch(map[string]any{"level": "debug"}, http.StatusConflict)
		case 3:
			patch(map[string]any{"level": "debug"}, http.StatusConflict)
			patch(map[string]any{"level": nil, "format": nil}, http.StatusOK)
			if logLevel.Level() != slog.LevelError {
				t.Fatal("reset overrode original environment")
			}
		}
		slog.Error("f32 logger format witness")
		stop()
		os.Stdout = stdout
		if err := output.Close(); err != nil {
			t.Fatal(err)
		}
		captured, err := os.Open(output.Name())
		if err != nil {
			t.Fatal(err)
		} //nolint:gosec // Test-created temporary log file.
		scan := bufio.NewScanner(captured)
		found := false
		for scan.Scan() {
			line := scan.Text()
			if strings.Contains(line, "f32 logger format witness") {
				found = true
				if (wantFormat == "json") != json.Valid([]byte(line)) {
					t.Fatal("actual process logger format differs from startup owner", stage)
				}
			}
		}
		scanErr := scan.Err()
		_ = captured.Close()
		if scanErr != nil || !found {
			t.Fatal("missing actual logger witness", stage, scanErr)
		}
		local, err := sqlite.ReadNodeLogSettings(t.Context(), filepath.Join(cfg.Node.DataDir, sqlite.FileName), cfg.Node.Name)
		if err != nil {
			t.Fatal(err)
		}
		if stage >= 3 && (local.Level != nil || local.Format != nil) {
			t.Fatal("explicit null retained dormant overlay")
		}
		if stage == 2 && (local.Level == nil || *local.Level != "debug" || local.Format == nil || *local.Format != "text") {
			t.Fatal("host lock erased shadowed overlay")
		}
	}
}
