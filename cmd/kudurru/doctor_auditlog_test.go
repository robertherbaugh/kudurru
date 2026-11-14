package main

import (
	"context"
	"net"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/api/apitest"
	"github.com/robertherbaugh/kudurru/internal/audit/auditlog"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

// Run the real doctor command against the production public router; neither
// the CLI fake API nor local Kubernetes logic supplies diagnosis responses.
func TestDoctorAuditLogRealRouter(t *testing.T) {
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, user, state, output, summary string
		count                              int
		exact, failed                      bool
	}{
		{"available", "auditor", auditlog.DiagnosticAvailable, "INFO AuditLogAvailable:", "summary: 0 failed, 0 warnings", 0, true, false},
		{"over-limit", "auditor", auditlog.DiagnosticOverLimit, "WARNING AuditLogObjectCountHigh:", "summary: 0 failed, 1 warnings", 50_001, false, false},
		{"missing", "auditor", auditlog.DiagnosticMissing, "ERROR AuditLogCRDMissing:", "summary: 1 failed, 0 warnings", 0, false, true},
		{"forbidden", "auditor", auditlog.DiagnosticForbidden, "ERROR AuditLogForbidden:", "summary: 1 failed, 0 warnings", 0, false, true},
		{"unavailable", "auditor", auditlog.DiagnosticUnavailable, "WARNING AuditLogUnavailable:", "summary: 0 failed, 1 warnings", 0, false, false},
		{"indeterminate", "auditor", auditlog.DiagnosticIndeterminate, "WARNING AuditLogIndeterminate:", "summary: 0 failed, 1 warnings", 500, false, false},
		{"no-audit-permission", "viewer", auditlog.DiagnosticMissing, "diagnoses: OK none", "summary: 0 failed, 0 warnings", 0, false, false},
		{"no-settings-permission", "zoneadmin", auditlog.DiagnosticMissing, "diagnoses: WARN not permitted for this token", "summary: 0 failed, 1 warnings", 0, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.Load("", nil)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Node.DataDir = t.TempDir()
			calls := 0
			srv, err := api.New(api.Options{Config: cfg, Store: memory.New(), Validator: v,
				Authenticate: apitest.Authenticate, Authorizer: apitest.Authorizer(apitest.StandardBindings()...),
				AuditLogDiagnosis: func(context.Context) auditlog.Diagnostic {
					calls++
					return auditlog.Diagnostic{State: tc.state, Namespace: "private-release", Count: tc.count, Exact: tc.exact}
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(srv.Close)
			httpSrv := httptest.NewServer(srv)
			t.Cleanup(httpSrv.Close)
			h := newHarness(t)
			h.env["KUDURRU_TOKEN"] = strings.TrimPrefix(apitest.Bearer(tc.user), "Bearer ")
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			data := t.TempDir()
			path := doctorConfig(t, data, listener)
			out, _, err := h.run("--config", path, "--server", httpSrv.URL, "doctor", "--skip-ports")
			if (err != nil) != tc.failed || !strings.Contains(out, tc.output) || !strings.Contains(out, tc.summary) {
				t.Fatalf("doctor result: %v\n%s", err, out)
			}
			if strings.HasPrefix(tc.name, "no-") {
				if calls != 0 || strings.Contains(out, "private-release") || strings.Contains(out, "AuditLogCRDMissing") {
					t.Fatal("denied CLI request acquired or disclosed audit metadata")
				}
			} else if calls != 1 {
				t.Fatalf("CLI did not reuse exactly one public diagnosis acquisition: %d", calls)
			}
		})
	}
}
