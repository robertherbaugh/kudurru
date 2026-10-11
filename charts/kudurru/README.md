# kudurru Helm chart

kudurru (koo-DOO-roo) is a Kubernetes-native DNS server: authoritative zones
as CRDs, forwarding with a cache, blocklists, per-client policies, a query
log and a UI. Install the `kudurru-crds` chart first.

```sh
helm install kudurru-crds oci://ghcr.io/robertherbaugh/charts/kudurru-crds
helm install kudurru oci://ghcr.io/robertherbaugh/charts/kudurru -n kudurru-system --create-namespace
```

The guide, the shapes (standalone, hostNetwork, dnsdist edge, MetalLB,
kube-vip, keepalived) and the monitoring values are in
[docs/deploy/helm.md](../../docs/deploy/helm.md); every value is commented in
[values.yaml](values.yaml). `files/` holds generated copies of
`deploy/prometheus/rules.yaml` and `deploy/grafana/kudurru-overview.json`
(`make deploy-assets`).
