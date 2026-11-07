package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/ee/auditstore"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/api/apitest"
	"github.com/robertherbaugh/kudurru/internal/apiclient"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/auth/authtest"
	"github.com/robertherbaugh/kudurru/internal/cli"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

type auditCLIReader struct {
	audit.Memory
	verify func(context.Context) error
}

func (r *auditCLIReader) Verify(ctx context.Context) error { return r.verify(ctx) }

func auditVerifyAPI(t *testing.T, rd audit.Reader, lic api.LicenseSource) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Node.DataDir = t.TempDir()
	srv, err := api.New(api.Options{
		Store: authtest.Memory(t), Validator: authtest.Validator(t), Config: cfg,
		AuditReader: rd, License: lic, NodeName: "audit-cli-test",
		Authenticate: apitest.Authenticate, Authorizer: apitest.Authorizer(apitest.StandardBindings()...),
		Logger: slog.New(slog.DiscardHandler), Now: func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	calls := &atomic.Int32{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/v1/audit/verify" || r.Method != http.MethodGet || r.URL.RawQuery != "" {
			t.Error("audit verification must request the complete retained trail once")
		}
		srv.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts, calls
}

func runAuditVerifyCLI(ctx context.Context, t *testing.T, server, user string, args ...string) (string, error) {
	t.Helper()
	o := &cliOpts{
		getenv: func(string) string { return "" }, now: func() time.Time { return testNow },
		stdin: strings.NewReader(""), creds: &cli.Credentials{Keyring: &memKeyring{}},
	}
	root := newRootWith(o)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(io.Discard)
	base := []string{"--cli-config", filepath.Join(t.TempDir(), "contexts.yaml"), "--server", server}
	if user != "" {
		base = append(base, "--token", apitest.BearerPrefix+user)
	}
	root.SetArgs(append(base, args...))
	err := root.ExecuteContext(ctx)
	return out.String(), err
}

func TestAuditVerifyCLIRealSQLite(t *testing.T) {
	for _, tampered := range []bool{false, true} {
		for _, output := range []string{"table", "json"} {
			t.Run(map[bool]string{true: "tampered", false: "verified"}[tampered]+"/"+output, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "state.db")
				st, err := sqlite.Open(path, sqlite.Options{Validator: authtest.Validator(t)})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = st.Close() })
				if _, err := st.RecordAudit(t.Context(), sqlite.AuditEntry{Action: "dnszones.update"}); err != nil {
					t.Fatal(err)
				}
				if tampered {
					db, err := sql.Open("sqlite", path)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = db.Close() }()
					if _, err := db.ExecContext(t.Context(), "UPDATE audit SET action = 'changed' WHERE id = 1"); err != nil {
						t.Fatal(err)
					}
				}
				ts, calls := auditVerifyAPI(t, auditstore.SQLite{Store: st}, license.Unrestricted())
				out, err := runAuditVerifyCLI(t.Context(), t, ts.URL, "auditor", "audit", "verify", "-o", output)
				if (err != nil) != tampered || calls.Load() != 1 {
					t.Fatalf("verification: err = %v, calls = %d, output = %q", err, calls.Load(), out)
				}
				if output == "json" {
					var got api.AuditVerification
					if err := json.Unmarshal([]byte(out), &got); err != nil {
						t.Fatal(err)
					}
					if got.Verified == tampered || !got.CheckedAt.Equal(testNow) || (got.Reason != "") != tampered {
						t.Fatalf("JSON verification = %+v", got)
					}
				} else if !strings.Contains(out, "Verified:") || !strings.Contains(out, "Checked at:") || !strings.Contains(out, testNow.Format(time.RFC3339)) || strings.Contains(out, "true") == tampered {
					t.Fatalf("human verification = %q", out)
				}
				if tampered && (!strings.Contains(out, "changed records") || err.Error() != "audit verification failed") {
					t.Fatalf("integrity failure must preserve the server's reason: %q, %v", out, err)
				}
			})
		}
	}
}

func TestAuditVerifyCLIRealAPIErrors(t *testing.T) {
	community, err := license.New(license.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, user string
		reader     audit.Reader
		license    api.LicenseSource
		code       int
		key        string
	}{
		{name: "storage unavailable", user: "auditor", reader: &auditCLIReader{verify: func(context.Context) error { return errors.New("private storage details") }}, code: http.StatusServiceUnavailable, key: api.KeyUnavailable},
		{name: "verifier unavailable", user: "auditor", reader: &audit.Memory{}, code: http.StatusServiceUnavailable, key: api.KeyUnavailable},
		{name: "reader unavailable", user: "auditor", code: http.StatusServiceUnavailable, key: api.KeyUnavailable},
		{name: "permission", user: "viewer", code: http.StatusForbidden, key: api.KeyForbidden},
		{name: "license", user: "auditor", license: community, code: http.StatusPaymentRequired, key: api.KeyLicense},
		{name: "anonymous", code: http.StatusUnauthorized, key: api.KeyUnauthenticated},
	} {
		for _, output := range []string{"table", "json"} {
			t.Run(tc.name+"/"+output, func(t *testing.T) {
				lic := tc.license
				if lic == nil {
					lic = license.Unrestricted()
				}
				ts, calls := auditVerifyAPI(t, tc.reader, lic)
				out, err := runAuditVerifyCLI(t.Context(), t, ts.URL, tc.user, "audit", "verify", "-o", output)
				if !apiclient.IsStatus(err, tc.code) || !apiclient.IsKey(err, tc.key) || out != "" || calls.Load() != 1 {
					t.Fatalf("API error = %v; output = %q, calls = %d", err, out, calls.Load())
				}
				if strings.Contains(err.Error(), "private") {
					t.Fatal("backend details exposed by CLI")
				}
			})
		}
	}
}

func TestAuditVerifyCLIGatesAndCache(t *testing.T) {
	var scans atomic.Int32
	rd := &auditCLIReader{verify: func(context.Context) error { scans.Add(1); return nil }}
	ts, _ := auditVerifyAPI(t, rd, license.Unrestricted())
	// Both authorized built-in roles reach the same cached server verification.
	for _, user := range []string{"auditor", "admin"} {
		if out, err := runAuditVerifyCLI(t.Context(), t, ts.URL, user, "audit", "verify", "-o", "json"); err != nil || !strings.Contains(out, `"verified": true`) {
			t.Fatalf("authorized verification: %v, %q", err, out)
		}
	}
	if scans.Load() != 1 {
		t.Fatalf("CLI bypassed the server cache: %d scans", scans.Load())
	}
	community, err := license.New(license.Options{})
	if err != nil {
		t.Fatal(err)
	}
	denied, _ := auditVerifyAPI(t, rd, community)
	for _, tc := range []struct {
		server, user string
		code         int
	}{
		{ts.URL, "viewer", http.StatusForbidden},
		{denied.URL, "auditor", http.StatusPaymentRequired},
	} {
		if _, err := runAuditVerifyCLI(t.Context(), t, tc.server, tc.user, "audit", "verify"); !apiclient.IsStatus(err, tc.code) {
			t.Fatalf("gate error = %v", err)
		}
	}
	if scans.Load() != 1 {
		t.Fatal("denied command started a verification scan")
	}
}

func TestAuditVerifyCLIContextAndTimeout(t *testing.T) {
	for _, mode := range []string{"cancellation", "context deadline", "request timeout"} {
		t.Run(mode, func(t *testing.T) {
			started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			rd := &auditCLIReader{verify: func(ctx context.Context) error {
				close(started)
				defer close(finished)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-release:
					return nil
				}
			}}
			ts, _ := auditVerifyAPI(t, rd, license.Unrestricted())
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			args := []string{"audit", "verify", "-o", "json"}
			switch mode {
			case "context deadline":
				ctx, cancel = context.WithTimeout(ctx, 500*time.Millisecond)
				defer cancel()
			case "request timeout":
				args = append(args, "--request-timeout", "500ms")
			}
			type result struct {
				out string
				err error
			}
			done := make(chan result, 1)
			go func() {
				out, err := runAuditVerifyCLI(ctx, t, ts.URL, "auditor", args...)
				done <- result{out, err}
			}()
			// Release the server's intentionally independent shared scan on failure too.
			defer func() {
				close(release)
				select {
				case <-finished:
				case <-time.After(3 * time.Second):
					t.Error("shared verification scan did not finish after release")
				}
			}()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("verification request did not reach real API")
			}
			want := context.DeadlineExceeded
			if mode == "cancellation" {
				cancel()
				want = context.Canceled
			}
			select {
			case got := <-done:
				if !errors.Is(got.err, want) || got.out != "" {
					t.Fatalf("command did not honor context/timeout: %v, %q", got.err, got.out)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("command waited for the server's shared verification scan")
			}
		})
	}
}

func TestAuditVerifyCLIArgumentValidation(t *testing.T) {
	ts, calls := auditVerifyAPI(t, &audit.Memory{}, license.Unrestricted())
	for _, args := range [][]string{
		{"audit", "verify", "unexpected"},
		{"audit", "verify", "--since", "24h"},
		{"audit", "verify", "-o", "invalid"},
	} {
		if _, err := runAuditVerifyCLI(t.Context(), t, ts.URL, "auditor", args...); err == nil {
			t.Fatalf("invalid invocation accepted: %v", args)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid invocation contacted the API")
	}
}
