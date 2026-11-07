package main

import (
	"context"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	auditv1 "github.com/robertherbaugh/kudurru/api/audit/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/audit/auditkeys"
	"github.com/robertherbaugh/kudurru/internal/audit/auditlog"
	"github.com/robertherbaugh/kudurru/internal/audit/spool"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
)

// auditLogStore is the cluster-wide audit store of mode=kubernetes without
// a central database (audit.store auditlog, D157): this node's spool is
// shipped into AuditLog batches, and every replica reads all of them.
type auditLogStore struct {
	shipper *audit.Shipper
	reader  *auditlog.Reader
	client  client.Client
}

// openAuditLog wires the AuditLog store over the node's spool. It returns
// nil when the AuditLog CRD is not installed (a kudurru-crds chart older
// than the app): the node then shows its own spool and logs why.
// Failed probes (including unavailable or not-yet-authorized API access)
// retain the store so the shipper can retry without blocking DNS startup.
func openAuditLog(ctx context.Context, restCfg *rest.Config, sp *spool.Spool, keys *auditkeys.Source, logger *slog.Logger, startupTimeout time.Duration) (*auditLogStore, error) {
	if keys == nil {
		return nil, auditkeys.ErrUnavailable
	}
	c, err := openAuditLogClient(ctx, restCfg, logger, startupTimeout)
	if err != nil || c == nil {
		return nil, err
	}
	ns := kube.PodNamespace()
	tgt := &auditlog.Target{Client: c, Namespace: ns, Keys: keys}
	sh := &audit.Shipper{Source: sp, Target: tgt, Logger: logger}
	return &auditLogStore{shipper: sh, reader: &auditlog.Reader{Client: c, Namespace: ns, Keys: keys, VerifyPending: sp.Verify, Pending: sh.Pending, PendingSource: sp.SourceID()}, client: c}, nil
}

func openAuditLogClient(ctx context.Context, restCfg *rest.Config, logger *slog.Logger, startupTimeout time.Duration) (client.Client, error) {
	scheme := runtime.NewScheme()
	if err := auditv1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	// The mapper performs discovery without the List context. Bound its
	// transport separately from the audit client's ordinary requests.
	discoveryCfg := rest.CopyConfig(restCfg)
	discoveryCfg.Timeout = startupTimeout
	hc, err := rest.HTTPClientFor(discoveryCfg)
	if err != nil {
		return nil, err
	}
	mapper, err := apiutil.NewDynamicRESTMapper(discoveryCfg, hc)
	if err != nil {
		return nil, err
	}
	c, err := client.New(restCfg, client.Options{Scheme: scheme, Mapper: mapper})
	if err != nil {
		return nil, err
	}
	ns := kube.PodNamespace()
	probe := &metav1.PartialObjectMetadataList{}
	probe.SetGroupVersionKind(auditv1.GroupVersion.WithKind("AuditLogList"))
	pctx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	if err := c.List(pctx, probe, client.InNamespace(ns), client.Limit(1)); meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
		logger.Warn("audit: the AuditLog CRD is not installed; each replica shows only the audit events it recorded. Upgrade the kudurru-crds chart.",
			"component", "audit", "crd", "auditlogs."+auditv1.Group)
		return nil, nil
	} else if err != nil && !kube.IsUnavailable(err) {
		logger.Warn("audit: AuditLog probe failed; continuing, the shipper retries",
			"component", "audit", "error", err)
	}
	return c, nil
}

// auditLogDiagnosis is independent of the startup Reader/shipper decision: a
// CRD missing then can appear later and be diagnosed without discovery caching.
// Transport setup failures remain explicit unavailable observations and never
// add an audit dependency to DNS startup.
func auditLogDiagnosis(cfg *rest.Config) func(context.Context) auditlog.Diagnostic {
	ns := kube.PodNamespace()
	probe, err := auditlog.NewProbe(cfg, ns)
	if err != nil {
		return func(context.Context) auditlog.Diagnostic {
			return auditlog.Diagnostic{State: auditlog.DiagnosticUnavailable, Namespace: ns}
		}
	}
	return probe.Diagnose
}

// prune is the leader's retention job.
func (a *auditLogStore) prune(ctx context.Context, before time.Time) (int64, error) {
	return a.reader.Prune(ctx, before, a.client)
}
