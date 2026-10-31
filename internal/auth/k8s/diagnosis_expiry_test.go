package k8s_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/audit/auditlog"
	"github.com/robertherbaugh/kudurru/internal/auth/authtest"
	"github.com/robertherbaugh/kudurru/internal/auth/k8s"
	"github.com/robertherbaugh/kudurru/internal/auth/keys"
	"github.com/robertherbaugh/kudurru/internal/auth/stack"
	"github.com/robertherbaugh/kudurru/internal/config"
)

func TestProjectedAuditDiagnosisOriginalExpiry(t *testing.T) {
	for _, path := range []string{"jwks", "review"} {
		for _, boundary := range []string{"valid", "before acquisition", "during acquisition"} {
			t.Run(path+"/"+boundary, func(t *testing.T) {
				f := newProjectedAPI(t)
				f.discoveryFail.Store(path == "review")
				cfg, err := config.Load("", nil)
				if err != nil {
					t.Fatal("load config")
				}
				st := authtest.Memory(t)
				s, err := stack.New(t.Context(), stack.Options{Config: cfg, Store: st, Keys: keys.Keys{Current: make([]byte, 32)}, Kube: f.cfg(), Now: f.now,
					DisableBootstrap: true, Getenv: func(string) string { return "" }, Out: io.Discard, Logger: slog.New(slog.DiscardHandler)})
				if err != nil {
					t.Fatal("mount identity stack")
				}
				calls := 0
				server, err := api.New(api.Options{Store: st, Validator: authtest.Validator(t), Config: cfg, Now: f.now, Authorizer: s.Authorizer(),
					Authenticate: func(next http.Handler) http.Handler {
						return s.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							if boundary == "before acquisition" {
								f.set(f.start.Add(k8s.MaxTokenLifetime))
							}
							next.ServeHTTP(w, r)
						}))
					},
					AuditLogDiagnosis: func(context.Context) auditlog.Diagnostic {
						calls++
						if boundary == "during acquisition" {
							f.set(f.start.Add(k8s.MaxTokenLifetime))
						}
						return auditlog.Diagnostic{State: auditlog.DiagnosticAvailable, Namespace: "private-release", Count: 1, Exact: true}
					}, Logger: slog.New(slog.DiscardHandler)})
				if err != nil {
					t.Fatal("mount diagnosis API")
				}
				t.Cleanup(server.Close)
				r := httptest.NewRequest(http.MethodGet, "/api/v1/info/messages", nil)
				r.Header.Set("Authorization", "Bearer "+f.sign(t, f.claims()))
				w := httptest.NewRecorder()
				server.Handler().ServeHTTP(w, r) // one original signed request
				wantStatus, wantCalls := http.StatusOK, 1
				if boundary == "before acquisition" {
					wantStatus, wantCalls = http.StatusUnauthorized, 0
				}
				if w.Code != wantStatus || calls != wantCalls {
					t.Fatalf("status=%d acquisitions=%d, want %d/%d", w.Code, calls, wantStatus, wantCalls)
				}
				if boundary != "valid" {
					if strings.Contains(w.Body.String(), "private-release") || strings.Contains(w.Body.String(), "AuditLogAvailable") {
						t.Fatal("audit diagnosis outlived the original projected credential")
					}
				} else {
					var out api.List[api.DiagnosisMessage]
					if json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out.Items) != 1 || out.Items[0].Reason != "AuditLogAvailable" {
						t.Fatal("valid projected credential did not receive the audit diagnosis")
					}
				}
			})
		}
	}
}
