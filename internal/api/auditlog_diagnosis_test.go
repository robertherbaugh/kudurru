package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/audit/auditlog"
	"github.com/robertherbaugh/kudurru/internal/license"
)

func TestAuditLogDiagnosisPublicRouterAndDoctor(t *testing.T) {
	for _, captured := range []bool{false, true} {
		for _, tc := range []struct {
			state, reason, severity string
			count                   int
			exact                   bool
		}{
			{auditlog.DiagnosticAvailable, "AuditLogAvailable", "info", 50_000, true},
			{auditlog.DiagnosticOverLimit, "AuditLogObjectCountHigh", "warning", 50_001, false},
			{auditlog.DiagnosticMissing, "AuditLogCRDMissing", "error", 0, false},
			{auditlog.DiagnosticForbidden, "AuditLogForbidden", "error", 0, false},
			{auditlog.DiagnosticUnavailable, "AuditLogUnavailable", "warning", 0, false},
			{auditlog.DiagnosticIndeterminate, "AuditLogIndeterminate", "warning", 500, false},
		} {
			mode := "legacy/"
			if captured {
				mode = "captured/"
			}
			t.Run(mode+tc.state, func(t *testing.T) {
				calls := 0
				h := newHarness(t, func(o *Options) {
					o.AuditLogDiagnosis = func(ctx context.Context) auditlog.Diagnostic {
						calls++
						if ctx.Err() != nil {
							t.Fatal("canceled acquisition")
						}
						return auditlog.Diagnostic{State: tc.state, Namespace: "release", Count: tc.count, Exact: tc.exact}
					}
					if captured {
						o.InfoMessageReads = infoReadFunc(func(context.Context, time.Time, func(string, string, string, string, string, string) error) error {
							return nil
						})
					}
				})
				var out List[DiagnosisMessage]
				h.do(http.MethodGet, "/api/v1/info/messages", "auditor", nil).want(http.StatusOK).json(&out)
				if len(out.Items) != 1 {
					t.Fatalf("missing diagnostic: %+v", out.Items)
				}
				m := out.Items[0]
				if m.Source != "auditlog" || m.Reason != tc.reason || m.Severity != tc.severity || m.Node != "node-a" || m.Object != "" || !strings.HasPrefix(m.Message, "Current AuditLog operational probe: ") || m.Hint == "" {
					t.Fatalf("incorrect public diagnostic: %+v", m)
				}
				if tc.state == auditlog.DiagnosticAvailable && !strings.Contains(m.Message, "complete metadata-only scan counted 50000 batch objects") {
					t.Fatal("complete count not described")
				}
				if tc.state == auditlog.DiagnosticOverLimit && !strings.Contains(m.Message, "at least 50,001") {
					t.Fatal("capped count not described as lower bound")
				}
				var report DoctorReport
				h.do(http.MethodGet, "/api/v1/doctor", "auditor", nil).want(http.StatusOK).json(&report)
				if calls != 2 {
					t.Fatalf("doctor did not reuse one acquisition: %d", calls)
				}
				wantFailures, wantWarnings := 0, 0
				if tc.severity == "error" {
					wantFailures = 1
				}
				if tc.severity == "warning" {
					wantWarnings = 1
				}
				if report.Failures != wantFailures || report.Warnings != wantWarnings {
					t.Fatalf("doctor severity mismatch: %+v", report)
				}
			})
		}
	}
}

func TestAuditLogDiagnosisDeniedDoesNotAcquire(t *testing.T) {
	for _, mode := range []string{"viewer", "operator", "zoneadmin", "anonymous", "unlicensed", "canceled", "original-unavailable"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			h := newHarness(t, func(o *Options) {
				o.AuditLogDiagnosis = func(context.Context) auditlog.Diagnostic {
					calls++
					return auditlog.Diagnostic{State: auditlog.DiagnosticMissing, Namespace: "private-release"}
				}
				if mode == "unlicensed" {
					o.License = licenseManager(t, 0)
				}
				if mode == "original-unavailable" || mode == "canceled" {
					o.InfoMessageReads = infoReadFunc(func(context.Context, time.Time, func(string, string, string, string, string, string) error) error {
						return errors.New("original owner unavailable")
					})
				}
			})
			user, status := mode, http.StatusOK
			switch mode {
			case "anonymous":
				user, status = "", http.StatusUnauthorized
			case "zoneadmin":
				status = http.StatusForbidden
			case "unlicensed":
				user = "auditor"
			case "canceled", "original-unavailable":
				user, status = "auditor", http.StatusServiceUnavailable
			}
			for _, path := range []string{"/api/v1/info/messages", "/api/v1/doctor"} {
				r := h.req(http.MethodGet, path, user, nil)
				if mode == "canceled" {
					ctx, cancel := context.WithCancel(r.Context())
					cancel()
					r = r.WithContext(ctx)
				}
				want := status
				// Doctor preserves its existing operational unavailable warning.
				if path == "/api/v1/doctor" && (mode == "original-unavailable" || mode == "canceled") {
					want = http.StatusOK
				}
				response := h.serve(r).want(want)
				if strings.Contains(response.Body.String(), "private-release") || strings.Contains(response.Body.String(), "AuditLogCRDMissing") {
					t.Fatal("unauthorized audit metadata disclosed")
				}
			}
			if calls != 0 {
				t.Fatalf("denied request acquired audit data %d times", calls)
			}
		})
	}
}

func TestAuditLogDiagnosisFinalPermissionAndLicenseChecks(t *testing.T) {
	for _, gate := range []string{"permission", "license"} {
		t.Run(gate, func(t *testing.T) {
			h := newHarness(t)
			calls := 0
			h.srv.opts.AuditLogDiagnosis = func(context.Context) auditlog.Diagnostic {
				calls++
				if gate == "permission" {
					h.srv.opts.Authorizer = nil
				} else {
					h.srv.opts.License = licenseManager(t, 0)
				}
				return auditlog.Diagnostic{State: auditlog.DiagnosticAvailable, Namespace: "private-release", Count: 1, Exact: true}
			}
			var out List[DiagnosisMessage]
			h.do(http.MethodGet, "/api/v1/info/messages", "auditor", nil).want(http.StatusOK).json(&out)
			if calls != 1 || len(out.Items) != 0 {
				t.Fatal("final permission or license loss disclosed audit diagnosis")
			}
		})
	}
}

func TestAuditLogDiagnosisInvalidResult(t *testing.T) {
	for _, d := range []auditlog.Diagnostic{
		{State: auditlog.DiagnosticAvailable, Namespace: "release", Count: 3, Exact: false},
		{State: auditlog.DiagnosticAvailable, Namespace: "release", Count: 50_001, Exact: true},
		{State: auditlog.DiagnosticOverLimit, Namespace: "release", Count: 1},
		{State: auditlog.DiagnosticAvailable, Namespace: strings.Repeat("x", 256), Exact: true},
	} {
		m := auditLogDiagnosisMessage(d)
		if m.Severity != "warning" || m.Reason != "AuditLogIndeterminate" {
			t.Fatal("invalid/partial result called healthy")
		}
	}
}

func TestAuditLogDiagnosisLicenseDeadlineDuringAcquisition(t *testing.T) {
	for _, expired := range []bool{false, true} {
		name := "grace"
		if expired {
			name = "expired"
		}
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
			clock := now
			pub, priv, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			manager, err := license.New(license.Options{Keyring: license.Keyring{"test": pub}, Now: func() time.Time { return clock }})
			if err != nil {
				t.Fatal(err)
			}
			expires := now.Add(time.Hour)
			raw, err := license.Sign(priv, "test", license.Claims{ID: "diagnosis-test", Licensee: "test", Plan: license.PlanPersonal, Nodes: 3, IssuedAt: now.Add(-time.Hour).Unix(), ExpiresAt: expires.Unix()})
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.Set(raw, "test"); err != nil {
				t.Fatal(err)
			}
			calls := 0
			h := newHarness(t, func(o *Options) {
				o.License = manager
				o.AuditLogDiagnosis = func(context.Context) auditlog.Diagnostic {
					calls++
					clock = expires
					if expired {
						clock = expires.Add(license.Grace)
					}
					return auditlog.Diagnostic{State: auditlog.DiagnosticAvailable, Namespace: "private-release", Count: 1, Exact: true}
				}
			})
			var out List[DiagnosisMessage]
			h.do(http.MethodGet, "/api/v1/info/messages", "auditor", nil).want(http.StatusOK).json(&out)
			want := 1
			if expired {
				want = 0
			}
			if calls != 1 || len(out.Items) != want {
				t.Fatal("post-acquisition license deadline was not honored")
			}
		})
	}
}
