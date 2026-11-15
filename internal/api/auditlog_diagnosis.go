package api

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/robertherbaugh/kudurru/internal/audit/auditlog"
	"github.com/robertherbaugh/kudurru/internal/license"
)

// auditLogMessage is a live operational observation, appended only after the
// original canonical diagnosis read succeeds. Audit permission and license
// gates precede acquisition and are checked again before disclosing the result.
func (s *Server) auditLogMessage(ctx context.Context) (DiagnosisMessage, bool) {
	allowed := func() bool {
		return ctx != nil && ctx.Err() == nil && s.allowed(ctx, "audit", "read", clusterScope) && s.requireFeature(license.FeatureAudit, "audit diagnostics") == nil
	}
	if s.opts.AuditLogDiagnosis == nil || !allowed() {
		return DiagnosisMessage{}, false
	}
	d := s.opts.AuditLogDiagnosis(ctx)
	if !allowed() {
		return DiagnosisMessage{}, false
	}
	return auditLogDiagnosisMessage(d), true
}

func auditLogDiagnosisMessage(d auditlog.Diagnostic) DiagnosisMessage {
	m := DiagnosisMessage{Source: "auditlog", Severity: "warning", Reason: "AuditLogIndeterminate",
		Message: "Current AuditLog operational probe: object count is indeterminate; the bounded metadata scan did not establish a complete count.",
		Hint:    "Check AuditLog API availability and batch volume; incomplete scans are not a healthy count. DNS readiness is independent of this check."}
	if d.Namespace == "" || len(validation.IsDNS1123Label(d.Namespace)) != 0 || d.Count < 0 || d.Count > auditlog.RecommendedObjects+1 {
		return m
	}
	scope := fmt.Sprintf("release namespace %q", d.Namespace)
	switch d.State {
	case auditlog.DiagnosticAvailable:
		if !d.Exact || d.Count > auditlog.RecommendedObjects {
			return m
		}
		m.Severity, m.Reason = "info", "AuditLogAvailable"
		m.Message = fmt.Sprintf("Current AuditLog operational probe: the list API is available; a complete metadata-only scan counted %d batch objects in %s (50,000 recommended maximum).", d.Count, scope)
		m.Hint = "This namespace snapshot does not verify shipping or audit integrity. If the CRD was absent at startup, restart replicas to activate cluster audit shipping and reads."
	case auditlog.DiagnosticOverLimit:
		if d.Exact || d.Count != auditlog.RecommendedObjects+1 {
			return m
		}
		m.Reason = "AuditLogObjectCountHigh"
		m.Message = fmt.Sprintf("Current AuditLog operational probe: at least 50,001 batch objects in %s exceed the 50,000 recommendation; counting stopped at that lower bound.", scope)
		m.Hint = "Consider audit.store: database with a central database, or review audit.retention. This is a namespace count of batch objects, not events or a cluster-wide total."
	case auditlog.DiagnosticMissing:
		m.Severity, m.Reason = "error", "AuditLogCRDMissing"
		m.Message = "Current AuditLog operational probe: the AuditLog list API resource was not found; CRD availability and object count cannot be established in " + scope + "."
		m.Hint = "Install or upgrade the kudurru-crds chart. If the CRD was absent at startup, restart replicas to activate cluster audit shipping and reads; DNS keeps working."
	case auditlog.DiagnosticForbidden:
		m.Severity, m.Reason = "error", "AuditLogForbidden"
		m.Message = "Current AuditLog operational probe: the node's AuditLog list request was denied; resource availability and count are unknown in " + scope + "."
		m.Hint = "Check this node's Kubernetes credentials and AuditLog list permission in the release namespace. No audit contents were read."
	case auditlog.DiagnosticUnavailable:
		m.Reason = "AuditLogUnavailable"
		m.Message = "Current AuditLog operational probe: the AuditLog API is unavailable; resource availability and count are unknown in " + scope + "."
		m.Hint = "Check the node's connection to the Kubernetes API. Local audit recording and DNS readiness are independent of this probe."
	}
	return m
}
