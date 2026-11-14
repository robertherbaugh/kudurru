package main

import (
	"context"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/k8s"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
)

// kubeImpersonation is the kube driver's per-write identity in
// auth.kubernetes.mode impersonate (D386): API writes of kudurru.io objects
// carry the request's authenticated principal in their context, and the
// driver sends them with that principal's Impersonate-User/Group headers.
// Writes without a principal (controllers, DNS UPDATE, integrations) and the
// principals k8s.Impersonation does not impersonate run as kudurru's own
// service account. sar mode (the default) returns nil: nothing is
// impersonated.
func kubeImpersonation(cfg *config.Config) kube.ImpersonateFunc {
	if cfg.Auth.Kubernetes.Mode != config.KubernetesModeImpersonate {
		return nil
	}
	return func(ctx context.Context) (kube.Impersonation, bool) {
		p, ok := auth.PrincipalFrom(ctx)
		if !ok {
			// A principal whose credential bounds ended during the request
			// yields an empty user, which the driver refuses; only a context
			// without any principal writes as kudurru.
			return kube.Impersonation{}, auth.PrincipalPresent(ctx)
		}
		user, groups, ok := k8s.Impersonation(p)
		return kube.Impersonation{User: user, Groups: groups}, ok
	}
}
