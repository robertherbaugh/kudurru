package main

import (
	"context"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	dhcpv1 "github.com/robertherbaugh/kudurru/api/dhcp/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dhcpkube"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
)

// openDHCPKube builds the DHCP runtime of the kube store (D180): DHCPScope
// objects in the store, DHCPLease objects through their own client, the
// kudurru-dhcp Lease and the pod label the DHCP Service selects. Its only
// startup request reads this pod's labels (bounded); a missing CRD shows up
// as retried watch errors and never blocks DNS.
func openDHCPKube(cfg *config.Config, st store.Store, restCfg *rest.Config, logger *slog.Logger) (*dhcpkube.Runtime, error) {
	iface, server, err := cfg.DHCP.Kubernetes()
	if err != nil {
		return nil, err
	}
	scheme := runtime.NewScheme()
	if err := dhcpv1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	hc, err := rest.HTTPClientFor(restCfg)
	if err != nil {
		return nil, err
	}
	mapper, err := apiutil.NewDynamicRESTMapper(restCfg, hc)
	if err != nil {
		return nil, err
	}
	c, err := client.NewWithWatch(restCfg, client.Options{Scheme: scheme, Mapper: mapper})
	if err != nil {
		return nil, err
	}
	ns := kube.PodNamespace()
	pods := &dhcpkube.ClientPods{Client: c, Namespace: ns, Selector: releaseSelector(c, ns, cfg.Node.Name, logger)}
	return dhcpkube.New(dhcpkube.Options{
		Store: st, Leases: &dhcpkube.ClientLeases{Client: c, Namespace: ns}, Pods: pods,
		Elector: st.Elector(dhcpkube.ElectionName), PodName: cfg.Node.Name,
		Interface: iface, ServerIdentifier: server, DataDir: cfg.Node.DataDir, Logger: logger,
	})
}

// releaseSelector returns the release labels of this pod, so the serving
// replica clears the DHCP label only from its own release's pods. Without
// them it clears the label from any pod in the namespace.
func releaseSelector(c client.Client, ns, pod string, logger *slog.Logger) map[string]string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var self corev1.Pod
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: pod}, &self); err != nil {
		logger.Warn("DHCP: this pod's release labels are unavailable; the DHCP label is managed namespace-wide", "component", "dhcp", "error", err)
		return nil
	}
	selector := map[string]string{}
	for _, key := range []string{"app.kubernetes.io/name", "app.kubernetes.io/instance"} {
		if v, ok := self.Labels[key]; ok {
			selector[key] = v
		}
	}
	return selector
}
